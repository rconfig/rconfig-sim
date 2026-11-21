// Package uitest provides fake rcfg-sim instances for the controller's
// tests: each is an httptest.Server serving /healthz, /metrics and the real
// admin API (internal/admin) over an in-memory runtime config, plus an env
// file pointing at it. Test-only; nothing in a binary imports it.
package uitest

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
)

// Instance is one fake rcfg-sim instance.
type Instance struct {
	Name     string // its IP, and env file name
	EnvPath  string
	Server   *httptest.Server // /healthz and /metrics (METRICS_ADDR)
	Admin    *httptest.Server // the admin API's own listener (ADMIN_ADDR)
	Holder   *runtime.Holder
	Devices  int
	Bytes    atomic.Int64 // what /metrics reports as rcfgsim_bytes_sent_total
	Sessions atomic.Int64 // rcfgsim_sessions_total{result="ok"}
	Faults   atomic.Int64 // rcfgsim_faults_injected_total{type="auth_fail"}
	Active   atomic.Int64 // rcfgsim_active_sessions
	AdminOff atomic.Bool  // /admin/v1/* answers 404, as if the admin API did not answer
	Down     atomic.Bool  // every request fails, as a stopped instance
	Hosts    []string     // hostnames by port offset
	// Events is the instance's event bus: publish to it to feed
	// /admin/v1/events; Events.Active() reports whether anyone is
	// connected to that stream.
	Events *activity.Bus
	// Table is the instance's device state table, served by /admin/v1/devices.
	Table *activity.DeviceTable
	done  chan struct{}
}

// Options for a fake instance.
type Options struct {
	Devices  int    // default 4
	ExtraEnv string // appended to the env file
	// EventRate caps each /admin/v1/events stream, events per second, as
	// rcfg-sim's 200/s cap does. Zero means effectively uncapped.
	EventRate float64
	// DroppedEvery is how often /admin/v1/events reports drops. Zero means 1s.
	DroppedEvery time.Duration
}

// Start starts a fake instance named ip, with its env file in envDir.
func Start(t *testing.T, envDir, ip string, opt Options) *Instance {
	t.Helper()
	if opt.Devices == 0 {
		opt.Devices = 4
	}
	if opt.EventRate == 0 {
		opt.EventRate = 1e6
	}
	in := &Instance{
		Name:    ip,
		Devices: opt.Devices,
		Holder: runtime.NewHolder(&runtime.Live{
			DelayMin: 50 * time.Millisecond, DelayMax: 500 * time.Millisecond,
			MaxSessions: 5000, ActiveDevices: opt.Devices, ProfileSeed: 42, ProfileRules: []string{},
		}),
		Events: activity.NewBus(opt.EventRate, 0),
		Table:  activity.NewDeviceTable(opt.Devices),
		done:   make(chan struct{}),
	}
	for i := 0; i < opt.Devices; i++ {
		in.Hosts = append(in.Hosts, fmt.Sprintf("rtr-%s-%d", ip, i))
	}
	api := admin.Handler(in, admin.Options{
		Version: "test", ListenIP: ip, PortStart: 10000, PortCount: opt.Devices, Devices: opt.Devices,
		DroppedEvery: opt.DroppedEvery,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "# TYPE rcfgsim_bytes_sent_total counter\nrcfgsim_bytes_sent_total %d\n", in.Bytes.Load())
		fmt.Fprintf(w, "rcfgsim_active_sessions %d\n", in.Active.Load())
		fmt.Fprintf(w, "rcfgsim_sessions_total{result=\"ok\"} %d\nrcfgsim_sessions_total{result=\"error\"} 0\n", in.Sessions.Load())
		fmt.Fprintf(w, "rcfgsim_faults_injected_total{type=\"auth_fail\"} %d\n", in.Faults.Load())
	})
	adminMux := http.NewServeMux()
	adminMux.HandleFunc(admin.Prefix, func(w http.ResponseWriter, r *http.Request) {
		if in.AdminOff.Load() {
			http.NotFound(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	// A stopped instance refuses both listeners: drop the connection.
	downable := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if in.Down.Load() {
				if hj, ok := w.(http.Hijacker); ok {
					if c, _, err := hj.Hijack(); err == nil {
						_ = c.Close()
						return
					}
				}
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	in.Server = httptest.NewServer(downable(mux))
	in.Admin = httptest.NewServer(downable(adminMux))
	t.Cleanup(func() { close(in.done); in.Server.Close(); in.Admin.Close() })

	in.EnvPath = filepath.Join(envDir, ip+".env")
	env := fmt.Sprintf("# fake instance %s\nLISTEN_IP=%s\nPORT_START=10000\nPORT_COUNT=%d\nMETRICS_ADDR=%s\nADMIN_ADDR=%s\n%s",
		ip, ip, opt.Devices, in.Addr(), in.AdminAddr(), opt.ExtraEnv)
	if err := os.WriteFile(in.EnvPath, []byte(env), 0o660); err != nil {
		t.Fatal(err)
	}
	return in
}

// AdminAddr is the fake's admin API address.
func (in *Instance) AdminAddr() string {
	return in.Admin.Listener.Addr().(*net.TCPAddr).String()
}

// Addr is the fake's metrics address.
func (in *Instance) Addr() string {
	return in.Server.Listener.Addr().(*net.TCPAddr).String()
}

// Live returns the fake's current runtime config.
func (in *Instance) Live() *runtime.Live { return in.Holder.Load() }

// admin.Instance implementation.

func (in *Instance) Runtime() *runtime.Holder { return in.Holder }
func (in *Instance) ResolveProfiles(rules []profile.Rule, seed int64) (map[int]*profile.Profile, error) {
	out := map[int]*profile.Profile{}
	for i, h := range in.Hosts {
		if p := profile.Assign(h, seed, rules); p != nil {
			out[10000+i] = p
		}
	}
	return out, nil
}
func (in *Instance) ActiveSessions() int64              { return 3 }
func (in *Instance) DeviceTable() *activity.DeviceTable { return in.Table }
func (in *Instance) Bus() *activity.Bus                 { return in.Events }
func (in *Instance) StartedAt() time.Time               { return time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC) }
func (in *Instance) Done() <-chan struct{}              { return in.done }

// WriteManifest writes a generator-style manifest for the given instances
// (with an optional pinned profile per host) and points each env file at it.
func WriteManifest(t *testing.T, dir string, pinned map[string]string, insts ...*Instance) string {
	t.Helper()
	p := filepath.Join(dir, "manifest.csv")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintln(f, "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket,profile")
	for _, in := range insts {
		for i, h := range in.Hosts {
			fmt.Fprintf(f, "%s,%s,%d,Cisco,cisco_ios,admin,admin,enable123,/x/%s.cfg,sm,%s\n", h, in.Name, 10000+i, h, pinned[h])
		}
		b, _ := os.ReadFile(in.EnvPath)
		if err := os.WriteFile(in.EnvPath, append(b, []byte("MANIFEST="+p+"\n")...), 0o660); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// LoadEnv sets the fake's live config from its env file, as rcfg-sim reads
// its settings when it starts: FAULT_RATES, DEVICE_PROFILES, PROFILE_SEED,
// the delay window, the session cap and ACTIVE_DEVICES, each defaulting as
// the unit does. A fake systemctl calls it on start and restart.
func (in *Instance) LoadEnv() error {
	f, err := envfile.Read(in.EnvPath)
	if err != nil {
		return err
	}
	get := func(k, def string) string {
		if v, ok := f.Values[k]; ok {
			return v
		}
		return def
	}
	num := func(k, def string) (int64, error) {
		n, err := strconv.ParseInt(get(k, def), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", k, err)
		}
		return n, nil
	}
	faults, err := fault.ParseRates(get("FAULT_RATES", ""))
	if err != nil {
		return err
	}
	var rules []string
	for _, r := range strings.Split(get("DEVICE_PROFILES", ""), ",") {
		if r = strings.TrimSpace(r); r != "" {
			rules = append(rules, r)
		}
	}
	if rules == nil {
		rules = []string{}
	}
	seed, err := num("PROFILE_SEED", "42")
	if err != nil {
		return err
	}
	lo, err := num("RESPONSE_DELAY_MS_MIN", "50")
	if err != nil {
		return err
	}
	hi, err := num("RESPONSE_DELAY_MS_MAX", "500")
	if err != nil {
		return err
	}
	caps, err := num("MAX_CONCURRENT_SESSIONS", "5000")
	if err != nil {
		return err
	}
	active := int64(in.Devices)
	if a := get("ACTIVE_DEVICES", "all"); a != "all" {
		if active, err = strconv.ParseInt(a, 10, 64); err != nil {
			return fmt.Errorf("ACTIVE_DEVICES: %w", err)
		}
	}
	in.Holder.Store(&runtime.Live{Faults: faults, ProfileRules: rules, ProfileSeed: seed,
		DelayMin: time.Duration(lo) * time.Millisecond, DelayMax: time.Duration(hi) * time.Millisecond,
		MaxSessions: caps, ActiveDevices: int(min(active, int64(in.Devices)))})
	return nil
}
