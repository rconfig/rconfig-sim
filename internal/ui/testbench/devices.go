package testbench

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
)

// Device sources (D12).
const (
	SourceGenerated = "generated"
	SourceScenario  = "scenario" // a scenario package (Phase 9)
)

// SSH login modes for a fleet with optical devices: rcfg-sim's --ssh-auth
// values (README, "SSH-layer auth vs in-band TL1 auth").
const (
	SSHAuthDriver   = "driver"   // TL1 devices log in inside TL1 only, like real 6500s; IP devices still ask
	SSHAuthPassword = "password" // an SSH password first, then the TL1 login (today's default)
	SSHAuthNone     = "none"     // no SSH password at all: optical-only fleets
)

// Devices is a testbench's device source (Phase 8 item 3): a generated
// fleet or a scenario package.
type Devices struct {
	Source    string       `json:"source"`
	Generated *Generated   `json:"generated,omitempty"`
	Scenario  *ScenarioRef `json:"scenario,omitempty"`
}

// ScenarioRef names a scenario package and the version the testbench was
// made for. The rest is filled from the library when the testbench loads.
type ScenarioRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`

	Name    string `json:"-"` // the package's name
	Devices int    `json:"-"` // its device count
	SSHAuth string `json:"-"` // its ssh_auth
}

// Generated is a generated fleet: per-family model counts and options.
type Generated struct {
	Seed     int64                     `json:"seed"`
	Families map[string]*FamilyDevices `json:"families"`
}

// FamilyDevices is one family's part of a generated fleet. A family that
// is absent is not in the fleet.
type FamilyDevices struct {
	Models map[string]int `json:"models"`
	// SSHAuth is the instances' --ssh-auth; only the optical family
	// carries it, because that is where the choice matters.
	SSHAuth string `json:"ssh_auth,omitempty"`
	// Options are the catalogue's model options for this family, by name
	// (for example rne_dual_home_pct).
	Options map[string]int `json:"options,omitempty"`
}

// legacyBuild is the Phase 6 "build" section, read only to migrate old
// files.
type legacyBuild struct {
	Devices      int               `json:"devices"`
	Distribution string            `json:"distribution"`
	Seed         int64             `json:"seed"`
	Options      map[string]string `json:"options,omitempty"`
}

// Family returns a family's part of the fleet, or nil.
func (d Devices) Family(id string) *FamilyDevices {
	if d.Generated == nil {
		return nil
	}
	return d.Generated.Families[id]
}

// Counts is every model's device count, all families together.
func (d Devices) Counts() map[string]int {
	out := map[string]int{}
	if d.Generated == nil {
		return out
	}
	for _, f := range d.Generated.Families {
		if f == nil {
			continue
		}
		for m, n := range f.Models {
			if n > 0 {
				out[m] += n
			}
		}
	}
	return out
}

// Total is the number of devices.
func (d Devices) Total() int {
	if d.Source == SourceScenario && d.Scenario != nil {
		return d.Scenario.Devices
	}
	n := 0
	for _, c := range d.Counts() {
		n += c
	}
	return n
}

// FamilyIDs lists the families with devices, sorted.
func (d Devices) FamilyIDs() []string {
	var out []string
	if d.Generated == nil {
		return out
	}
	for id, f := range d.Generated.Families {
		if f != nil && familyTotal(f) > 0 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func familyTotal(f *FamilyDevices) int {
	n := 0
	for _, c := range f.Models {
		n += c
	}
	return n
}

// SSHAuth is the --ssh-auth the instances run with: the scenario's, the
// optical family's choice, or password (the default) without one.
func (d Devices) SSHAuth() string {
	if d.Source == SourceScenario && d.Scenario != nil && d.Scenario.SSHAuth != "" {
		return d.Scenario.SSHAuth
	}
	if f := d.Family("optical"); f != nil && f.SSHAuth != "" {
		return f.SSHAuth
	}
	return SSHAuthPassword
}

// Seed is the generator seed.
func (d Devices) Seed() int64 {
	if d.Generated == nil {
		return 0
	}
	return d.Generated.Seed
}

// Kind is how the testbench list groups it: ip, optical, mix, scenario, or
// "" when it has no devices.
func (d Devices) Kind() string {
	if d.Source == SourceScenario {
		return SourceScenario
	}
	fams := d.FamilyIDs()
	switch {
	case len(fams) > 1:
		return "mix"
	case len(fams) == 1:
		return fams[0]
	}
	return ""
}

// Request is the build this fleet needs: exact counts, the seed and the
// generator flags of the family options.
func (d Devices) Request(cat *catalogue.Catalogue) jobs.Request {
	opts := map[string]string{}
	if d.Generated != nil {
		for _, fid := range slices.Sorted(maps.Keys(d.Generated.Families)) {
			f := d.Generated.Families[fid]
			if f == nil {
				continue
			}
			for name, v := range f.Options {
				for _, o := range cat.FamilyOptions(fid) {
					if o.Name == name {
						opts[o.FlagName()] = strconv.Itoa(v)
					}
				}
			}
		}
	}
	return jobs.Request{Counts: d.Counts(), Seed: d.Seed(), Options: opts}
}

// validateDevices checks the device source against the catalogue: known
// families and models, each model in its own family, at least one device,
// ssh_auth only on optical and never none with IP devices, options known
// and in range.
func validateDevices(d Devices, cat *catalogue.Catalogue) error {
	if d.Source == SourceScenario {
		return validateScenarioRef(d)
	}
	if d.Source != SourceGenerated {
		return fmt.Errorf("devices.source %q: want %q or %q", d.Source, SourceGenerated, SourceScenario)
	}
	if d.Scenario != nil {
		return errors.New("devices.scenario is only for source scenario")
	}
	g := d.Generated
	if g == nil {
		return errors.New("devices.generated is missing")
	}
	var errs []error
	for _, fid := range slices.Sorted(maps.Keys(g.Families)) {
		f := g.Families[fid]
		where := "devices.generated.families." + fid
		if _, ok := cat.Family(fid); !ok {
			errs = append(errs, fmt.Errorf("%s: unknown family", where))
			continue
		}
		if f == nil {
			continue
		}
		for _, m := range slices.Sorted(maps.Keys(f.Models)) {
			model, ok := cat.Model(m)
			switch {
			case !ok:
				errs = append(errs, fmt.Errorf("%s: unknown model %q", where, m))
			case model.Family != fid:
				errs = append(errs, fmt.Errorf("%s: model %s belongs to the %s family", where, m, model.Family))
			case f.Models[m] < 0:
				errs = append(errs, fmt.Errorf("%s: model %s: count must be >= 0", where, m))
			}
		}
		if f.SSHAuth != "" {
			if fid != "optical" {
				errs = append(errs, fmt.Errorf("%s.ssh_auth: only the optical family chooses the SSH login", where))
			} else if !slices.Contains([]string{SSHAuthDriver, SSHAuthPassword, SSHAuthNone}, f.SSHAuth) {
				errs = append(errs, fmt.Errorf("%s.ssh_auth %q: want driver, password or none", where, f.SSHAuth))
			}
		}
		opts := cat.FamilyOptions(fid)
		for _, name := range slices.Sorted(maps.Keys(f.Options)) {
			i := slices.IndexFunc(opts, func(o catalogue.Option) bool { return o.Name == name })
			if i < 0 {
				errs = append(errs, fmt.Errorf("%s.options: %q is not an option of this family's models", where, name))
				continue
			}
			if v := f.Options[name]; v < opts[i].Min || v > opts[i].Max {
				errs = append(errs, fmt.Errorf("%s.options.%s: %d is outside %d to %d", where, name, v, opts[i].Min, opts[i].Max))
			}
		}
	}
	if d.Total() < 1 {
		errs = append(errs, errors.New("devices: at least one family needs devices"))
	}
	if d.SSHAuth() == SSHAuthNone && d.Family("ip") != nil && familyTotal(d.Family("ip")) > 0 {
		errs = append(errs, errors.New("devices: SSH login \"none\" is for optical-only fleets; it would also remove the password from the IP devices"))
	}
	return errors.Join(errs...)
}

// validateScenarioRef checks a scenario source's shape. Whether the
// package exists and is valid is the library's question (Store.check).
func validateScenarioRef(d Devices) error {
	var errs []error
	if d.Generated != nil {
		errs = append(errs, errors.New("devices.generated is only for source generated"))
	}
	switch r := d.Scenario; {
	case r == nil:
		errs = append(errs, errors.New("devices.scenario is missing"))
	case r.ID == "":
		errs = append(errs, errors.New("devices.scenario.id is missing"))
	case r.Version < 1:
		errs = append(errs, errors.New("devices.scenario.version must be 1 or more"))
	}
	return errors.Join(errs...)
}

// migrate turns a Phase 6 build section into a generated device source:
// the distribution's percentages become counts of the stated total (the
// generator's rounding), each model in its catalogue family, and known
// generator options become family options.
func migrate(b legacyBuild, cat *catalogue.Catalogue) (Devices, error) {
	weights, err := cat.ParseDistribution(b.Distribution)
	if err != nil {
		return Devices{}, fmt.Errorf("build.distribution: %w", err)
	}
	if b.Devices < 1 {
		return Devices{}, errors.New("build.devices must be >= 1")
	}
	g := &Generated{Seed: b.Seed, Families: map[string]*FamilyDevices{}}
	for m, n := range cat.Counts(b.Devices, weights) {
		fid := cat.FamilyOfModel(m)
		if g.Families[fid] == nil {
			g.Families[fid] = &FamilyDevices{Models: map[string]int{}}
		}
		g.Families[fid].Models[m] = n
	}
	// A bad option does not lose the rest: the caller keeps what migrated
	// and shows the error, so the operator can fix it in the editor.
	var errs []error
	for _, flagName := range slices.Sorted(maps.Keys(b.Options)) {
		v := b.Options[flagName]
		placed := false
		for fid, f := range g.Families {
			for _, o := range cat.FamilyOptions(fid) {
				if o.FlagName() == flagName {
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil {
						errs = append(errs, fmt.Errorf("build.options.%s: %q is not a whole number", flagName, v))
						placed = true
						continue
					}
					if f.Options == nil {
						f.Options = map[string]int{}
					}
					f.Options[o.Name] = n
					placed = true
				}
			}
		}
		if !placed {
			errs = append(errs, fmt.Errorf("build.options: %q is not an option of the fleet's models", flagName))
		}
	}
	return Devices{Source: SourceGenerated, Generated: g}, errors.Join(errs...)
}
