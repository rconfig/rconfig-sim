package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

func (be *benchEnv) basic(path, user, pass string) *http.Response {
	be.t.Helper()
	hr, _ := http.NewRequest("GET", be.http.URL+path, nil)
	hr.SetBasicAuth(user, pass)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(hr)
	if err != nil {
		be.t.Fatal(err)
	}
	return resp
}

func (be *benchEnv) poll() { be.fleet.Poll(context.Background()) }

func TestDevicesStatusesAndFilters(t *testing.T) {
	be := newBenchEnv(t)
	body := be.get("/devices")
	for _, want := range []string{"live build B0", "old-0", "old-5", "active <span class=\"text-fg\">6</span>", "Export CSV", "sm <span class=\"text-fg\">6</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("devices page missing %q", want)
		}
	}
	// Families come from the catalogue: a badge per row, a count and a filter.
	if !strings.Contains(body, `href="/devices?family=ip"`) || !strings.Contains(body, `<span class="badge badge-family">IP</span>`) ||
		!strings.Contains(body, `<option value="optical">optical</option>`) {
		t.Error("devices page has no family badge, count or filter")
	}
	if body := be.get("/devices?family=optical"); !strings.Contains(body, "0 of 6 devices match") {
		t.Error("family filter")
	}
	if body := be.get("/devices?family=ip"); !strings.Contains(body, "6 of 6 devices match") {
		t.Error("family filter ip")
	}
	// Lower active devices on 127.0.0.1 to 1, stop 127.0.0.2: no testbench
	// runs, so its devices are "testbench stopped".
	_, _ = be.insts["127.0.0.1"].Holder.Update(func(l *runtime.Live) error { l.ActiveDevices = 1; return nil })
	be.insts["127.0.0.2"].Down.Store(true)
	be.poll()
	body = be.get("/devices?status=inactive")
	if !strings.Contains(body, "5 of 6 devices match") || !strings.Contains(body, "beyond active count") || !strings.Contains(body, "testbench stopped") {
		t.Fatal("inactive filter")
	}
	if body := be.get("/devices?reason=" + url.QueryEscape("beyond active count")); !strings.Contains(body, "2 of 6 devices match") {
		t.Fatal("reason filter")
	}
	// Admin API off on 127.0.0.1: unknown.
	be.insts["127.0.0.2"].Down.Store(false)
	be.insts["127.0.0.1"].AdminOff.Store(true)
	be.poll()
	if body := be.get("/devices?status=unknown"); !strings.Contains(body, "3 of 6 devices match") || !strings.Contains(body, "admin API off") {
		t.Fatal("admin off")
	}
	// Sort, search, header find.
	if body := be.get("/devices?sort=hostname&desc=1"); strings.Index(body, ">old-5<") > strings.Index(body, ">old-0<") {
		t.Fatal("descending sort")
	}
	if body := be.get("/devices?q=127.0.0.2"); !strings.Contains(body, "3 of 6 devices match") {
		t.Fatal("search by IP")
	}
	resp, _ := be.do(req{method: "GET", path: "/devices/find?q=old-4"})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "open=old-4") {
		t.Fatalf("find: %d %s", resp.StatusCode, loc)
	}
}

func TestDevicesExportAndBasic(t *testing.T) {
	be := newBenchEnv(t)
	_, _ = be.insts["127.0.0.1"].Holder.Update(func(l *runtime.Live) error { l.ActiveDevices = 2; return nil })
	be.poll()

	resp, body := be.do(req{method: "GET", path: "/devices/export?format=csv&status=active"})
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), "rcfg-sim-B0-active-") {
		t.Fatalf("csv export: %d %v", resp.StatusCode, resp.Header)
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(recs[0], ",") != "hostname,ip,port,username,password,enable_password,model,status,status_reason" || len(recs) != 6 {
		t.Fatalf("csv: %v", recs)
	}
	if strings.Join(recs[1], ",") != "old-0,127.0.0.1,10000,admin,admin,enable123,sm,active," {
		t.Fatalf("row %v", recs[1])
	}
	_, body = be.do(req{method: "GET", path: "/devices/export?format=json&reason=" + url.QueryEscape("beyond active count")})
	var objs []map[string]any
	if err := json.Unmarshal([]byte(body), &objs); err != nil || len(objs) != 1 || objs[0]["hostname"] != "old-2" || len(objs[0]) != 9 {
		t.Fatalf("json: %v %v", objs, err)
	}

	// HTTP Basic: export routes only.
	if r := be.basic("/devices/export?format=csv", "admin", testPassword); r.StatusCode != 200 {
		t.Fatalf("basic on export: %d", r.StatusCode)
	}
	for _, p := range []string{"/devices", "/fleet", "/runs", "/device/old-1", "/bench/pill"} {
		if r := be.basic(p, "admin", testPassword); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("basic on %s: %d, want 401", p, r.StatusCode)
		}
	}
	if r := be.basic("/runs/20991231-000000/export?format=csv", "admin", testPassword); r.StatusCode != http.StatusNotFound {
		t.Errorf("basic on a run export: %d, want 404 (allowed, no such run)", r.StatusCode)
	}
	if r := be.basic("/devices/export?format=csv", "admin", "wrong"); r.StatusCode != http.StatusUnauthorized || r.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("wrong basic password: %d", r.StatusCode)
	}
	if n := be.auth.recentFailures("127.0.0.1"); n != 1 {
		t.Fatalf("basic failure not counted: %d", n)
	}
	// Exporting ticks the checklist.
	if cl := be.get("/checklist"); !strings.Contains(cl, `class="text-muted">Export the devices (CSV or JSON)<span class="sr-only"> (done)</span>`) {
		t.Fatalf("checklist export tick:\n%s", cl)
	}
}

func TestDeviceDrawerAndTestLogin(t *testing.T) {
	be := newBenchEnv(t)
	body := be.post2("/device/old-4")
	for _, want := range []string{"ssh -p 10001 admin@127.0.0.2", "Test login", "enable123"} {
		if !strings.Contains(body, want) {
			t.Errorf("drawer missing %q:\n%s", want, body)
		}
	}
	if r := be.post("/device/old-4/test-login", nil); !strings.Contains(r, "Logged in in 40ms") || !strings.Contains(r, "Cisco IOS Software") {
		t.Fatalf("test login ok: %s", r)
	}
	// A failed login on an instance with auth_fail on says it looks injected.
	_, _ = be.insts["127.0.0.1"].Holder.Update(func(l *runtime.Live) error {
		l.Faults, _ = faultSet(map[string]float64{"auth_fail": 0.05})
		return nil
	})
	be.poll()
	if r := be.post("/device/old-1/test-login", nil); !strings.Contains(r, "Failed at ssh login") || !strings.Contains(r, "injected auth_fail fault (rate 5%)") {
		t.Fatalf("test login failure: %s", r)
	}
	if len(be.tests) != 2 || be.tests[0] != "old-4 127.0.0.2:10001" {
		t.Fatalf("test login calls %v", be.tests)
	}
	// A plain link (no htmx) opens the page with the drawer.
	resp, _ := be.do(req{method: "GET", path: "/device/old-4"})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "open=old-4") {
		t.Fatalf("plain drawer link: %d", resp.StatusCode)
	}
	// The old per-device path is gone; /devices/ keeps only find and export.
	if resp, _ := be.do(req{method: "GET", path: "/devices/old-4", htmx: true}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/devices/old-4: %d, want 404", resp.StatusCode)
	}
}

// post2 is an htmx GET (the drawer is fetched with hx-get).
func (be *benchEnv) post2(path string) string {
	be.t.Helper()
	resp, body := be.do(req{method: "GET", path: path, htmx: true})
	if resp.StatusCode != 200 {
		be.t.Fatalf("GET %s: %d", path, resp.StatusCode)
	}
	return body
}

func TestDevicesScopesNotesAndChecklist(t *testing.T) {
	be := newBenchEnv(t)
	if !strings.Contains(be.get("/fleet"), "Getting started") {
		t.Fatal("no checklist on the Fleet screen")
	}
	be.createBench("Web A", 1)

	// Not built yet: the testbench tab offers to build it.
	body := be.get("/devices?testbench=web-a")
	if !strings.Contains(body, "No kept build has this testbench") || !strings.Contains(body, "/testbenches/web-a/keep-build") {
		t.Fatal("testbench tab before build")
	}
	be.post("/testbenches/web-a/keep-build", nil)
	be.waitJob()
	body = be.get("/devices?testbench=web-a")
	if !strings.Contains(body, "testbench Web A") || !strings.Contains(body, "kept but not live") || !strings.Contains(body, "build not live") {
		t.Fatal("testbench tab after build")
	}

	// Run it: the run records its build; its tab lists it as live.
	be.post("/testbenches/web-a/start", nil)
	be.waitJob()
	runID := be.bench.Active().RunID
	if body := be.get("/devices?run=" + runID); !strings.Contains(body, "run "+runID) || strings.Contains(body, "kept but not live") {
		t.Fatal("run tab")
	}
	if body := be.get("/devices?run=20991231-000000"); !strings.Contains(body, "No run") {
		t.Fatal("unknown run")
	}

	// Notes and tags on the active run, then on a stored one.
	be.post("/runs/"+runID+"/notes", url.Values{"notes": {"first pass"}, "tags": {"rc-8.4.2, smoke"}})
	be.post("/bench/stop", nil)
	be.waitJob()
	if body := be.get("/runs/" + runID); !strings.Contains(body, "first pass") || !strings.Contains(body, "rc-8.4.2, smoke") {
		t.Fatal("notes not shown")
	}
	be.post("/runs/"+runID+"/notes", url.Values{"notes": {"second pass"}, "tags": {"smoke"}})
	if body := be.get("/runs?tag=smoke"); !strings.Contains(body, "Runs tagged") || !strings.Contains(body, "/runs/"+runID) {
		t.Fatal("tag filter")
	}
	if body := be.get("/runs?tag=nope"); strings.Contains(body, "/runs/"+runID+"\"") {
		t.Fatal("tag filter matched a run without the tag")
	}

	// Dismiss the checklist.
	be.post("/checklist/dismiss", nil)
	if strings.Contains(be.get("/fleet"), "Getting started") {
		t.Fatal("checklist still shown after dismiss")
	}
}

func faultSet(rates map[string]float64) (*fault.Set, error) { return fault.NewSetFromRates(rates) }
