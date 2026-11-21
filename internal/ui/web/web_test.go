package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// fakeSystemctl records systemctl calls; is-active answers "inactive".
type fakeSystemctl struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSystemctl) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "is-active" {
		return []byte("inactive\n"), nil
	}
	return nil, nil
}

func (f *fakeSystemctl) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type env struct {
	t       *testing.T
	dir     string
	insts   []*uitest.Instance
	fleet   *fleet.Fleet
	sysctl  *fakeSystemctl
	srv     *Server
	http    *httptest.Server
	presets string
	auth    *Auth
	session string // a logged-in session, sent by default
}

// testPassword is the test user's password; testAuth hashes it at the
// lowest bcrypt cost to keep tests fast.
const testPassword = "correct horse"

func testAuth(t *testing.T) *Auth {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth(AuthOptions{Username: "admin", PasswordHash: string(h), FailDelay: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// client is an HTTP client carrying the env's session cookie.
func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(e.http.URL)
	jar.SetCookies(u, []*http.Cookie{{Name: SessionCookie, Value: e.session}})
	return &http.Client{Jar: jar}
}

// newEnv starts n fake instances and a UI over them.
func newEnv(t *testing.T, n int) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), sysctl: &fakeSystemctl{}}
	for i := 1; i <= n; i++ {
		e.insts = append(e.insts, uitest.Start(t, e.dir, "127.0.0."+string(rune('0'+i)), uitest.Options{Devices: 20}))
	}
	uitest.WriteManifest(t, e.dir, nil, e.insts...)
	units := systemd.New("systemctl", e.sysctl.run)
	e.fleet = fleet.New(fleet.Options{EnvDir: e.dir, Units: units})
	e.fleet.Poll(context.Background())
	e.presets = filepath.Join(t.TempDir(), "presets")
	e.auth = testAuth(t)
	e.session = e.auth.NewSession()
	e.srv = New(Options{Fleet: e.fleet, Units: units, Presets: presets.NewStore(e.presets), Auth: e.auth, Catalogue: testCatalogue(t),
		Poll: time.Second, RollingWait: 3 * time.Second, FrameEvery: 100 * time.Millisecond})
	e.http = httptest.NewServer(e.srv)
	// The UI server first: it ends open Activity streams, which the HTTP
	// server's Close would otherwise wait for.
	t.Cleanup(func() { e.srv.Close(); e.http.Close() })
	return e
}

type req struct {
	method, path string
	form         url.Values
	htmx         bool
	anon         bool              // send no session
	session      string            // send this session instead of the env's
	header       map[string]string // extra headers
}

func (e *env) do(r req) (*http.Response, string) {
	e.t.Helper()
	var body io.Reader
	if r.form != nil {
		body = strings.NewReader(r.form.Encode())
	}
	hr, _ := http.NewRequest(r.method, e.http.URL+r.path, body)
	if r.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if r.htmx {
		hr.Header.Set("HX-Request", "true")
	}
	if !r.anon {
		id := e.session
		if r.session != "" {
			id = r.session
		}
		hr.AddCookie(&http.Cookie{Name: SessionCookie, Value: id})
	}
	for k, v := range r.header {
		if k == "Host" {
			hr.Host = v
			continue
		}
		hr.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(hr)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (e *env) get(path string) string {
	e.t.Helper()
	resp, body := e.do(req{method: "GET", path: path})
	if resp.StatusCode != 200 {
		e.t.Fatalf("GET %s: %d\n%s", path, resp.StatusCode, body)
	}
	return body
}

func (e *env) post(path string, form url.Values) string {
	e.t.Helper()
	resp, body := e.do(req{method: "POST", path: path, form: form, htmx: true})
	if resp.StatusCode != 200 {
		e.t.Fatalf("POST %s: %d\n%s", path, resp.StatusCode, body)
	}
	return body
}

// failuresForm is the Failures form with the given percentages and rules.
func failuresForm(pcts map[string]string, seed string, rules ...[3]string) url.Values {
	f := url.Values{"seed": {seed}}
	for _, ft := range views.FaultTypes {
		f.Set("pct_"+ft.Type, pcts[ft.Type])
	}
	for _, r := range rules {
		f.Add("rule_kind", r[0])
		f.Add("rule_param", r[1])
		f.Add("rule_pct", r[2])
	}
	return f
}

func TestCSRFRequiresHXRequest(t *testing.T) {
	e := newEnv(t, 1)
	resp, _ := e.do(req{method: "POST", path: "/fleet/delay", form: url.Values{"delay_min": {"0"}, "delay_max": {"1"}}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without HX-Request: %d", resp.StatusCode)
	}
	if e.insts[0].Live().DelayMax != 500*time.Millisecond {
		t.Fatal("a refused request changed the instance")
	}
}

// TestPagesAreSelfContained: strict CSP, and no asset or link off this host.
func TestPagesAreSelfContained(t *testing.T) {
	e := newEnv(t, 2)
	for _, p := range []string{"/fleet", "/failures", "/activity"} {
		resp, body := e.do(req{method: "GET", path: p})
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: CSP %q", p, csp)
		}
		if m := regexp.MustCompile(`(?i)(src|href)="(https?:)?//`).FindString(body); m != "" {
			t.Errorf("%s: external reference %q", p, m)
		}
		if strings.Contains(body, "<script>") || regexp.MustCompile(`\son[a-z]+=`).MatchString(body) {
			t.Errorf("%s: inline script found (CSP forbids it)", p)
		}
	}
	for _, asset := range []string{"/static/app.css", "/static/app.js", "/static/theme.js", "/static/vendor/htmx-2.0.11.min.js",
		"/static/img/sim-logo-white.svg", "/static/img/sim-logo-gradient.svg", "/static/img/favicon.svg",
		"/static/fonts/sora-latin-wght-normal.woff2", "/static/fonts/plus-jakarta-sans-latin-wght-normal.woff2"} {
		if resp, _ := e.do(req{method: "GET", path: asset}); resp.StatusCode != 200 {
			t.Errorf("%s: %d", asset, resp.StatusCode)
		}
	}
	if resp, _ := e.do(req{method: "GET", path: "/static/src/app.css"}); resp.StatusCode != 404 {
		t.Error("the CSS source must not be served")
	}
	if resp, _ := e.do(req{method: "GET", path: "/static/"}); resp.StatusCode != 404 {
		t.Error("no directory listing")
	}
}

func TestFleetPageAndCards(t *testing.T) {
	e := newEnv(t, 3)
	e.insts[1].Down.Store(true)
	e.insts[2].AdminOff.Store(true)
	e.fleet.Poll(context.Background())

	body := e.get("/fleet")
	for _, want := range []string{"127.0.0.1", "127.0.0.2", "127.0.0.3", ">Up<", ">Down<", ">Admin API off<",
		"1 / 3 instances up", "ADMIN_ADDR", "<span class=\"num font-semibold\">inactive</span>", `hx-get="/fleet/cards"`} {
		if !strings.Contains(body, want) {
			t.Errorf("fleet page missing %q", want)
		}
	}
	cards := e.get("/fleet/cards")
	if !strings.Contains(cards, `id="health" hx-swap-oob="true"`) || !strings.Contains(cards, "every 1000ms") {
		t.Errorf("cards fragment must poll and update the health pill out of band:\n%s", cards)
	}
}

func TestFailuresApplyFansOutAndPersists(t *testing.T) {
	e := newEnv(t, 3)
	form := failuresForm(map[string]string{"auth_fail": "2", "disconnect_mid": "0.5"}, "7", [3]string{"hang", "3600s", "3"}, [3]string{"refuse", "", "1.5"})
	body := e.post("/failures/apply", form)
	if !strings.Contains(body, "applied to 3 of 3 instances") || !strings.Contains(body, `id="pending" hx-swap-oob`) {
		t.Fatalf("flash: %s", body)
	}
	for _, in := range e.insts {
		l := in.Live()
		if l.Faults.Rate(fault.TypeAuthFail) != 0.02 || l.ProfileSeed != 7 || !slices.Equal(l.ProfileRules, []string{"hang:3600s@3%", "refuse@1.5%"}) {
			t.Errorf("%s live: faults %v seed %d rules %v", in.Name, l.Faults, l.ProfileSeed, l.ProfileRules)
		}
		f, _ := envfile.Read(in.EnvPath)
		for k, want := range map[string]string{"FAULT_RATES": "auth_fail=0.02,disconnect_mid=0.005", "DEVICE_PROFILES": "hang:3600s@3%,refuse@1.5%", "PROFILE_SEED": "7"} {
			if got, _ := f.Get(k); got != want {
				t.Errorf("%s %s: %q, want %q", in.Name, k, got, want)
			}
		}
	}

	// Validation stays server-side.
	for _, bad := range []url.Values{
		failuresForm(map[string]string{"auth_fail": "150"}, "1"),
		failuresForm(nil, "x"),
		failuresForm(nil, "1", [3]string{"refuse", "", "60"}, [3]string{"disconnect", "", "60"}),
		failuresForm(nil, "1", [3]string{"explode", "", "1"}),
		failuresForm(nil, "1", [3]string{"latency", "200-800ms,refuse", "1"}),
	} {
		if body := e.post("/failures/apply", bad); !strings.Contains(body, "toast-error") {
			t.Errorf("want an error flash for %v, got %s", bad, body)
		}
	}
}

func TestPendingFailures(t *testing.T) {
	e := newEnv(t, 1)
	if body := e.post("/pending/failures", failuresForm(nil, "42")); strings.Contains(body, "Pending changes") {
		t.Fatalf("unchanged form must show no pending bar: %s", body)
	}
	body := e.post("/pending/failures", failuresForm(map[string]string{"malformed": "1"}, "9", [3]string{"refuse", "", "2"}))
	for _, want := range []string{"Pending changes", "malformed", "0%", "1%", "Profiles", "refuse@2%", "Seed", `hx-post="/failures/apply"`, `hx-include="#failures-form"`, "brand-fill"} {
		if !strings.Contains(body, want) {
			t.Errorf("pending bar missing %q:\n%s", want, body)
		}
	}
	if e.insts[0].Live().Generation != 1 {
		t.Fatal("pending must not change anything")
	}
}

func TestUnitActions(t *testing.T) {
	e := newEnv(t, 2)
	body := e.post("/fleet/instances/127.0.0.1/stop", url.Values{})
	if !strings.Contains(body, "127.0.0.1: stop done") {
		t.Fatalf("flash: %s", body)
	}
	if !slices.Contains(e.sysctl.Calls(), "systemctl stop rcfg-sim@127.0.0.1.service") {
		t.Fatalf("calls: %v", e.sysctl.Calls())
	}
	before := len(e.sysctl.Calls())
	if body := e.post("/fleet/instances/10.9.9.9/start", url.Values{}); !strings.Contains(body, "No instance") {
		t.Fatalf("unknown instance: %s", body)
	}
	if body := e.post("/fleet/all/enable", url.Values{}); !strings.Contains(body, "Unsupported") {
		t.Fatalf("bad verb: %s", body)
	}
	if len(e.sysctl.Calls()) != before {
		t.Fatal("refused actions must not run systemctl")
	}
	body = e.post("/fleet/all/restart", url.Values{})
	if !strings.Contains(body, "Restart applied to 2 of 2") {
		t.Fatalf("restart all: %s", body)
	}
}

func TestRollingRestart(t *testing.T) {
	e := newEnv(t, 2)
	body := e.post("/fleet/rolling-restart", url.Values{"pause": {"0"}})
	if !strings.Contains(body, "Rolling restart of 2 instances started") {
		t.Fatalf("start: %s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rv := e.srv.rollingView()
		if rv.Done {
			if rv.Err != "" || len(rv.Steps) != 6 {
				t.Fatalf("rolling: %+v", rv)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rolling restart did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var restarts []string
	for _, c := range e.sysctl.Calls() {
		if strings.Contains(c, "restart") {
			restarts = append(restarts, c)
		}
	}
	if !slices.Equal(restarts, []string{"systemctl restart rcfg-sim@127.0.0.1.service", "systemctl restart rcfg-sim@127.0.0.2.service"}) {
		t.Fatalf("restart order: %v", restarts)
	}
	if body := e.post("/fleet/rolling-restart", url.Values{"pause": {"-1"}}); !strings.Contains(body, "Pause must be") {
		t.Fatalf("bad pause: %s", body)
	}
}

func TestFleetControls(t *testing.T) {
	e := newEnv(t, 2)
	if body := e.post("/fleet/delay", url.Values{"delay_min": {"5"}, "delay_max": {"15"}}); !strings.Contains(body, "applied to 2 of 2") {
		t.Fatalf("delay: %s", body)
	}
	if e.insts[1].Live().DelayMax != 15*time.Millisecond {
		t.Fatal("delay not applied")
	}
	if body := e.post("/fleet/delay", url.Values{"delay_min": {"20"}, "delay_max": {"10"}}); !strings.Contains(body, "min &lt;= max") {
		t.Fatalf("bad delay: %s", body)
	}

	body := e.post("/pending/active", url.Values{"active": {"30"}})
	if !strings.Contains(body, "Split: 127.0.0.1 15, 127.0.0.2 15") {
		t.Fatalf("pending active: %s", body)
	}
	if body := e.post("/fleet/active", url.Values{"active": {"30"}}); !strings.Contains(body, "applied to 2 of 2") {
		t.Fatalf("active: %s", body)
	}
	if e.insts[0].Live().ActiveDevices+e.insts[1].Live().ActiveDevices != 30 {
		t.Fatal("active split not applied")
	}
	if body := e.post("/fleet/active", url.Values{"active": {"41"}}); !strings.Contains(body, "0 to 40") {
		t.Fatalf("active over max: %s", body)
	}

	if body := e.post("/fleet/caps", url.Values{"cap_127.0.0.1": {"5000"}, "cap_127.0.0.2": {"77"}}); !strings.Contains(body, "Session caps applied to 1 of 1") {
		t.Fatalf("caps: %s", body)
	}
	f, _ := envfile.Read(e.insts[1].EnvPath)
	if v, _ := f.Get("MAX_CONCURRENT_SESSIONS"); v != "77" {
		t.Fatalf("cap env: %q", v)
	}
}

func TestDriftShownAndReapplied(t *testing.T) {
	e := newEnv(t, 1)
	in := e.insts[0]
	b, _ := os.ReadFile(in.EnvPath)
	_ = os.WriteFile(in.EnvPath, append(b, []byte("FAULT_RATES=auth_fail=0.5\n")...), 0o660)
	e.fleet.Poll(context.Background())
	body := e.get("/fleet/cards")
	if !strings.Contains(body, "Drift") || !strings.Contains(body, "faults") || !strings.Contains(body, "/fleet/instances/127.0.0.1/reapply") {
		t.Fatalf("drift badge missing:\n%s", body)
	}
	if body := e.post("/fleet/instances/127.0.0.1/reapply", url.Values{}); !strings.Contains(body, "applied to 1 of 1") {
		t.Fatalf("reapply: %s", body)
	}
	if in.Live().Faults.Rate(fault.TypeAuthFail) != 0.5 {
		t.Fatal("env settings not pushed")
	}
	if body := e.get("/fleet/cards"); strings.Contains(body, "Drift") {
		t.Fatal("drift still shown after re-apply")
	}
}

func TestAffectedPanel(t *testing.T) {
	e := newEnv(t, 2)
	body := e.post("/failures/affected", failuresForm(nil, "42", [3]string{"refuse", "", "50"}))
	if !strings.Contains(body, "devices affected") || !strings.Contains(body, `data-copy="affected-list"`) || !strings.Contains(body, "rtr-127.0.0.") {
		t.Fatalf("affected: %s", body)
	}
	if body := e.post("/failures/affected", failuresForm(nil, "42", [3]string{"refuse", "", "500"})); !strings.Contains(body, "text-st-error") {
		t.Fatalf("bad rule must show an error: %s", body)
	}
}

func TestPresets(t *testing.T) {
	e := newEnv(t, 2)
	page := e.get("/failures")
	for _, name := range []string{"Flaky WAN", "Auth storm", "Slow core"} {
		if !strings.Contains(page, name) {
			t.Errorf("built-in preset %s not listed", name)
		}
	}
	form := failuresForm(map[string]string{"slow_response": "4"}, "3", [3]string{"latency", "100-200ms", "5"})
	form.Set("preset_name", "My lab")
	resp, body := e.do(req{method: "POST", path: "/presets/save", form: form, htmx: true})
	if !strings.Contains(body, "Preset My lab saved") || resp.Header.Get("HX-Trigger") != "presets-changed" {
		t.Fatalf("save: %s", body)
	}
	if _, err := os.Stat(filepath.Join(e.presets, "my-lab.json")); err != nil {
		t.Fatal(err)
	}
	if body := e.get("/failures?preset=My+lab"); !strings.Contains(body, "Preset My lab loaded into the form") || !strings.Contains(body, `value="100-200ms"`) {
		t.Fatalf("load preset into form failed")
	}
	if body := e.post("/presets/apply", url.Values{"name": {"Auth storm"}}); !strings.Contains(body, "applied to 2 of 2") {
		t.Fatalf("apply preset: %s", body)
	}
	if e.insts[1].Live().Faults.Rate(fault.TypeAuthFail) != 0.15 {
		t.Fatal("preset not applied")
	}
	if body := e.post("/presets/delete", url.Values{"name": {"Auth storm"}}); !strings.Contains(body, "built-in") {
		t.Fatalf("delete built-in: %s", body)
	}
	if body := e.post("/presets/delete", url.Values{"name": {"My lab"}}); !strings.Contains(body, "deleted") {
		t.Fatalf("delete: %s", body)
	}
	if resp, _ := e.do(req{method: "GET", path: "/failures?preset=nope"}); resp.StatusCode != 404 {
		t.Fatalf("unknown preset: %d", resp.StatusCode)
	}
}

// Template render tests: key elements and attributes, rendered to a buffer.

func renderString(t *testing.T, c interface {
	Render(context.Context, io.Writer) error
}) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestLayoutRender(t *testing.T) {
	out := renderString(t, views.FleetPage(views.Page{Title: "Fleet", Nav: "fleet", Up: 2, Total: 3}, views.FleetModel{Poll: "2000ms"}))
	for _, want := range []string{
		`<html lang="en" data-theme="dark">`,
		`"includeIndicatorStyles":false`, `"allowEval":false`,
		`src="/static/vendor/htmx-2.0.11.min.js"`, `src="/static/theme.js"`,
		`class="brand-hairline`, `id="pending"`, `id="flash"`,
		`data-theme-set="dark"`, `data-theme-set="light"`, `data-theme-set="auto"`,
		`class="logo-dark`, `class="logo-light`, `aria-current="page"`, "2 / 3 instances up",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("layout missing %q", want)
		}
	}
}

func TestComponentRender(t *testing.T) {
	bar := renderString(t, views.PendingBar(views.Diff{ApplyURL: "/fleet/delay", Form: "form-delay", Items: []views.DiffItem{{What: "Response delay", From: "1", To: "2"}}}))
	if !strings.Contains(bar, `hx-post="/fleet/delay"`) || !strings.Contains(bar, `hx-include="#form-delay"`) || !strings.Contains(bar, `hx-target="#flash"`) {
		t.Errorf("pending bar: %s", bar)
	}
	if empty := renderString(t, views.PendingBar(views.Diff{})); strings.TrimSpace(empty) != "" {
		t.Errorf("empty diff must render nothing: %q", empty)
	}
	row := renderString(t, views.RuleRowView(views.RuleRow{Kind: "latency", Param: "200-800ms", Pct: "10"}))
	for _, want := range []string{`name="rule_kind"`, `<option value="latency" selected>`, `value="200-800ms"`, `name="rule_pct"`, "data-remove-row"} {
		if !strings.Contains(row, want) {
			t.Errorf("rule row missing %q: %s", want, row)
		}
	}
	page := renderString(t, views.FailuresPage(views.Page{}, views.FailuresModel{Faults: views.FaultTypes, Devices: 1000}))
	for _, want := range []string{`id="failures-form"`, `hx-post="/failures/apply"`, `hx-post="/pending/failures"`,
		`hx-trigger="input changed delay:250ms from:#failures-form"`, `name="pct_auth_fail"`, `data-sync="pct-auth_fail"`,
		`data-estimate="pct-malformed"`, `data-default-max="10"`, `hx-post="/failures/affected"`} {
		if !strings.Contains(page, want) {
			t.Errorf("failures page missing %q", want)
		}
	}
}

// TestApplyToDownInstanceSavesForNextStart (section 5.4): a down instance's
// env file is written and the result says it applies on next start.
func TestApplyToDownInstanceSavesForNextStart(t *testing.T) {
	e := newEnv(t, 2)
	e.insts[1].Down.Store(true)
	e.fleet.Poll(context.Background())
	body := e.post("/failures/apply", failuresForm(map[string]string{"auth_fail": "3"}, "42"))
	if !strings.Contains(body, "applied to 1 of 2 instances; 1 more on next start") || !strings.Contains(body, "127.0.0.2: down; saved to its env file, applies on next start") {
		t.Fatalf("flash: %s", body)
	}
	f, _ := envfile.Read(e.insts[1].EnvPath)
	if v, _ := f.Get("FAULT_RATES"); v != "auth_fail=0.03" {
		t.Fatalf("down instance env: %q", v)
	}
}

// testCatalogue is the real generator's catalogue.
func testCatalogue(t testing.TB) *catalogue.Source {
	t.Helper()
	c, err := catalogue.Decode(uitest.CatalogueJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	return catalogue.Static(c)
}
