package web

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/inventory"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/runs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/testbench"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	shippedscenarios "github.com/rcfg-sim/rcfg-sim/scenarios"
)

type benchEnv struct {
	*env
	insts     map[string]*uitest.Instance
	runner    *jobs.Runner
	bench     *testbench.Manager
	data      string   // --data-dir
	scenarios string   // --scenarios-dir
	tests     []string // Test login calls: "host addr"
}

// newBenchEnv is two fake instances (127.0.0.1-2, 3 devices each) on an
// adopted data/ layout, with a systemctl that stops and starts them.
func newBenchEnv(t *testing.T) *benchEnv {
	t.Helper()
	tmp := t.TempDir()
	root, envDir, data := filepath.Join(tmp, "data"), filepath.Join(tmp, "etc"), filepath.Join(tmp, "ui")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	gen := filepath.Join(tmp, "gen")
	uitest.WriteFakeGen(t, gen)
	t.Setenv("FAKE_GEN_ARGS", filepath.Join(tmp, "gen-args"))
	b0 := filepath.Join(root, "builds", "B0")
	must(os.MkdirAll(filepath.Join(b0, "configs"), 0o755))
	man := "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n"
	for i := 0; i < 6; i++ {
		man += fmt.Sprintf("old-%d,127.0.0.%d,%d,Cisco,cisco_ios,admin,admin,enable123,%s/configs/d%d.cfg,sm\n", i, 1+i/3, 10000+i%3, b0, i)
	}
	must(os.WriteFile(filepath.Join(b0, "manifest.csv"), []byte(man), 0o644))
	must(os.WriteFile(filepath.Join(b0, "build.json"), []byte(`{"name":"B0","created":"2026-10-30T16:40:00Z","count":6,"seed":1,"distribution":"sm:100"}`), 0o644))
	must(os.Symlink("builds/B0", filepath.Join(root, "current")))
	must(os.MkdirAll(envDir, 0o755))

	be := &benchEnv{insts: map[string]*uitest.Instance{}}
	for _, ip := range []string{"127.0.0.1", "127.0.0.2"} {
		be.insts[ip] = uitest.Start(t, envDir, ip, uitest.Options{Devices: 3, ExtraEnv: "MANIFEST=" + filepath.Join(root, "current", "manifest.csv") + "\n"})
	}
	sysctl := &fakeSystemctl{}
	ctl := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := sysctl.run(ctx, name, args...)
		in := be.insts[strings.TrimSuffix(strings.TrimPrefix(args[len(args)-1], "rcfg-sim@"), ".service")]
		if in != nil {
			switch args[0] {
			case "stop":
				in.Down.Store(true)
			case "start", "restart":
				if err := in.LoadEnv(); err != nil {
					t.Errorf("%s: %v", in.Name, err)
				}
				in.Down.Store(false)
			}
		}
		return out, err
	}
	units := systemd.New("systemctl", ctl)
	fl := fleet.New(fleet.Options{EnvDir: envDir, Units: units})
	fl.Poll(context.Background())
	be.runner = jobs.NewRunner(jobs.Options{
		Catalogue: catalogue.NewSource(gen, nil),
		EnvDir:    envDir, GenBin: gen, LogDir: filepath.Join(tmp, "jobs"), Units: units,
		Healthy: fl.Healthy, Pause: time.Millisecond, Wait: 2 * time.Second,
		Free: func(string) (int64, error) { return 1 << 40, nil },
	})
	cat := catalogue.NewSource(gen, nil)
	store := testbench.NewStore(filepath.Join(data, "testbenches"), cat)
	be.scenarios = filepath.Join(tmp, "scenarios")
	store.UseScenarios(scenarios.NewLibrary(shippedscenarios.Shipped, be.scenarios, cat))
	rs := runs.NewStore(filepath.Join(data, "runs"), 0, nil)
	be.bench = testbench.NewManager(testbench.ManagerOptions{DataDir: data, Store: store, Runs: rs, Jobs: be.runner,
		Fleet: fl, Units: units, StartTimeout: 3 * time.Second, WaitEvery: 20 * time.Millisecond})
	auth := testAuth(t)
	be.data = data
	srv := New(Options{Fleet: fl, Units: units, Presets: presets.NewStore(filepath.Join(tmp, "presets")), Jobs: be.runner, Catalogue: cat,
		Bench: be.bench, Testbenches: store, Runs: rs, Auth: auth, Poll: time.Second, DataDir: data,
		TestLogin: func(_ context.Context, addr string, d inventory.Device, _ time.Duration) inventory.LoginResult {
			be.tests = append(be.tests, d.Hostname+" "+addr)
			if d.Hostname == "old-1" {
				return inventory.LoginResult{Stage: "ssh login", Command: "show version", Err: "ssh: unable to authenticate"}
			}
			return inventory.LoginResult{OK: true, Stage: "done", Command: "show version", Output: []string{"Cisco IOS Software"},
				LoginTime: 40 * time.Millisecond, Total: 90 * time.Millisecond}
		}})
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); hs.Close(); be.bench.Close(); be.runner.Close() })
	be.env = &env{t: t, dir: envDir, fleet: fl, sysctl: sysctl, srv: srv, http: hs, auth: auth, session: auth.NewSession()}
	return be
}

func (be *benchEnv) waitJob() {
	be.t.Helper()
	j := be.runner.Current()
	if j == nil {
		be.t.Fatal("no job")
	}
	select {
	case <-j.Done():
	case <-time.After(20 * time.Second):
		be.t.Fatal("job did not finish")
	}
	if j.Err() != nil {
		lines, _ := j.Lines(0)
		be.t.Fatalf("%s: %v\n%s", j.Kind, j.Err(), strings.Join(lines, "\n"))
	}
}

// redirect posts and returns the HX-Redirect target.
func (be *benchEnv) redirect(path string, form url.Values) string {
	be.t.Helper()
	resp, body := be.do(req{method: "POST", path: path, form: form, htmx: true})
	loc := resp.Header.Get("HX-Redirect")
	if resp.StatusCode != 200 || loc == "" {
		be.t.Fatalf("POST %s: %d, no HX-Redirect\n%s", path, resp.StatusCode, body)
	}
	return loc
}

// behaviourFields are the behaviour step's fields used by these tests.
func behaviourFields() url.Values {
	return url.Values{
		"duration": {""}, "f_auth_fail": {"10"}, "profile_seed": {"42"}, "rules": {"latency:100-200ms@50%"},
		"delay_min": {"0"}, "delay_max": {"20"}, "max_sessions": {"40"}, "active": {"all"},
		"ov_ip": {"127.0.0.2"}, "ov_faults": {"disconnect_mid=0.2"}, "ov_profile_seed": {""}, "ov_rules": {""},
		"ov_delay": {""}, "ov_max_sessions": {"7"}, "ov_active": {""},
	}
}

var draftField = regexp.MustCompile(`name="draft" value="([^"]*)"`)

// flowDriver walks the guided flow the way the browser does: each post
// carries the draft the last answer gave.
type flowDriver struct {
	be    *benchEnv
	draft string
	body  string
}

func (be *benchEnv) flowFrom(path string) *flowDriver {
	be.t.Helper()
	f := &flowDriver{be: be, body: be.get(path)}
	f.take()
	return f
}

func (f *flowDriver) take() {
	f.be.t.Helper()
	m := draftField.FindStringSubmatch(f.body)
	if m == nil {
		f.be.t.Fatalf("no draft in:\n%s", f.body)
	}
	f.draft = html.UnescapeString(m[1])
}

// step posts the fields of step at and asks for step to (-1: stay, as a
// preset button does).
func (f *flowDriver) step(at, to int, fields url.Values) string {
	f.be.t.Helper()
	v := url.Values{"draft": {f.draft}, "at": {strconv.Itoa(at)}}
	if to >= 0 {
		v.Set("goto", strconv.Itoa(to))
	}
	for k, vs := range fields {
		v[k] = vs
	}
	f.body = f.be.post("/testbenches/flow", v)
	f.take()
	return f.body
}

func (f *flowDriver) save(query string, fields url.Values) (*http.Response, string) {
	f.be.t.Helper()
	v := url.Values{"draft": {f.draft}, "at": {"3"}}
	for k, vs := range fields {
		v[k] = vs
	}
	return f.be.do(req{method: "POST", path: "/testbenches/save" + query, form: v, htmx: true})
}

// createBench makes an IP testbench of 6 sm devices through the flow.
func (be *benchEnv) createBench(name string, seed int) string {
	be.t.Helper()
	f := be.flowFrom("/testbenches/new")
	f.step(0, 1, url.Values{"kind": {"ip"}})
	f.step(1, 2, url.Values{"seed": {strconv.Itoa(seed)}, "count_sm": {"6"}})
	f.step(2, 3, behaviourFields())
	resp, body := f.save("", url.Values{"name": {name}, "description": {"web test"}})
	loc := resp.Header.Get("HX-Redirect")
	if loc == "" {
		be.t.Fatalf("save %s: no redirect\n%s", name, body)
	}
	return loc
}

func TestTestbenchScreens(t *testing.T) {
	be := newBenchEnv(t)

	// The list groups the shipped testbenches by kind; none fit this host.
	body := be.get("/testbenches")
	for _, want := range []string{"Enterprise IP", "Optical transport", "Service provider mix",
		"Scenario 5: full fleet", "Optical: Ciena GNE network", "Shipped", "doesn&#39;t fit this host", "need more IP aliases",
		"New testbench", "Import a testbench"} {
		if !strings.Contains(body, want) {
			t.Errorf("list missing %q", want)
		}
	}

	if strings.Contains(body, "Custom") || strings.Contains(body, "Needs attention") {
		t.Error("list has a Custom or Needs attention group with nothing to put in it")
	}

	if loc := be.createBench("Web A", 1); loc != "/testbenches/web-a" {
		t.Fatalf("created at %s", loc)
	}
	// Yours sit in their kind's group beside the shipped ones; the filter
	// shows one or the other. A file that no longer validates is listed
	// in Needs attention with its reason, and has no Start.
	if err := os.WriteFile(filepath.Join(be.data, "testbenches", "broken.json"), []byte(`{"name":"Broken","bogus":1}`), 0o640); err != nil {
		t.Fatal(err)
	}
	body = be.get("/testbenches")
	ip := body[strings.Index(body, `aria-label="Enterprise IP"`):strings.Index(body, `aria-label="Optical transport"`)]
	if !strings.Contains(ip, "Web A") || !strings.Contains(ip, "Yours") || !strings.Contains(ip, "Scenario 5: full fleet") {
		t.Errorf("Enterprise IP group:\n%s", ip)
	}
	att := body[strings.Index(body, `aria-label="Needs attention"`):]
	if !strings.Contains(att, "Broken") || !strings.Contains(att, "unknown field") || strings.Contains(att, "/testbenches/broken/start") {
		t.Errorf("Needs attention group:\n%s", att)
	}
	if yours := be.get("/testbenches?show=yours"); strings.Contains(yours, "Scenario 5: full fleet") || !strings.Contains(yours, "Web A") {
		t.Error("show=yours lists shipped testbenches or misses yours")
	}
	if shipped := be.get("/testbenches?show=shipped"); strings.Contains(shipped, "Web A") || !strings.Contains(shipped, "Scenario 5: full fleet") {
		t.Error("show=shipped lists yours or misses shipped")
	}
	if err := os.Remove(filepath.Join(be.data, "testbenches", "broken.json")); err != nil {
		t.Fatal(err)
	}
	// Editing opens the flow at the devices step, kind fixed.
	edit := be.get("/testbenches/web-a")
	for _, want := range []string{"Web A", "v1", `name="count_sm" value="6"`, "Enterprise IP devices"} {
		if !strings.Contains(edit, want) {
			t.Errorf("editor missing %q", want)
		}
	}
	if strings.Contains(edit, "Optical transport devices") {
		t.Error("an IP testbench shows the optical panel")
	}
	// Save (through the flow again) bumps the version; a bad field is refused.
	f := be.flowFrom("/testbenches/web-a")
	f.step(1, 2, url.Values{"seed": {"1"}, "count_sm": {"6"}})
	bad := behaviourFields()
	bad.Set("delay_max", "x")
	if body := f.step(2, 3, bad); !strings.Contains(body, "delay max must be a whole number") {
		t.Fatalf("bad behaviour: %s", body)
	}
	f.step(2, 3, behaviourFields())
	if resp, body := f.save("", url.Values{"name": {"Web A"}, "description": {"web test"}}); resp.Header.Get("HX-Redirect") != "/testbenches/web-a" {
		t.Fatalf("save: %s", body)
	}
	if !strings.Contains(be.get("/testbenches/web-a"), "v2") {
		t.Fatal("save did not bump the version")
	}

	// Export, then import it under another name.
	exp := be.get("/testbenches/web-a/export")
	if !strings.Contains(exp, `"version": 2`) || !strings.Contains(exp, `"127.0.0.2"`) || !strings.Contains(exp, `"source": "generated"`) {
		t.Fatalf("export:\n%s", exp)
	}
	var mp bytes.Buffer
	mw := multipart.NewWriter(&mp)
	fw, _ := mw.CreateFormFile("file", "tb.json")
	_, _ = fw.Write([]byte(strings.Replace(exp, `"Web A"`, `"Web B"`, 1)))
	_ = mw.Close()
	hr, _ := http.NewRequest("POST", be.http.URL+"/testbenches/import", &mp)
	hr.Header.Set("Content-Type", mw.FormDataContentType())
	hr.Header.Set("HX-Request", "true")
	resp, err := be.client().Do(hr)
	if err != nil || resp.Header.Get("HX-Redirect") != "/testbenches/web-b" {
		t.Fatalf("import: %v %v", err, resp.Header)
	}
	resp.Body.Close()
	be.post("/testbenches/web-b/duplicate", nil)
	if !strings.Contains(be.get("/testbenches"), "Web B (copy)") {
		t.Fatal("duplicate")
	}

	// Start A: a job; the pill shows it running, and its name opens the run.
	if body := be.post("/testbenches/web-a/start", nil); !strings.Contains(body, "/build/jobs/") {
		t.Fatalf("start: %s", body)
	}
	be.waitJob()
	runID := be.bench.Active().RunID
	pill := be.get("/bench/pill")
	if !strings.Contains(pill, "Web A · running") || !strings.Contains(pill, `href="/runs/`+runID+`"`) || !strings.Contains(pill, `href="/devices?run=`+runID+`"`) {
		t.Fatalf("pill: %s", pill)
	}
	if !strings.Contains(be.get("/fleet"), "Web A · running") {
		t.Fatal("pill not in the header")
	}
	if be.insts["127.0.0.2"].Live().MaxSessions != 7 || be.insts["127.0.0.1"].Live().MaxSessions != 40 {
		t.Fatal("override not applied to its instance only")
	}

	// A change on the Fleet screen marks it modified; Save to testbench.
	be.fleet.Poll(context.Background())
	be.post("/fleet/caps", url.Values{"cap_127.0.0.1": {"40"}, "cap_127.0.0.2": {"9"}})
	pill = be.get("/bench/pill")
	if !strings.Contains(pill, "modified") || !strings.Contains(pill, "Save to testbench") {
		t.Fatalf("pill after a change: %s", pill)
	}
	if body := be.post("/bench/save", nil); !strings.Contains(body, "version 3") {
		t.Fatalf("save to testbench: %s", body)
	}
	if strings.Contains(be.get("/bench/pill"), "modified") {
		t.Fatal("still modified after save")
	}

	// Starting B while A runs asks first.
	if body := be.post("/testbenches/web-b/start", nil); !strings.Contains(body, "Stop Web A and start Web B") {
		t.Fatalf("no confirm: %s", body)
	}
	be.post("/testbenches/web-b/start?replace=1", nil)
	be.waitJob()
	be.post("/bench/stop", nil)
	be.waitJob()
	if strings.Contains(be.get("/bench/pill"), "running") {
		t.Fatal("still running after stop")
	}

	// Runs: two, with detail, exports and compare.
	list := be.get("/runs")
	ids := regexp.MustCompile(`href="/runs/([0-9-]+)"`).FindAllStringSubmatch(list, -1)
	if len(ids) != 2 || !strings.Contains(list, "stopped") {
		t.Fatalf("runs list: %d\n%s", len(ids), list)
	}
	a, b := ids[1][1], ids[0][1]
	detail := be.get("/runs/" + a)
	for _, want := range []string{"Web A", "Timeline", "Session caps 127.0.0.2=9", "Saved to testbench", "By instance", "127.0.0.2"} {
		if !strings.Contains(detail, want) {
			t.Errorf("run detail missing %q", want)
		}
	}
	if csv := be.get("/runs/" + a + "/export?format=csv"); !strings.HasPrefix(csv, "run,testbench,version,instance,sessions_ok") {
		t.Fatalf("csv: %s", csv)
	}
	if js := be.get("/runs/" + a + "/export?format=json"); !strings.Contains(js, `"outcome": "stopped"`) {
		t.Fatalf("json: %s", js)
	}
	cmp := be.get("/runs/compare?id=" + a + "&id=" + b)
	if !strings.Contains(cmp, "Settings that differ") || !strings.Contains(cmp, "name") {
		t.Fatalf("compare: %s", cmp)
	}

	// Delete B; shipped ones cannot be.
	if loc := be.redirect("/testbenches/web-b/delete", nil); loc != "/testbenches" {
		t.Fatal(loc)
	}
	if body := be.post("/testbenches/s5-full-fleet/delete", nil); !strings.Contains(body, "cannot be deleted") {
		t.Fatalf("deleted a shipped testbench: %s", body)
	}
}

// TestGuidedFlowKinds covers each kind's devices step: only the panels
// that apply, the SSH login choices and its restart warning, presets, and
// the review step's summary.
func TestGuidedFlowKinds(t *testing.T) {
	be := newBenchEnv(t)

	// Step 1: the kinds as choice cards, Topology among them.
	f := be.flowFrom("/testbenches/new")
	for _, want := range []string{"Enterprise IP fleet", "Optical transport (TL1)", "Service provider mix", "Topology scenario",
		"Start from a shipped testbench", `class="stepper"`} {
		if !strings.Contains(f.body, want) {
			t.Errorf("step 1 missing %q", want)
		}
	}
	if strings.Contains(f.body, "Phase 9") {
		t.Error("the Topology card is still marked as coming")
	}

	// Optical: the optical panel only, its models from the catalogue, the
	// SSH login in plain words, and the dual-homed RNE option.
	body := f.step(0, 1, url.Values{"kind": {"optical"}})
	for _, want := range []string{"Optical transport devices", "count_ciena-6500-tl1-gne", "count_cisco-ons15454-tl1",
		"Login inside TL1 only, like real 6500s", "SSH password first, then TL1 login", "No SSH password at all",
		"restart needed", "Dual-homed remote NEs", "opt_rne_dual_home_pct", "shared RNE"} {
		if !strings.Contains(body, want) {
			t.Errorf("optical step missing %q", want)
		}
	}
	for _, not := range []string{"Enterprise IP devices", "count_sm", "Typical enterprise"} {
		if strings.Contains(body, not) {
			t.Errorf("optical step shows IP lever %q", not)
		}
	}
	// Back to kind, then the mix: both panels, separate subtotals, no "none".
	f.step(1, 0, url.Values{"seed": {"42"}, "count_ciena-6500-tl1-gne": {"6"}, "ssh_auth": {"driver"}, "opt_rne_dual_home_pct": {"30"}})
	body = f.step(0, 1, url.Values{"kind": {"mix"}})
	for _, want := range []string{"Enterprise IP devices", "Optical transport devices", "IP subtotal", "Optical subtotal"} {
		if !strings.Contains(body, want) {
			t.Errorf("mix step missing %q", want)
		}
	}
	if strings.Contains(body, "No SSH password at all") {
		t.Error("mix offers SSH login none")
	}
	// A preset fills the IP counts from the IP subtotal.
	body = f.step(1, -1, url.Values{"seed": {"42"}, "count_sm": {"10"}, "count_ciena-6500-tl1-gne": {"0"}, "ssh_auth": {"driver"},
		"opt_rne_dual_home_pct": {"0"}, "preset": {"heavy"}})
	if !strings.Contains(body, `name="count_xl" value="4"`) || !strings.Contains(body, `name="count_sm" value="1"`) {
		t.Fatal("heavy preset did not set the IP counts")
	}
	// Six devices (4 IP, 2 optical) fit this host of 2 x 3.
	f.step(1, 2, url.Values{"seed": {"42"}, "count_sm": {"4"}, "count_ciena-6500-tl1-gne": {"2"}, "ssh_auth": {"driver"}, "opt_rne_dual_home_pct": {"30"}})
	body = f.step(2, 3, behaviourFields())
	for _, want := range []string{"6 devices on 2 instances", "Generates a new build", `Sets the SSH login to &#34;driver&#34;`, "Save and start"} {
		if !strings.Contains(body, want) {
			t.Errorf("review missing %q", want)
		}
	}
	resp, body := f.save("?as=1&start=1", url.Values{"name": {"SP lab"}, "description": {"mix"}})
	if resp.Header.Get("HX-Redirect") != "/testbenches" {
		t.Fatalf("save and start: %s", body)
	}
	be.waitJob()
	if a := be.bench.Active(); a.Name != "SP lab" {
		t.Fatalf("active %+v", a)
	}
	for _, in := range be.insts {
		if env, _ := os.ReadFile(in.EnvPath); !strings.Contains(string(env), "SSH_AUTH=driver\n") {
			t.Fatalf("%s env:\n%s", in.Name, env)
		}
	}
	exp := be.get("/testbenches/sp-lab/export")
	if !strings.Contains(exp, `"ssh_auth": "driver"`) || !strings.Contains(exp, `"rne_dual_home_pct": 30`) {
		t.Fatalf("export:\n%s", exp)
	}

	// Start from a shipped testbench: an editable copy.
	f = be.flowFrom("/testbenches/new")
	body = f.step(0, 0, url.Values{"kind": {"shipped"}})
	if !strings.Contains(body, "Optical: three-vendor mix") {
		t.Fatal("no shipped choices")
	}
	body = f.step(0, 1, url.Values{"kind": {"shipped"}, "from": {"optical-three-vendor"}})
	if !strings.Contains(body, `name="count_infinera-dtnx-tl1" value="1500"`) || strings.Contains(body, "Enterprise IP devices") {
		t.Fatal("shipped copy's devices step")
	}
	// Nothing on the devices step: refused.
	if body := f.step(1, 2, url.Values{"seed": {"1"}, "ssh_auth": {"driver"}, "opt_rne_dual_home_pct": {"0"}}); !strings.Contains(body, "Add at least one device") {
		t.Fatal("empty fleet accepted")
	}
}
func TestTestbenchRoutesNeedLogin(t *testing.T) {
	be := newBenchEnv(t)
	for _, p := range []string{"/testbenches", "/runs", "/bench/pill"} {
		resp, _ := be.do(req{method: "GET", path: p, anon: true})
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s without a session: %d", p, resp.StatusCode)
		}
	}
	resp, _ := be.do(req{method: "POST", path: "/bench/stop", anon: true, htmx: true})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST without a session: %d", resp.StatusCode)
	}
	resp, _ = be.do(req{method: "POST", path: "/bench/stop"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without HX-Request: %d", resp.StatusCode)
	}
}
