// Package testbench stores testbenches (a build plus the runtime settings
// to run on it, with optional per-instance overrides and duration) and runs
// their lifecycle: start, restart, stop and auto-stop, one at a time
// (docs/UI-ARCHITECTURE.md, Phase 6).
package testbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
)

// maxDelayMS matches the admin API's limit on PUT /admin/v1/behaviour.
const maxDelayMS = 3_600_000

// Testbench is one testbench file.
type Testbench struct {
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Version     int                 `json:"version"`
	Devices     Devices             `json:"devices"`
	Runtime     Runtime             `json:"runtime"`
	Overrides   map[string]Override `json:"overrides,omitempty"` // by instance IP
	Duration    string              `json:"duration,omitempty"`  // optional auto-stop, e.g. "2h"

	// Build is the Phase 6 format, read only to migrate it on load.
	Build *legacyBuild `json:"build,omitempty"`

	Slug    string `json:"-"` // file name without .json
	Shipped bool   `json:"-"` // embedded and read-only
	Problem string `json:"-"` // why it fails validation on load; it cannot start until fixed
}

// Runtime is the fleet-wide runtime config, in the admin API's shapes.
type Runtime struct {
	Faults        map[string]float64 `json:"faults"`
	Profiles      Profiles           `json:"profiles"`
	DelayMS       DelayMS            `json:"delay_ms"`
	MaxSessions   int64              `json:"max_sessions"`
	ActiveDevices Active             `json:"active_devices"`
}

// Profiles is the device profile seed and rules.
type Profiles struct {
	Seed  int64    `json:"seed"`
	Rules []string `json:"rules"`
}

// DelayMS is the response delay window.
type DelayMS struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Override replaces any runtime value for one instance. Nil fields come
// from Runtime.
type Override struct {
	Faults        map[string]float64 `json:"faults,omitempty"`
	Profiles      *Profiles          `json:"profiles,omitempty"`
	DelayMS       *DelayMS           `json:"delay_ms,omitempty"`
	MaxSessions   *int64             `json:"max_sessions,omitempty"`
	ActiveDevices *Active            `json:"active_devices,omitempty"`
}

// Active is an active-device count, or every device ("all"). In Runtime it
// is a fleet total, split across instances as the Fleet screen does; in an
// Override it is that instance's own count.
type Active struct {
	All bool
	N   int
}

func (a Active) String() string {
	if a.All {
		return "all"
	}
	return strconv.Itoa(a.N)
}

// ParseActive reads "all" or a count.
func ParseActive(s string) (Active, error) {
	s = strings.TrimSpace(s)
	if s == "all" || s == "" {
		return Active{All: true}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return Active{}, fmt.Errorf("active devices %q: want \"all\" or a count >= 0", s)
	}
	return Active{N: n}, nil
}

func (a Active) MarshalJSON() ([]byte, error) {
	if a.All {
		return []byte(`"all"`), nil
	}
	return json.Marshal(a.N)
}

func (a *Active) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		if s != "all" {
			return fmt.Errorf("active_devices %q: want \"all\" or a number", s)
		}
		*a = Active{All: true}
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil || n < 0 {
		return fmt.Errorf("active_devices %s: want \"all\" or a number >= 0", b)
	}
	*a = Active{N: n}
	return nil
}

// DurationValue is the parsed Duration; zero when unset.
func (tb Testbench) DurationValue() time.Duration {
	d, _ := time.ParseDuration(tb.Duration)
	return d
}

// Validate is the check on save and import: every value valid on its own,
// as the catalogue, a Build job and the admin API check them. Whether the
// testbench fits this host is Fit's job; one that does not can still be
// saved.
func Validate(tb Testbench, cat *catalogue.Catalogue) error {
	var errs []error
	if n := strings.TrimSpace(tb.Name); n == "" || len(n) > 64 {
		errs = append(errs, errors.New("name must be 1 to 64 characters"))
	}
	if len(tb.Description) > 500 {
		errs = append(errs, errors.New("description must be at most 500 characters"))
	}
	if tb.Version < 0 {
		errs = append(errs, errors.New("version must be >= 0"))
	}
	if tb.Build != nil {
		errs = append(errs, errors.New("the old build section is still present; it is migrated on load"))
	}
	if err := validateDevices(tb.Devices, cat); err != nil {
		errs = append(errs, err)
	}
	if err := validateRuntime("runtime", tb.Runtime.Faults, &tb.Runtime.Profiles, &tb.Runtime.DelayMS, &tb.Runtime.MaxSessions); err != nil {
		errs = append(errs, err)
	}
	if a := tb.Runtime.ActiveDevices; !a.All && tb.Devices.Source != SourceScenario && a.N > tb.Devices.Total() {
		errs = append(errs, fmt.Errorf("runtime.active_devices %d is more than the %d devices built", a.N, tb.Devices.Total()))
	}
	for _, ip := range slices.Sorted(maps.Keys(tb.Overrides)) {
		if _, err := netip.ParseAddr(ip); err != nil {
			errs = append(errs, fmt.Errorf("overrides: %q is not an instance IP", ip))
			continue
		}
		o := tb.Overrides[ip]
		if err := validateRuntime("overrides."+ip, o.Faults, o.Profiles, o.DelayMS, o.MaxSessions); err != nil {
			errs = append(errs, err)
		}
	}
	if tb.Duration != "" {
		if d, err := time.ParseDuration(tb.Duration); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("duration %q: want a positive Go duration such as 90m or 2h", tb.Duration))
		}
	}
	return errors.Join(errs...)
}

// validateRuntime checks the values given (nil and empty are "not set"),
// as the admin API does.
func validateRuntime(where string, faults map[string]float64, p *Profiles, d *DelayMS, maxSessions *int64) error {
	var errs []error
	if faults != nil {
		if _, err := fault.NewSetFromRates(faults); err != nil {
			errs = append(errs, fmt.Errorf("%s.faults: %w", where, err))
		}
	}
	if p != nil {
		for _, r := range p.Rules {
			if strings.Contains(r, ",") || strings.TrimSpace(r) == "" {
				errs = append(errs, fmt.Errorf("%s.profiles: rule %q: want one profile@percent item", where, r))
			}
		}
		if _, err := profile.Parse(strings.Join(p.Rules, ",")); err != nil {
			errs = append(errs, fmt.Errorf("%s.profiles: %w", where, err))
		}
	}
	if d != nil && (d.Min < 0 || d.Min > d.Max || d.Max > maxDelayMS) {
		errs = append(errs, fmt.Errorf("%s.delay_ms: want 0 <= min <= max <= %d", where, maxDelayMS))
	}
	if maxSessions != nil && *maxSessions < 1 {
		errs = append(errs, fmt.Errorf("%s.max_sessions must be >= 1", where))
	}
	return errors.Join(errs...)
}

// Fit lists why tb cannot start on the host with layout l (or layoutErr,
// when the host has no usable layout). Empty means it fits.
func Fit(tb Testbench, l jobs.Layout, layoutErr error) []string {
	if layoutErr != nil {
		return []string{layoutErr.Error()}
	}
	var out []string
	if tb.Devices.Source == SourceScenario {
		// A scenario need not fill every instance; the rest are not used.
		if n, capacity := tb.Devices.Total(), l.IPCount*l.DevicesPerIP; n > capacity {
			out = append(out, fmt.Sprintf("%d devices, but this host's %d instances have %d ports", n, l.IPCount, capacity))
		}
	} else if err := l.CheckCount(tb.Devices.Total()); err != nil {
		out = append(out, err.Error())
	}
	devices := instanceDevices(tb.Devices.Total(), l)
	for _, ip := range slices.Sorted(maps.Keys(tb.Overrides)) {
		name, ok := instanceFor(ip, l)
		if !ok {
			out = append(out, fmt.Sprintf("override for %s: no instance on this host has that IP", ip))
			continue
		}
		if devices[name] == 0 {
			out = append(out, fmt.Sprintf("override for %s: that instance is not used by this testbench", ip))
			continue
		}
		if a := tb.Overrides[ip].ActiveDevices; a != nil && !a.All && a.N > devices[name] {
			out = append(out, fmt.Sprintf("override for %s: %d active devices, but the instance gets %d", ip, a.N, devices[name]))
		}
	}
	return out
}

// instanceFor finds the instance whose IP (its name, as the unit instance)
// is ip.
func instanceFor(ip string, l jobs.Layout) (string, bool) {
	want, err := netip.ParseAddr(ip)
	if err != nil {
		return "", false
	}
	for _, n := range l.Instances {
		if a, err := netip.ParseAddr(n); err == nil && a == want {
			return n, true
		}
	}
	return "", false
}

// instanceDevices is how many devices each instance gets from a build of
// count devices: the generator fills IPs in order, DevicesPerIP each, and
// a scenario install places its devices the same way.
func instanceDevices(count int, l jobs.Layout) map[string]int {
	out := make(map[string]int, len(l.Instances))
	left := count
	for _, n := range l.Instances {
		d := min(max(left, 0), l.DevicesPerIP)
		out[n] = d
		left -= d
	}
	return out
}

// Resolve is each used instance's runtime config for tb on layout l: the
// fleet values, the active total split by device count, then the
// overrides. An instance that gets no devices (a scenario that needs fewer
// instances than the host has) is not used and not in the result.
func Resolve(tb Testbench, l jobs.Layout) (map[string]fleet.Desired, error) {
	devices := instanceDevices(tb.Devices.Total(), l)
	weights := make([]int, len(l.Instances))
	for i, n := range l.Instances {
		weights[i] = devices[n]
	}
	rt := tb.Runtime
	var split []int
	if !rt.ActiveDevices.All {
		var err error
		if split, err = fleet.SplitActive(rt.ActiveDevices.N, weights); err != nil {
			return nil, err
		}
	}
	out := make(map[string]fleet.Desired, len(l.Instances))
	for i, n := range l.Instances {
		d := fleet.Desired{
			Faults: nonNil(rt.Faults), ProfileSeed: rt.Profiles.Seed, ProfileRules: rules(rt.Profiles.Rules),
			DelayMinMS: rt.DelayMS.Min, DelayMaxMS: rt.DelayMS.Max, MaxSessions: rt.MaxSessions,
			ActiveDevices: weights[i],
		}
		if split != nil {
			d.ActiveDevices = split[i]
		}
		if weights[i] > 0 {
			out[n] = d
		}
	}
	for ip, o := range tb.Overrides {
		n, ok := instanceFor(ip, l)
		if !ok {
			return nil, fmt.Errorf("override for %s: no instance on this host has that IP", ip)
		}
		if _, used := out[n]; !used {
			return nil, fmt.Errorf("override for %s: that instance is not used by this testbench", ip)
		}
		d := out[n]
		if o.Faults != nil {
			d.Faults = o.Faults
		}
		if o.Profiles != nil {
			d.ProfileSeed, d.ProfileRules = o.Profiles.Seed, rules(o.Profiles.Rules)
		}
		if o.DelayMS != nil {
			d.DelayMinMS, d.DelayMaxMS = o.DelayMS.Min, o.DelayMS.Max
		}
		if o.MaxSessions != nil {
			d.MaxSessions = *o.MaxSessions
		}
		if o.ActiveDevices != nil {
			d.ActiveDevices = devices[n]
			if !o.ActiveDevices.All {
				d.ActiveDevices = min(o.ActiveDevices.N, devices[n])
			}
		}
		out[n] = d
	}
	return out, nil
}

func nonNil(m map[string]float64) map[string]float64 {
	if m == nil {
		return map[string]float64{}
	}
	return m
}

func rules(r []string) []string {
	if r == nil {
		return []string{}
	}
	return r
}
