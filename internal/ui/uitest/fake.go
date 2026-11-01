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
	"sync/atomic"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

// Instance is one fake rcfg-sim instance.
type Instance struct {
	Name     string // its IP, and env file name
	EnvPath  string
	Server   *httptest.Server
	Holder   *runtime.Holder
	Devices  int
	Bytes    atomic.Int64 // what /metrics reports as rcfgsim_bytes_sent_total
	Sessions atomic.Int64 // rcfgsim_sessions_total{result="ok"}
	Faults   atomic.Int64 // rcfgsim_faults_injected_total{type="auth_fail"}
	Active   atomic.Int64 // rcfgsim_active_sessions
	AdminOff atomic.Bool  // /admin/v1/* answers 404, as with --admin unset
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
	mux.HandleFunc(admin.Prefix, func(w http.ResponseWriter, r *http.Request) {
		if in.AdminOff.Load() {
			http.NotFound(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
	in.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if in.Down.Load() {
			// Drop the connection, as a stopped instance would refuse it.
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
					return
				}
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { close(in.done); in.Server.Close() })

	in.EnvPath = filepath.Join(envDir, ip+".env")
	env := fmt.Sprintf("# fake instance %s\nLISTEN_IP=%s\nPORT_START=10000\nPORT_COUNT=%d\nMETRICS_ADDR=%s\nADMIN=true\n%s",
		ip, ip, opt.Devices, in.Addr(), opt.ExtraEnv)
	if err := os.WriteFile(in.EnvPath, []byte(env), 0o660); err != nil {
		t.Fatal(err)
	}
	return in
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
