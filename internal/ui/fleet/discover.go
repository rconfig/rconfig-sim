// Package fleet finds the rcfg-sim instances on this host from their env
// files, polls each one (/healthz, /admin/v1/state, /metrics), detects drift
// between an instance's live config and its env file, and fans fleet-wide
// changes out to every instance, writing each accepted change back to that
// instance's env file.
package fleet

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
)

// UnitDefaults are the Environment= defaults in deploy/systemd/rcfg-sim@.service
// for the keys the controller reads. A key an env file leaves out takes this
// value, exactly as the unit would give the instance. LISTEN_IP defaults to
// the instance name. A test keeps this in step with the unit file.
var UnitDefaults = map[string]string{
	"PORT_START":              "10000",
	"PORT_COUNT":              "2500",
	"MANIFEST":                "/opt/rcfg-sim/manifest.csv",
	"METRICS_ADDR":            "0.0.0.0:9100",
	"RESPONSE_DELAY_MS_MIN":   "50",
	"RESPONSE_DELAY_MS_MAX":   "500",
	"MAX_CONCURRENT_SESSIONS": "5000",
	"ACTIVE_DEVICES":          "all",
	"FAULT_RATE":              "0.0",
	"FAULT_TYPES":             "",
	"FAULT_RATES":             "",
	"DEVICE_PROFILES":         "",
	"PROFILE_SEED":            "42",
	"ADMIN_ADDR":              "",
	"SSH_AUTH":                "password",
	"COMMANDS_ROOT":           "",
	"PASSWORD":                "admin",
	"ENABLE_PASSWORD":         "enable123",
	"ALLOW_EMPTY":             "false",
}

// Instance is one rcfg-sim@<name>.service, as its env file describes it.
type Instance struct {
	Name        string // the unit instance: the env file name without .env
	EnvPath     string
	ListenIP    string
	PortStart   int
	PortCount   int
	MetricsAddr string // as configured: /healthz and /metrics
	DialAddr    string // host:port the controller dials for them (127.0.0.1 for a wildcard bind)
	AdminAddr   string // ADMIN_ADDR: the admin API's loopback listener; "" when off
	Manifest    string
	Env         map[string]string // effective values: the file over UnitDefaults
	Err         error             // the env file could not be read or has bad values
	PortClash   []string          // instances whose metrics or admin listener shares a port with this one's
}

// Discover reads every *.env in dir (*.env.sample does not match) and
// returns the instances sorted by IP, then name.
func Discover(dir string) ([]Instance, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.env"))
	if err != nil {
		return nil, err
	}
	out := make([]Instance, 0, len(paths))
	for _, p := range paths {
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, load(p))
	}
	slices.SortFunc(out, func(a, b Instance) int {
		ai, aerr := netip.ParseAddr(a.ListenIP)
		bi, berr := netip.ParseAddr(b.ListenIP)
		if aerr == nil && berr == nil {
			if c := ai.Compare(bi); c != 0 {
				return c
			}
		}
		return strings.Compare(a.Name, b.Name)
	})
	markClashes(out)
	return out, nil
}

func load(path string) Instance {
	name := strings.TrimSuffix(filepath.Base(path), ".env")
	inst := Instance{Name: name, EnvPath: path, Env: map[string]string{}}
	for k, v := range UnitDefaults {
		inst.Env[k] = v
	}
	inst.Env["LISTEN_IP"] = name
	f, err := envfile.Read(path)
	if err != nil {
		inst.Err = err
	} else {
		for k, v := range f.Values {
			inst.Env[k] = v
		}
	}
	inst.ListenIP = inst.Env["LISTEN_IP"]
	inst.Manifest = inst.Env["MANIFEST"]
	inst.MetricsAddr = inst.Env["METRICS_ADDR"]
	inst.AdminAddr = strings.TrimSpace(inst.Env["ADMIN_ADDR"])
	var errs []string
	if inst.PortStart, err = strconv.Atoi(inst.Env["PORT_START"]); err != nil {
		errs = append(errs, "PORT_START "+strconv.Quote(inst.Env["PORT_START"]))
	}
	if inst.PortCount, err = strconv.Atoi(inst.Env["PORT_COUNT"]); err != nil || inst.PortCount < 1 {
		errs = append(errs, "PORT_COUNT "+strconv.Quote(inst.Env["PORT_COUNT"]))
	}
	if inst.DialAddr, err = dialAddr(inst.MetricsAddr); err != nil {
		errs = append(errs, "METRICS_ADDR "+strconv.Quote(inst.MetricsAddr))
	}
	if inst.AdminAddr != "" && !loopbackAddr(inst.AdminAddr) {
		// rcfg-sim refuses to start with it, so say so on the card.
		errs = append(errs, "ADMIN_ADDR "+strconv.Quote(inst.AdminAddr)+" (must be a loopback host:port)")
	}
	if len(errs) > 0 && inst.Err == nil {
		inst.Err = fmt.Errorf("bad value: %s", strings.Join(errs, ", "))
	}
	return inst
}

// dialAddr turns a listen address into one to dial: a wildcard or empty host
// becomes 127.0.0.1.
func dialAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("metrics address %q: want host:port", listen)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// markClashes records instances whose metrics listeners would collide: the
// same port on the same host, or the same port where either binds all
// addresses.
func markClashes(insts []Instance) {
	type bind struct {
		inst       int
		host, port string
		wild       bool
	}
	var binds []bind
	for i, in := range insts {
		for _, addr := range []string{in.MetricsAddr, in.AdminAddr} {
			host, port, err := net.SplitHostPort(addr)
			if err != nil || port == "" {
				continue
			}
			binds = append(binds, bind{i, host, port, host == "" || host == "0.0.0.0" || host == "::"})
		}
	}
	for a := range binds {
		for b := range binds {
			x, y := binds[a], binds[b]
			if a == b || x.port != y.port || !(x.wild || y.wild || x.host == y.host) {
				continue
			}
			name := insts[y.inst].Name
			if !slices.Contains(insts[x.inst].PortClash, name) {
				insts[x.inst].PortClash = append(insts[x.inst].PortClash, name)
			}
		}
	}
}

// loopbackAddr reports whether addr is host:port on a loopback address, as
// rcfg-sim requires of ADMIN_ADDR.
func loopbackAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Desired is the config an instance's env file asks for, in the shape the
// admin API reports, so the two can be compared.
type Desired struct {
	Faults        map[string]float64
	ProfileRules  []string
	ProfileSeed   int64
	DelayMinMS    int64
	DelayMaxMS    int64
	MaxSessions   int64
	ActiveDevices int
}

// DesiredFromEnv reads the controller-owned settings from an instance's
// effective env, the same way rcfg-sim reads its flags.
func DesiredFromEnv(inst Instance) (Desired, error) {
	e := inst.Env
	var d Desired
	var set *fault.Set
	var err error
	if strings.TrimSpace(e["FAULT_RATES"]) != "" {
		set, err = fault.ParseRates(e["FAULT_RATES"])
	} else {
		var rate float64
		if rate, err = strconv.ParseFloat(e["FAULT_RATE"], 64); err == nil {
			set, err = fault.NewSet(e["FAULT_TYPES"], rate)
		}
	}
	if err != nil {
		return d, fmt.Errorf("faults: %w", err)
	}
	d.Faults = set.Rates()
	d.ProfileRules = SplitRules(e["DEVICE_PROFILES"])
	if d.ProfileSeed, err = strconv.ParseInt(e["PROFILE_SEED"], 10, 64); err != nil {
		return d, fmt.Errorf("PROFILE_SEED: %w", err)
	}
	if d.DelayMinMS, err = strconv.ParseInt(e["RESPONSE_DELAY_MS_MIN"], 10, 64); err != nil {
		return d, fmt.Errorf("RESPONSE_DELAY_MS_MIN: %w", err)
	}
	if d.DelayMaxMS, err = strconv.ParseInt(e["RESPONSE_DELAY_MS_MAX"], 10, 64); err != nil {
		return d, fmt.Errorf("RESPONSE_DELAY_MS_MAX: %w", err)
	}
	if d.MaxSessions, err = strconv.ParseInt(e["MAX_CONCURRENT_SESSIONS"], 10, 64); err != nil {
		return d, fmt.Errorf("MAX_CONCURRENT_SESSIONS: %w", err)
	}
	if a := e["ACTIVE_DEVICES"]; a == "all" {
		d.ActiveDevices = inst.PortCount
	} else if d.ActiveDevices, err = strconv.Atoi(a); err != nil {
		return d, fmt.Errorf("ACTIVE_DEVICES: %w", err)
	}
	return d, nil
}

// SplitRules splits a --device-profiles spec into its items, as rcfg-sim
// does for reporting.
func SplitRules(spec string) []string {
	out := []string{}
	for _, item := range strings.Split(spec, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
