package testbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/runs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	shippedscenarios "github.com/rcfg-sim/rcfg-sim/scenarios"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// layout is two instances of 3 devices each.
var layout = jobs.Layout{IPBase: "10.50.0.1", IPCount: 2, PortStart: 10000, DevicesPerIP: 3, Instances: []string{"10.50.0.1", "10.50.0.2"}}

func sample() Testbench {
	return Testbench{
		Name: "Auth storm", Description: "x",
		Devices: ipDevices(1, map[string]int{"sm": 6}),
		Runtime: Runtime{Faults: map[string]float64{"auth_fail": 0.15}, Profiles: Profiles{Seed: 42, Rules: []string{}},
			DelayMS: DelayMS{Min: 0, Max: 10}, MaxSessions: 50, ActiveDevices: Active{All: true}},
	}
}

func ipDevices(seed int64, models map[string]int) Devices {
	return Devices{Source: SourceGenerated, Generated: &Generated{Seed: seed, Families: map[string]*FamilyDevices{"ip": {Models: models}}}}
}

var testCat *catalogue.Catalogue

func cat(t testing.TB) *catalogue.Catalogue {
	t.Helper()
	if testCat == nil {
		c, err := catalogue.Decode(uitest.CatalogueJSON(t))
		if err != nil {
			t.Fatal(err)
		}
		testCat = c
	}
	return testCat
}

func newStore(t testing.TB, dir string) *Store {
	s := NewStore(dir, catalogue.Static(cat(t)))
	s.UseScenarios(scenarios.NewLibrary(shippedscenarios.Shipped, "", catalogue.Static(cat(t))))
	return s
}

func TestShippedTestbenchesValidate(t *testing.T) {
	all, err := newStore(t, t.TempDir()).List()
	if err != nil {
		t.Fatal(err)
	}
	var shipped []Testbench
	for _, tb := range all {
		if tb.Shipped {
			shipped = append(shipped, tb)
		}
	}
	if len(shipped) != 11 {
		t.Fatalf("%d shipped testbenches, want scenarios 3 to 9, three optical or mixed, and mesh campus", len(shipped))
	}
	kinds := map[string]int{}
	for _, tb := range shipped {
		if tb.Problem != "" {
			t.Errorf("%s needs attention: %s", tb.Slug, tb.Problem)
		}
		kinds[tb.Devices.Kind()]++
	}
	if kinds["ip"] != 7 || kinds["optical"] != 2 || kinds["mix"] != 1 || kinds["scenario"] != 1 {
		t.Fatalf("kinds %v", kinds)
	}
	big := jobs.Layout{IPBase: "10.50.0.1", IPCount: 20, PortStart: 10000, DevicesPerIP: 2500}
	for i := 1; i <= 20; i++ {
		big.Instances = append(big.Instances, fmt.Sprintf("10.50.0.%d", i))
	}
	for _, tb := range shipped {
		if err := Validate(tb, cat(t)); err != nil {
			t.Errorf("%s: %v", tb.Slug, err)
		}
		// The scenarios' own host: 20 IPs of 2,500. Scenario 3 (500) and 4
		// (5,000) do not fit it, and must say why. Mesh campus (3 devices)
		// fits any host with 3 ports: it need not fill every instance.
		why := Fit(tb, big, nil)
		if fits := tb.Devices.Total() == 50000 || tb.Devices.Source == SourceScenario; fits != (len(why) == 0) {
			t.Errorf("%s on 20x2500: fit reasons %v", tb.Slug, why)
		}
		// This test host (2 instances of 3) fits only mesh campus.
		if why := Fit(tb, layout, nil); (len(why) == 0) != (tb.Slug == "topology-mesh-campus") {
			t.Errorf("%s on a 6-device host: fit reasons %v", tb.Slug, why)
		}
	}
	s8, _ := newStore(t, t.TempDir()).Get("s8-localised-faults")
	if s8.Overrides["10.50.0.3"].Faults["auth_fail"] != 0.1 {
		t.Fatalf("scenario 8 override: %+v", s8.Overrides)
	}
}

func TestValidate(t *testing.T) {
	ok := sample()
	if err := Validate(ok, cat(t)); err != nil {
		t.Fatal(err)
	}
	bad := func(f func(*Testbench)) Testbench {
		tb := sample()
		tb.Overrides = map[string]Override{}
		f(&tb)
		return tb
	}
	neg := int64(0)
	for name, tb := range map[string]Testbench{
		"name":        bad(func(tb *Testbench) { tb.Name = " " }),
		"devices":     bad(func(tb *Testbench) { tb.Devices = ipDevices(1, map[string]int{"sm": 0}) }),
		"model":       bad(func(tb *Testbench) { tb.Devices = ipDevices(1, map[string]int{"nope": 1}) }),
		"source":      bad(func(tb *Testbench) { tb.Devices.Source = "magic" }),
		"fault":       bad(func(tb *Testbench) { tb.Runtime.Faults = map[string]float64{"auth_fail": 2} }),
		"profile":     bad(func(tb *Testbench) { tb.Runtime.Profiles.Rules = []string{"bogus"} }),
		"delay":       bad(func(tb *Testbench) { tb.Runtime.DelayMS = DelayMS{Min: 10, Max: 5} }),
		"sessions":    bad(func(tb *Testbench) { tb.Runtime.MaxSessions = 0 }),
		"active":      bad(func(tb *Testbench) { tb.Runtime.ActiveDevices = Active{N: 7} }),
		"override ip": bad(func(tb *Testbench) { tb.Overrides["host-a"] = Override{} }),
		"override":    bad(func(tb *Testbench) { tb.Overrides["10.50.0.1"] = Override{MaxSessions: &neg} }),
		"duration":    bad(func(tb *Testbench) { tb.Duration = "soon" }),
	} {
		if err := Validate(tb, cat(t)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// Overrides for an IP this host lacks are valid, but do not fit.
	tb := sample()
	tb.Overrides = map[string]Override{"10.50.0.9": {}}
	if err := Validate(tb, cat(t)); err != nil {
		t.Fatal(err)
	}
	if why := Fit(tb, layout, nil); len(why) != 1 || !strings.Contains(why[0], "10.50.0.9") {
		t.Fatalf("fit: %v", why)
	}
	if why := Fit(sample(), layout, errors.New("not adopted")); len(why) != 1 {
		t.Fatalf("fit with no layout: %v", why)
	}
}

func TestActiveJSON(t *testing.T) {
	for in, want := range map[string]Active{`"all"`: {All: true}, `0`: {N: 0}, `12`: {N: 12}} {
		var a Active
		if err := json.Unmarshal([]byte(in), &a); err != nil || a != want {
			t.Errorf("%s: %+v %v", in, a, err)
		}
		if b, _ := json.Marshal(a); string(b) != in {
			t.Errorf("round trip %s: %s", in, b)
		}
	}
	for _, in := range []string{`"some"`, `-1`, `1.5`} {
		var a Active
		if json.Unmarshal([]byte(in), &a) == nil {
			t.Errorf("%s: want an error", in)
		}
	}
}

func TestResolve(t *testing.T) {
	tb := sample()
	tb.Devices = ipDevices(1, map[string]int{"sm": 5}) // 3 + 2
	tb.Runtime.ActiveDevices = Active{N: 4}
	ms := int64(7)
	tb.Overrides = map[string]Override{"10.50.0.2": {Faults: map[string]float64{"disconnect_mid": 0.2}, MaxSessions: &ms}}
	got, err := Resolve(tb, layout)
	if err != nil {
		t.Fatal(err)
	}
	a, b := got["10.50.0.1"], got["10.50.0.2"]
	if a.Faults["auth_fail"] != 0.15 || a.MaxSessions != 50 || b.MaxSessions != 7 || b.Faults["auth_fail"] != 0 || b.Faults["disconnect_mid"] != 0.2 {
		t.Fatalf("resolve: %+v %+v", a, b)
	}
	if a.ActiveDevices+b.ActiveDevices != 4 || a.ActiveDevices > 3 || b.ActiveDevices > 2 {
		t.Fatalf("active split %d + %d", a.ActiveDevices, b.ActiveDevices)
	}
	tb.Runtime.ActiveDevices = Active{All: true}
	got, _ = Resolve(tb, layout)
	if got["10.50.0.1"].ActiveDevices != 3 || got["10.50.0.2"].ActiveDevices != 2 {
		t.Fatalf("all: %+v", got)
	}

	// And back: FromInstances recovers the runtime and the override.
	rt, ov := FromInstances(got, map[string]int{"10.50.0.1": 3, "10.50.0.2": 2}, layout.Instances)
	if !rt.ActiveDevices.All || rt.MaxSessions != 50 || len(ov) != 1 || *ov["10.50.0.2"].MaxSessions != 7 || ov["10.50.0.2"].Faults["disconnect_mid"] != 0.2 {
		t.Fatalf("from instances: %+v %+v", rt, ov)
	}
}

func TestStore(t *testing.T) {
	s := newStore(t, t.TempDir())
	tb, err := s.Create(sample())
	if err != nil || tb.Version != 1 || tb.Slug != "auth-storm" {
		t.Fatalf("create: %+v %v", tb, err)
	}
	if _, err := s.Create(sample()); !errors.Is(err, ErrExists) {
		t.Fatalf("create twice: %v", err)
	}
	tb.Description = "changed"
	tb, err = s.Save("auth-storm", tb)
	if err != nil || tb.Version != 2 {
		t.Fatalf("save: %+v %v", tb, err)
	}
	got, err := s.Get("auth-storm")
	if err != nil || got.Description != "changed" || got.Version != 2 {
		t.Fatalf("get: %+v %v", got, err)
	}
	// Rename on save.
	got.Name = "Auth storm 2"
	if got, err = s.Save("auth-storm", got); err != nil || got.Slug != "auth-storm-2" || got.Version != 3 {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if _, err := s.Get("auth-storm"); err == nil {
		t.Fatal("old slug still there")
	}
	// Shipped: read-only, name taken, can be duplicated.
	if _, err := s.Save("s5-full-fleet", shippedBy(t, "s5-full-fleet")); err == nil {
		t.Fatal("saved over a shipped testbench")
	}
	if err := s.Delete("s5-full-fleet"); err == nil {
		t.Fatal("deleted a shipped testbench")
	}
	dup := shippedBy(t, "s5-full-fleet")
	dup.Name = s.DuplicateName(dup.Name)
	if dup.Name != "Scenario 5: full fleet, steady state (copy)" {
		t.Fatalf("duplicate name %q", dup.Name)
	}
	if _, err := s.Create(dup); err != nil {
		t.Fatal(err)
	}
	if s.DuplicateName(shippedBy(t, "s5-full-fleet").Name) != "Scenario 5: full fleet, steady state (copy 2)" {
		t.Fatal("second duplicate name")
	}
	// Export and import round trip; import refuses unknown fields and bad values.
	exp := Encode(got)
	imp, err := Decode(exp, cat(t))
	if err != nil || imp.Name != got.Name || imp.Version != 3 {
		t.Fatalf("decode export: %+v %v", imp, err)
	}
	if _, err := Decode([]byte(`{"name":"x","bogus":1}`), cat(t)); err == nil {
		t.Fatal("unknown field accepted")
	}
	if _, err := Decode([]byte(`{"name":"x","build":{"devices":0}}`), cat(t)); err == nil {
		t.Fatal("invalid testbench accepted")
	}
	// Traversal: a slug is never a path.
	if _, err := s.Get("../x"); err == nil {
		t.Fatal("got ../x")
	}
	list, err := s.List()
	if err != nil || len(list) != 13 || !list[0].Shipped || list[11].Shipped != false {
		t.Fatalf("list: %d %v", len(list), err)
	}
	must(t, s.Delete("auth-storm-2"))
}

// --- lifecycle ---

type lrig struct {
	t      *testing.T
	root   string // data
	envDir string
	data   string // --data-dir
	insts  map[string]*uitest.Instance
	fleet  *fleet.Fleet
	runner *jobs.Runner
	store  *Store
	runs   *runs.Store
	m      *Manager
	args   string

	mu    sync.Mutex
	calls []string
	now   time.Time
}

func newLRig(t *testing.T) *lrig {
	t.Helper()
	tmp := t.TempDir()
	r := &lrig{t: t, root: filepath.Join(tmp, "data"), envDir: filepath.Join(tmp, "etc"), data: filepath.Join(tmp, "ui"),
		insts: map[string]*uitest.Instance{}, args: filepath.Join(tmp, "gen-args"), now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	gen := filepath.Join(tmp, "fake-gen")
	uitest.WriteFakeGen(t, gen)
	t.Setenv("FAKE_GEN_ARGS", r.args)

	// Build B0: 6 devices on 127.0.0.1-2, 3 each.
	b0 := filepath.Join(r.root, "builds", "B0")
	must(t, os.MkdirAll(filepath.Join(b0, "configs"), 0o755))
	var man strings.Builder
	man.WriteString("hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n")
	for i := 0; i < 6; i++ {
		f := filepath.Join(b0, "configs", fmt.Sprintf("device-%d.cfg", i))
		must(t, os.WriteFile(f, []byte("old\n"), 0o644))
		fmt.Fprintf(&man, "old-%d,127.0.0.%d,%d,Cisco,cisco_ios,ops,s3cret,en4ble,%s,sm\n", i, 1+i/3, 10000+i%3, f)
	}
	must(t, os.WriteFile(filepath.Join(b0, "manifest.csv"), []byte(man.String()), 0o644))
	seed := int64(7)
	must(t, jobsWriteSettings(b0, seed))
	must(t, os.Symlink("builds/B0", filepath.Join(r.root, "current")))
	must(t, os.MkdirAll(r.envDir, 0o755))
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		r.insts[ip] = uitest.Start(t, r.envDir, ip, uitest.Options{Devices: 3,
			ExtraEnv: "MANIFEST=" + filepath.Join(r.root, "current", "manifest.csv") + "\n"})
	}

	r.fleet = fleet.New(fleet.Options{EnvDir: r.envDir})
	units := systemd.New("systemctl", r.systemctl)
	r.runner = jobs.NewRunner(jobs.Options{
		EnvDir: r.envDir, GenBin: gen, LogDir: filepath.Join(tmp, "jobs"), Units: units,
		Healthy: r.fleet.Healthy, Pause: time.Millisecond, Wait: 2 * time.Second, Now: r.clock,
		Free:      func(string) (int64, error) { return 1 << 40, nil },
		Catalogue: catalogue.NewSource(gen, nil),
	})
	t.Cleanup(r.runner.Close)
	r.store = NewStore(filepath.Join(r.data, "testbenches"), catalogue.NewSource(gen, nil))
	r.runs = runs.NewStore(filepath.Join(r.data, "runs"), 0, r.clock)
	r.fleet.Poll(context.Background())
	r.newManager()
	return r
}

func jobsWriteSettings(dir string, seed int64) error {
	b, _ := json.Marshal(map[string]any{"name": "B0", "created": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "count": 6, "seed": seed, "distribution": "sm:100"})
	return os.WriteFile(filepath.Join(dir, "build.json"), b, 0o644)
}

func (r *lrig) newManager() {
	r.m = NewManager(ManagerOptions{DataDir: r.data, Store: r.store, Runs: r.runs, Jobs: r.runner, Fleet: r.fleet,
		Units: systemd.New("systemctl", r.systemctl), StartTimeout: 2 * time.Second, WaitEvery: 10 * time.Millisecond, Now: r.clock})
	r.t.Cleanup(r.m.Close)
}

func (r *lrig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *lrig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// systemctl drives the fake instances: stop takes one down, start brings
// it up on its env file's settings, as rcfg-sim would read them.
func (r *lrig) systemctl(_ context.Context, _ string, args ...string) ([]byte, error) {
	verb, unit := args[0], args[len(args)-1]
	ip := strings.TrimSuffix(strings.TrimPrefix(unit, "rcfg-sim@"), ".service")
	r.mu.Lock()
	r.calls = append(r.calls, verb+" "+ip)
	r.mu.Unlock()
	in := r.insts[ip]
	if in == nil {
		return []byte("unknown"), errors.New("no such unit")
	}
	switch verb {
	case "is-active":
		if in.Down.Load() {
			return []byte("inactive\n"), errors.New("exit 3")
		}
		return []byte("active\n"), nil
	case "stop":
		in.Down.Store(true)
	case "start", "restart":
		if err := in.LoadEnv(); err != nil {
			r.t.Errorf("%s: %v", in.Name, err)
		}
		in.Down.Store(false)
	}
	return nil, nil
}

func (r *lrig) callsOf(verb string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		if strings.HasPrefix(c, verb+" ") {
			out = append(out, strings.TrimPrefix(c, verb+" "))
		}
	}
	return out
}

func (r *lrig) resetCalls() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

func (r *lrig) wait(j *jobs.Job, err error) error {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
	select {
	case <-j.Done():
	case <-time.After(20 * time.Second):
		r.t.Fatal("job did not finish")
	}
	return j.Err()
}

func (r *lrig) generated() bool {
	_, err := os.Stat(r.args)
	_ = os.Remove(r.args)
	return err == nil
}

func (r *lrig) testbench(name string, seed int64, f func(*Testbench)) Testbench {
	tb := sample()
	tb.Name, tb.Devices.Generated.Seed = name, seed
	if f != nil {
		f(&tb)
	}
	tb.Overrides = nil
	if f != nil {
		f(&tb)
	}
	tb, err := r.store.Create(tb)
	must(r.t, err)
	return tb
}

func TestLifecycleStartStopRestart(t *testing.T) {
	r := newLRig(t)
	r.insts["127.0.0.2"].Down.Store(true) // one instance down to begin with
	tb := r.testbench("A", 1, func(tb *Testbench) {
		tb.Overrides = map[string]Override{"127.0.0.2": {Faults: map[string]float64{"disconnect_mid": 0.2}}}
	})

	if err := r.wait(r.m.Start(tb.Slug, false)); err != nil {
		lines, _ := r.runner.Current().Lines(0)
		t.Fatalf("start: %v\n%s", err, strings.Join(lines, "\n"))
	}
	if !r.generated() {
		t.Fatal("first start must generate")
	}
	a := r.m.Active()
	if a.State != Running || a.Name != "A" || a.RunID == "" {
		t.Fatalf("active: %+v", a)
	}
	// The down instance was started; the up one was restarted for the new build.
	if got := r.callsOf("start"); !slices.Equal(got, []string{"127.0.0.2"}) {
		t.Fatalf("starts %v", got)
	}
	if got := r.callsOf("restart"); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Fatalf("restarts %v", got)
	}
	// The override reached only its instance and its env file.
	l1, l2 := r.insts["127.0.0.1"].Live(), r.insts["127.0.0.2"].Live()
	if l1.Faults.Rates()["auth_fail"] != 0.15 || l1.Faults.Rates()["disconnect_mid"] != 0 ||
		l2.Faults.Rates()["disconnect_mid"] != 0.2 || l2.Faults.Rates()["auth_fail"] != 0 {
		t.Fatalf("live faults: %v / %v", l1.Faults.Rates(), l2.Faults.Rates())
	}
	env1, _ := os.ReadFile(r.insts["127.0.0.1"].EnvPath)
	env2, _ := os.ReadFile(r.insts["127.0.0.2"].EnvPath)
	if strings.Contains(string(env1), "disconnect_mid") || !strings.Contains(string(env2), "FAULT_RATES=disconnect_mid=0.2") {
		t.Fatalf("env files:\n%s\n%s", env1, env2)
	}
	if b, err := os.ReadFile(filepath.Join(r.data, "active.json")); err != nil || !strings.Contains(string(b), `"state": "running"`) {
		t.Fatalf("active.json: %s %v", b, err)
	}

	// Traffic, then a restart (counters reset), then more traffic.
	in := r.insts["127.0.0.1"]
	in.Sessions.Store(10)
	r.fleet.Poll(context.Background())
	in.Sessions.Store(25)
	r.fleet.Poll(context.Background())
	r.resetCalls()
	if err := r.wait(r.m.Restart()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := r.callsOf("restart"); len(got) != 2 {
		t.Fatalf("restart restarted %v", got)
	}
	in.Sessions.Store(4) // reset by the restart
	r.fleet.Poll(context.Background())

	r.advance(time.Minute)
	if err := r.wait(r.m.Stop()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := r.callsOf("stop"); len(got) != 2 {
		t.Fatalf("stops %v", got)
	}
	if a := r.m.Active(); a.State != Idle {
		t.Fatalf("after stop: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(r.data, "active.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active.json left behind")
	}

	run, err := r.runs.Get(a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != runs.Stopped || run.Ended.IsZero() || run.Name != "A" {
		t.Fatalf("run: %+v", run)
	}
	// The run recorded the build it ran on.
	if cur, _ := os.Readlink(filepath.Join(r.root, "current")); run.Build == "" || "builds/"+run.Build != cur || len(run.ManifestSHA256) != 64 {
		t.Fatalf("run build %q sha %q, current %s", run.Build, run.ManifestSHA256, cur)
	}
	// 0 -> 10 -> 25 while running, then 4 after the restart's reset.
	if got := run.Instances["127.0.0.1"].Sessions["ok"]; got != 29 {
		t.Fatalf("ok sessions on 127.0.0.1: %v, want 29", got)
	}
	if run.Totals.Sessions["ok"] != 29 {
		t.Fatalf("total ok sessions %v", run.Totals.Sessions["ok"])
	}
	var kinds []string
	for _, e := range run.Events {
		kinds = append(kinds, e.Kind)
	}
	for _, k := range []string{"start", "build", "apply", "instance", "restart", "running", "stop"} {
		if !slices.Contains(kinds, k) {
			t.Errorf("no %q event in %v", k, kinds)
		}
	}
}

func TestLifecycleSwitchReusesBuild(t *testing.T) {
	r := newLRig(t)
	a := r.testbench("A", 1, nil)
	b := r.testbench("B", 2, nil)

	must(t, r.wait(r.m.Start(a.Slug, false)))
	if !r.generated() {
		t.Fatal("A: want a generation")
	}
	if _, err := r.m.Start(b.Slug, false); !errors.Is(err, ErrActive) {
		t.Fatalf("start while active: %v, want ErrActive", err)
	}
	must(t, r.wait(r.m.Start(b.Slug, true)))
	if !r.generated() {
		t.Fatal("B: want a generation")
	}
	if r.m.Active().Name != "B" {
		t.Fatal("B is not active")
	}
	// Back to A: the kept build is reused, no generator run.
	must(t, r.wait(r.m.Start(a.Slug, true)))
	if r.generated() {
		t.Fatal("switching back to A ran the generator")
	}
	list, _ := r.runs.List()
	if len(list) != 3 || list[1].Outcome != runs.Stopped || list[0].Outcome != runs.Running {
		t.Fatalf("runs: %d", len(list))
	}
}

func TestLifecycleFailureMidStart(t *testing.T) {
	r := newLRig(t)
	tb := r.testbench("A", 1, nil)
	// 127.0.0.2 never comes up: its start does nothing.
	r.insts["127.0.0.2"].Down.Store(true)
	r.insts["127.0.0.2"].AdminOff.Store(true)
	err := r.wait(r.m.Start(tb.Slug, false))
	if err == nil {
		t.Fatal("start succeeded")
	}
	a := r.m.Active()
	if a.State != Failed || !strings.Contains(a.Error, "127.0.0.2") {
		t.Fatalf("active: %+v", a)
	}
	run, _ := r.runs.Get(a.RunID)
	if run.Outcome != runs.Failed || !strings.Contains(run.Reason, "127.0.0.2") {
		t.Fatalf("run: %+v", run)
	}
	// Nothing rolled back: the instance that was up keeps the settings.
	if r.insts["127.0.0.1"].Live().MaxSessions != 50 {
		t.Fatal("instance settings rolled back")
	}
	must(t, r.m.Dismiss())
	if r.m.Active().State != Idle {
		t.Fatal("dismiss")
	}
}

func TestLifecycleAutoStopAndResume(t *testing.T) {
	r := newLRig(t)
	tb := r.testbench("Timed", 1, func(tb *Testbench) { tb.Duration = "1h" })
	must(t, r.wait(r.m.Start(tb.Slug, false)))
	a := r.m.Active()
	if a.StopAt.IsZero() {
		t.Fatal("no auto-stop time")
	}
	// The UI restarts mid-run: a new manager resumes from active.json.
	r.m.Close()
	r.newManager()
	if got := r.m.Active(); got.State != Running || got.RunID != a.RunID {
		t.Fatalf("resumed: %+v", got)
	}
	// Past the stop time when the UI restarts again: it stops at once.
	r.m.Close()
	r.advance(2 * time.Hour)
	r.newManager()
	deadline := time.Now().Add(10 * time.Second)
	for r.m.Active().State != Idle {
		if time.Now().After(deadline) {
			t.Fatalf("not auto-stopped: %+v", r.m.Active())
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, _ := r.runs.Get(a.RunID)
	if run.Outcome != runs.AutoStopped {
		t.Fatalf("outcome %s", run.Outcome)
	}
	var resumed, gaps int
	for _, e := range run.Events {
		if e.Kind == "resume" {
			resumed++
		}
		if e.Kind == runs.UIGap {
			gaps++
			if e.Start.IsZero() || !e.End.After(e.Start) && !e.End.Equal(e.Start) {
				t.Errorf("ui_gap without a span: %+v", e)
			}
		}
	}
	if resumed != 2 || gaps != 2 || !run.Incomplete() {
		t.Fatalf("want 2 resumes and 2 ui_gap events, incomplete totals: %+v", run.Events)
	}
}

func TestLifecycleChangesAndSave(t *testing.T) {
	r := newLRig(t)
	tb := r.testbench("A", 1, nil)
	must(t, r.wait(r.m.Start(tb.Slug, false)))

	// A change on the Failures screen, on one instance only.
	r.fleet.Poll(context.Background())
	res := r.fleet.ApplySessionCaps(context.Background(), map[string]int64{"127.0.0.2": 9})
	if !res[0].OK() {
		t.Fatal(res[0].Err)
	}
	r.m.Changed("Session cap 127.0.0.2 = 9")
	if !r.m.Active().Modified {
		t.Fatal("not marked modified")
	}
	// Restart waits for up and no drift against the env files, which hold
	// the change, not for the saved testbench's values.
	if err := r.wait(r.m.Restart()); err != nil {
		t.Fatalf("restart after a change: %v", err)
	}
	if r.insts["127.0.0.2"].Live().MaxSessions != 9 {
		t.Fatal("the change did not survive the restart")
	}
	// Notes on the active run go through the manager, so its saves keep them.
	if ok, err := r.m.Annotate(r.m.Active().RunID, "first try", []string{"smoke"}); !ok || err != nil {
		t.Fatalf("annotate: %v %v", ok, err)
	}
	if ok, _ := r.m.Annotate("other", "x", nil); ok {
		t.Fatal("annotated a run that is not active")
	}
	saved, err := r.m.SaveToTestbench()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 2 || saved.Overrides["127.0.0.2"].MaxSessions == nil || *saved.Overrides["127.0.0.2"].MaxSessions != 9 {
		t.Fatalf("saved: %+v", saved)
	}
	if a := r.m.Active(); a.Modified || a.Version != 2 {
		t.Fatalf("after save: %+v", a)
	}

	// Another change, then revert.
	r.fleet.Poll(context.Background())
	r.fleet.ApplySessionCaps(context.Background(), map[string]int64{"127.0.0.1": 3})
	r.m.Changed("Session cap 127.0.0.1 = 3")
	must(t, r.wait(r.m.Revert()))
	if r.insts["127.0.0.1"].Live().MaxSessions != 50 || r.m.Active().Modified {
		t.Fatal("revert")
	}
	if run, _ := r.runs.Get(r.m.Active().RunID); run.Notes != "first try" || !run.HasTag("smoke") {
		t.Fatalf("notes lost: %+v", run)
	}
}

func shippedBy(t *testing.T, slug string) Testbench {
	t.Helper()
	for _, tb := range Shipped(cat(t)) {
		if tb.Slug == slug {
			return tb
		}
	}
	t.Fatalf("no shipped %s", slug)
	return Testbench{}
}

// TestMigrateOldFormat: a Phase 6 file with a build section loads, its
// distribution becomes counts in the right families, and it runs as
// before (same counts, so the same build).
func TestMigrateOldFormat(t *testing.T) {
	old := []byte(`{"name":"Old","version":4,
		"build":{"devices":10,"distribution":"sm:60,md:20,ciena-6500-tl1-gne:20","seed":7,"options":{"rne-dual-home-pct":"25"}},
		"runtime":{"faults":{},"profiles":{"seed":42,"rules":[]},"delay_ms":{"min":0,"max":10},"max_sessions":5,"active_devices":"all"}}`)
	tb, err := Decode(old, cat(t))
	if err != nil {
		t.Fatal(err)
	}
	ip, opt := tb.Devices.Family("ip"), tb.Devices.Family("optical")
	if tb.Build != nil || tb.Devices.Seed() != 7 || ip.Models["sm"] != 6 || ip.Models["md"] != 2 || opt.Models["ciena-6500-tl1-gne"] != 2 ||
		opt.Options["rne_dual_home_pct"] != 25 || tb.Version != 4 {
		t.Fatalf("migrated: %+v %+v %+v", tb.Devices, ip, opt)
	}
	if tb.Devices.SSHAuth() != SSHAuthPassword {
		t.Fatal("migrated ssh_auth must stay password (today's default)")
	}
	req := tb.Devices.Request(cat(t))
	if req.Options["rne-dual-home-pct"] != "25" || req.Counts["sm"] != 6 || req.Seed != 7 {
		t.Fatalf("request %+v", req)
	}
	// Saved again, it is written in the new format.
	if b := string(Encode(tb)); strings.Contains(b, `"build"`) || !strings.Contains(b, `"source": "generated"`) {
		t.Fatalf("encoded:\n%s", b)
	}
	if _, err := Decode([]byte(`{"name":"x","build":{"devices":5,"distribution":"sm:100","seed":1,"options":{"bogus":"1"}},"runtime":{}}`), cat(t)); err == nil {
		t.Fatal("unknown generator option accepted on migration")
	}
}

// TestNeedsAttention: a stored testbench the catalogue no longer accepts
// (here an old file with an unknown generator option, and a file that is
// not JSON) still lists, with the reason, and cannot start.
func TestNeedsAttention(t *testing.T) {
	r := newLRig(t)
	dir := filepath.Join(r.data, "testbenches")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	old := `{"name":"Old","version":2,"build":{"devices":5,"distribution":"sm:100","seed":1,"options":{"bogus":"1"}},
		"runtime":{"faults":{},"profiles":{"seed":1,"rules":[]},"delay_ms":{"min":0,"max":0},"max_sessions":5,"active_devices":"all"}}`
	for f, b := range map[string]string{"old.json": old, "broken.json": "{not json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(b), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.store.List()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Testbench{}
	for _, tb := range list {
		got[tb.Slug] = tb
	}
	o, b := got["old"], got["broken"]
	if !strings.Contains(o.Problem, `"bogus"`) || o.Name != "Old" || o.Devices.Family("ip").Models["sm"] != 5 {
		t.Fatalf("old: %+v", o)
	}
	if b.Problem == "" || b.Name != "broken" {
		t.Fatalf("broken: %+v", b)
	}
	for _, sh := range Shipped(cat(t)) {
		if sh.Problem != "" {
			t.Fatalf("shipped %s: %s", sh.Slug, sh.Problem)
		}
	}
	if _, err := r.m.Start("old", false); err == nil || !strings.Contains(err.Error(), "needs attention") {
		t.Fatalf("start of an invalid testbench: %v", err)
	}
	// Fixed in the editor and saved, it validates again.
	o.Devices.Generated.Families["ip"].Options = nil
	if _, err := r.store.Save("old", o); err != nil {
		t.Fatal(err)
	}
	if tb, _ := r.store.Get("old"); tb.Problem != "" {
		t.Fatalf("still a problem after save: %s", tb.Problem)
	}
}

func TestFamilyRules(t *testing.T) {
	opt := func(f func(*FamilyDevices)) Testbench {
		tb := sample()
		tb.Devices.Generated.Families["optical"] = &FamilyDevices{Models: map[string]int{"ciena-6500-tl1-gne": 2}, SSHAuth: SSHAuthDriver}
		if f != nil {
			f(tb.Devices.Generated.Families["optical"])
		}
		return tb
	}
	if err := Validate(opt(nil), cat(t)); err != nil || opt(nil).Devices.Kind() != "mix" {
		t.Fatalf("service provider mix: %v", err)
	}
	for name, tb := range map[string]Testbench{
		"none with IP":   opt(func(f *FamilyDevices) { f.SSHAuth = SSHAuthNone }),
		"bad ssh_auth":   opt(func(f *FamilyDevices) { f.SSHAuth = "sometimes" }),
		"wrong family":   opt(func(f *FamilyDevices) { f.Models["sm"] = 1 }),
		"unknown option": opt(func(f *FamilyDevices) { f.Options = map[string]int{"nope": 1} }),
		"option range":   opt(func(f *FamilyDevices) { f.Options = map[string]int{"rne_dual_home_pct": 101} }),
		"ssh_auth on ip": func() Testbench {
			tb := sample()
			tb.Devices.Generated.Families["ip"].SSHAuth = SSHAuthDriver
			return tb
		}(),
		"unknown family": func() Testbench {
			tb := sample()
			tb.Devices.Generated.Families["topology"] = &FamilyDevices{}
			return tb
		}(),
		"empty fleet": func() Testbench {
			tb := sample()
			tb.Devices.Generated.Families["ip"].Models = map[string]int{}
			return tb
		}(),
	} {
		if err := Validate(tb, cat(t)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// Optical only: none is allowed.
	tb := opt(func(f *FamilyDevices) { f.SSHAuth = SSHAuthNone })
	delete(tb.Devices.Generated.Families, "ip")
	if err := Validate(tb, cat(t)); err != nil || tb.Devices.Kind() != "optical" || tb.Devices.SSHAuth() != SSHAuthNone {
		t.Fatalf("optical-only none: %v", err)
	}
}

// TestLifecycleSSHAuth: the SSH login is written to every env file and,
// with the build unchanged, only it makes Start restart the up instances.
func TestLifecycleSSHAuth(t *testing.T) {
	r := newLRig(t)
	mix := func(name string, auth string) Testbench {
		return r.testbench(name, 1, func(tb *Testbench) {
			tb.Devices.Generated.Families["ip"].Models = map[string]int{"sm": 4}
			tb.Devices.Generated.Families["optical"] = &FamilyDevices{Models: map[string]int{"ciena-6500-tl1-gne": 2}, SSHAuth: auth}
		})
	}
	a, b := mix("A", SSHAuthPassword), mix("B", SSHAuthDriver)
	must(t, r.wait(r.m.Start(a.Slug, false)))
	must(t, r.wait(r.m.Stop()))
	// The instances come back up by hand, then B starts: same build, new
	// SSH login.
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		_, _ = r.systemctl(context.Background(), "systemctl", "start", "rcfg-sim@"+ip+".service")
	}
	r.fleet.Poll(context.Background())
	_ = r.generated()
	r.resetCalls()
	must(t, r.wait(r.m.Start(b.Slug, false)))
	if r.generated() {
		t.Fatal("B has A's build settings; it must reuse the build")
	}
	if got := r.callsOf("restart"); len(got) != 2 {
		t.Fatalf("SSH login change: restarts %v, want both instances", got)
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		if env := envOf(t, r.insts[ip].EnvPath); !strings.Contains(env, "SSH_AUTH=driver\n") ||
			strings.Contains(env, "PASSWORD=") || strings.Contains(env, "COMMANDS_ROOT=") {
			// Without a scenario before it, a generated start leaves the
			// passwords and commands to the operator.
			t.Fatalf("%s env:\n%s", ip, env)
		}
	}
}

func envOf(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLifecycleScenario: starting Topology: mesh campus installs the
// package as a build on the first instance, sets the scenario's env keys,
// and stops the instance it does not use, which every fleet-wide action
// then leaves out. A second start reuses the build; a generated testbench
// afterwards restores the keys and starts every instance again.
func TestLifecycleScenario(t *testing.T) {
	r := newLRig(t)
	r.store.UseScenarios(scenarios.NewLibrary(shippedscenarios.Shipped, "", catalogue.Static(cat(t))))
	ctx := context.Background()
	gen := r.testbench("Gen", 7, nil)
	// Values set by hand before the scenario: 127.0.0.1 has its own
	// PASSWORD and COMMANDS_ROOT; 127.0.0.2 sets none of the three.
	f, err := os.OpenFile(r.insts["127.0.0.1"].EnvPath, os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString("PASSWORD=hand-set\nCOMMANDS_ROOT=/srv/my-commands\n")
	must(t, err)
	must(t, f.Close())

	must(t, r.wait(r.m.Start("topology-mesh-campus", false)))
	if r.generated() {
		t.Fatal("a scenario start ran the generator")
	}
	snap := envOf(t, filepath.Join(r.data, "owned-keys", "127.0.0.1.json"))
	if !strings.Contains(snap, `"PASSWORD": "hand-set"`) || !strings.Contains(snap, `"COMMANDS_ROOT": "/srv/my-commands"`) || !strings.Contains(snap, `"ENABLE_PASSWORD": null`) {
		t.Fatalf("snapshot of 127.0.0.1:\n%s", snap)
	}
	st := r.runner.Status()
	if !strings.HasPrefix(st.Live.Name, "mesh-campus-v1-") || st.Live.Scenario != "mesh-campus v1" || st.Live.Count != 3 || st.Live.Previous != "B0" {
		t.Fatalf("live build: %+v", st.Live)
	}
	man := envOf(t, filepath.Join(r.root, "current", "manifest.csv"))
	dir := filepath.Join(r.root, "builds", st.Live.Name)
	for _, want := range []string{
		"core-01,127.0.0.1,10000,Arista,cisco_ios,admin,,," + filepath.Join(dir, "configs", "core-01.cfg") + ",sm\n",
		"dist-01,127.0.0.1,10001,Juniper,junos,admin,,," + filepath.Join(dir, "configs", "dist-01.cfg") + ",sm\n",
		"access-01,127.0.0.1,10002,",
	} {
		if !strings.Contains(man, want) {
			t.Fatalf("manifest missing %q:\n%s", want, man)
		}
	}
	if _, err := os.Stat(filepath.Join(r.root, "current", "commands", "core-01", "show_lldp_neighbors_detail.txt")); err != nil {
		t.Fatal("commands not installed")
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		env := envOf(t, r.insts[ip].EnvPath)
		for _, want := range []string{"COMMANDS_ROOT=" + filepath.Join(r.root, "current", "commands") + "\n", "PASSWORD=\n", "ENABLE_PASSWORD=\n", "SSH_AUTH=password\n"} {
			if !strings.Contains(env, want) {
				t.Errorf("%s env missing %q:\n%s", ip, want, env)
			}
		}
	}
	// The second instance has no devices: stopped and shown as not used.
	if !r.insts["127.0.0.2"].Down.Load() || r.insts["127.0.0.1"].Down.Load() {
		t.Fatal("want 127.0.0.1 up and 127.0.0.2 stopped")
	}
	r.fleet.Poll(ctx)
	if v, _ := r.fleet.View("127.0.0.2"); v.Status != fleet.StatusUnused || v.Detail != fleet.UnusedDetail {
		t.Fatalf("unused instance: %s %s", v.Status, v.Detail)
	}
	// Fan-out, the active split and drift leave it out.
	if res := r.fleet.ApplyDelay(ctx, 5, 6); len(res) != 1 || res[0].Instance != "127.0.0.1" || !res[0].OK() {
		t.Fatalf("fan-out: %+v", res)
	}
	if split, err := r.fleet.ActiveSplit(2); err != nil || len(split) != 1 || split["127.0.0.1"] != 2 {
		t.Fatalf("active split: %v %v", split, err)
	}
	if v, _ := r.fleet.View("127.0.0.2"); len(v.Drift) != 0 {
		t.Fatalf("drift on an unused instance: %v", v.Drift)
	}
	// A rolling restart skips it.
	r.resetCalls()
	must(t, r.wait(r.m.Restart()))
	if got := r.callsOf("restart"); strings.Join(got, ",") != "127.0.0.1" {
		t.Fatalf("restart: %v", got)
	}
	if !r.insts["127.0.0.2"].Down.Load() {
		t.Fatal("the restart started the unused instance")
	}

	// Stopped and started again, the build is reused.
	must(t, r.wait(r.m.Stop()))
	must(t, r.wait(r.m.Start("topology-mesh-campus", false)))
	if again := r.runner.Status(); again.Live.Name != st.Live.Name {
		t.Fatalf("second start installed %s, want %s reused", again.Live.Name, st.Live.Name)
	}
	entries, _ := os.ReadDir(filepath.Join(r.root, "builds"))
	if len(entries) != 2 {
		t.Fatalf("builds: %d, want B0 and the scenario", len(entries))
	}

	// The second scenario start kept the first snapshot, not the
	// scenario's own values.
	if snap := envOf(t, filepath.Join(r.data, "owned-keys", "127.0.0.1.json")); !strings.Contains(snap, `"PASSWORD": "hand-set"`) {
		t.Fatalf("snapshot overwritten by a second scenario start:\n%s", snap)
	}

	// A generated testbench: the pre-scenario values come back exactly
	// (keys that were not set are removed again), the snapshots go, the
	// new build keeps B0's credentials, and both instances run again.
	r.resetCalls()
	must(t, r.wait(r.m.Start(gen.Slug, true)))
	if !r.generated() {
		t.Fatal("the generated testbench's build was not generated")
	}
	if m := envOf(t, filepath.Join(r.root, "current", "manifest.csv")); !strings.Contains(m, ",ops,s3cret,en4ble,") {
		t.Fatalf("generated build after a scenario took the scenario's credentials:\n%s", m)
	}
	if env := envOf(t, r.insts["127.0.0.1"].EnvPath); !strings.Contains(env, "\nPASSWORD=hand-set\n") ||
		!strings.Contains(env, "\nCOMMANDS_ROOT=/srv/my-commands\n") || strings.Contains(env, "ENABLE_PASSWORD=") {
		t.Errorf("127.0.0.1 env after a generated start:\n%s", env)
	}
	if env := envOf(t, r.insts["127.0.0.2"].EnvPath); strings.Contains(env, "PASSWORD=") || strings.Contains(env, "COMMANDS_ROOT=") {
		t.Errorf("127.0.0.2 env after a generated start:\n%s", env)
	}
	if left, _ := filepath.Glob(filepath.Join(r.data, "owned-keys", "*.json")); len(left) != 0 {
		t.Errorf("snapshots left after the restore: %v", left)
	}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		if !strings.Contains(envOf(t, r.insts[ip].EnvPath), "\nALLOW_EMPTY=true\n") {
			t.Errorf("%s: Start did not write ALLOW_EMPTY=true (the unit default is false)", ip)
		}
		if r.insts[ip].Down.Load() {
			t.Errorf("%s not started for the generated testbench", ip)
		}
	}
	if !slices.Contains(r.callsOf("start"), "127.0.0.2") {
		t.Fatalf("127.0.0.2 not started: %v", r.callsOf("start"))
	}
	// A second generated start finds no snapshot and touches none of the
	// three keys.
	before := envOf(t, r.insts["127.0.0.1"].EnvPath)
	must(t, r.wait(r.m.Start(gen.Slug, true)))
	if after := envOf(t, r.insts["127.0.0.1"].EnvPath); after != before {
		t.Fatalf("generated-to-generated start changed the env file:\n%s\nto\n%s", before, after)
	}
}
