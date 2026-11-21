package web

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// TestScenarioLibrary: the library lists mesh-campus as a choice card with
// its device count, drivers and fit; its detail draws the topology as SVG
// with focusable nodes linked to the device table; a broken package
// dropped into --scenarios-dir shows with its reason after a reload.
func TestScenarioLibrary(t *testing.T) {
	be := newBenchEnv(t)
	body := be.get("/scenarios")
	for _, want := range []string{`aria-current="page"`, ">Scenarios<", `name="id" value="mesh-campus"`, "Mesh campus",
		"3 devices", ">cisco_ios<", ">junos<", ">fits<", `hx-get="/scenarios"`, be.scenarios} {
		if !strings.Contains(body, want) {
			t.Errorf("library missing %q", want)
		}
	}
	if strings.Contains(body, `class="topology`) {
		t.Error("the library shows a detail before one is chosen")
	}

	d := be.get("/scenarios/mesh-campus")
	for _, want := range []string{`name="id" value="mesh-campus" checked`, `id="scenario-detail"`, `class="topology max-w-full"`,
		`href="#dev-core-01"`, `aria-label="dist-01, Juniper junos, tier 1"`, `id="dev-access-01"`, "127.0.0.1:10002",
		"Any password is accepted", "Response delay 50 to 300 ms", "No faults",
		"Fits: 3 devices on 1 of this host&#39;s 2 instances; the other 1 are not used.",
		`href="/testbenches/new?scenario=mesh-campus"`, "Create testbench from this scenario"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail missing %q", want)
		}
	}
	if n := strings.Count(d, `class="topo-node"`); n != 3 {
		t.Errorf("%d diagram nodes, want 3", n)
	}
	if n := len(regexp.MustCompile(`<line x1=`).FindAllString(d, -1)); n != 2 {
		t.Errorf("%d diagram links, want 2", n)
	}
	// The diagram is in tiers: core-01 above dist-01 above access-01.
	ys := map[string]int{}
	for _, m := range regexp.MustCompile(`<text x="\d+" y="(\d+)" text-anchor="middle" class="topo-name">([a-z0-9-]+)</text>`).FindAllStringSubmatch(d, -1) {
		ys[m[2]], _ = strconv.Atoi(m[1])
	}
	if !(ys["core-01"] > 0 && ys["core-01"] < ys["dist-01"] && ys["dist-01"] < ys["access-01"]) {
		t.Errorf("tiers: %v", ys)
	}
	if got := be.get("/scenarios?id=mesh-campus"); !strings.Contains(got, `id="dev-core-01"`) {
		t.Error("?id= does not open the detail")
	}
	resp, _ := be.do(req{method: "GET", path: "/scenarios/nope"})
	if resp.StatusCode != 404 {
		t.Errorf("unknown scenario: %d", resp.StatusCode)
	}

	// A package dropped in with a config missing: listed, with its reason,
	// and it cannot be picked.
	dir := filepath.Join(be.scenarios, "broken-lab")
	for name, data := range map[string]string{
		"scenario.json": `{"id":"broken-lab","name":"Broken lab","description":"x","version":1,"drivers":["cisco_ios"],"auth":{"mode":"any_password"},"topology":{"nodes":[],"links":[]}}`,
		"manifest.csv":  "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\nr1,10.0.0.1,1,Cisco,cisco_ios,a,,,configs/r1.cfg,sm\n",
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body = be.get("/scenarios")
	for _, want := range []string{"Broken lab", ">cannot be used<", "Needs attention", "r1 has no config file (configs/r1.cfg)", "your package in folder broken-lab"} {
		if !strings.Contains(body, want) {
			t.Errorf("library with a broken package missing %q", want)
		}
	}
	if !regexp.MustCompile(`value="broken-lab"[^>]*disabled`).MatchString(body) {
		t.Error("a broken package can be picked")
	}
	if d := be.get("/scenarios/broken-lab"); strings.Contains(d, "Create testbench from this scenario") || !strings.Contains(d, "This package cannot be used") {
		t.Error("a broken package offers a testbench")
	}
}

// TestScenarioTestbench: "Create testbench from this scenario" opens the
// flow on the devices step with the package chosen; the review step says
// which instances are left unused and that a restart is needed; the saved
// testbench sits in Topology scenarios. Starting the shipped one marks
// the unused instance Not used on the Fleet screen.
func TestScenarioTestbench(t *testing.T) {
	be := newBenchEnv(t)
	f := be.flowFrom("/testbenches/new?scenario=mesh-campus")
	for _, want := range []string{"Topology scenario", `name="scenario" value="mesh-campus" checked`, "3 devices from the package"} {
		if !strings.Contains(f.body, want) {
			t.Errorf("devices step missing %q", want)
		}
	}
	f.step(1, 2, url.Values{"scenario": {"mesh-campus"}})
	if !strings.Contains(f.body, `name="delay_max" value="300"`) {
		t.Error("the scenario's suggested behaviour was not applied")
	}
	beh := behaviourFields()
	beh.Set("ov_ip", "")
	beh.Set("ov_faults", "")
	beh.Set("ov_max_sessions", "")
	review := f.step(2, 3, beh)
	for _, want := range []string{"3 devices of the Mesh campus scenario on 1 of 2 instances: installs the package as a build, then starts.",
		"Uses 1 of 2 instances. The other 1 are not used by this testbench and are stopped: 127.0.0.2.",
		"accepts any password", "that needs a restart", "<title>cisco_ios 2</title>", "<title>junos 1</title>"} {
		if !strings.Contains(review, want) {
			t.Errorf("review missing %q", want)
		}
	}
	if strings.Contains(review, "Fix these before saving") {
		t.Fatalf("review has problems:\n%s", review)
	}
	resp, body := f.save("", url.Values{"name": {"My campus"}})
	if resp.Header.Get("HX-Redirect") != "/testbenches/my-campus" {
		t.Fatalf("save: %s", body)
	}
	list := be.get("/testbenches")
	topo := list[strings.Index(list, `aria-label="Topology scenarios"`):]
	for _, want := range []string{"My campus", "Yours", "Topology: mesh campus", "Shipped", ">Topology<"} {
		if !strings.Contains(topo, want) {
			t.Errorf("Topology scenarios group missing %q", want)
		}
	}
	// Editing keeps the kind and the package.
	if edit := be.get("/testbenches/my-campus"); !strings.Contains(edit, `name="scenario" value="mesh-campus" checked`) {
		t.Error("editing a scenario testbench lost its package")
	}

	if body := be.post("/testbenches/topology-mesh-campus/start", nil); !strings.Contains(body, "/build/jobs/") {
		t.Fatalf("start: %s", body)
	}
	be.waitJob()
	be.fleet.Poll(t.Context())
	if v, _ := be.fleet.View("127.0.0.2"); v.Status != fleet.StatusUnused {
		t.Fatalf("127.0.0.2: %s", v.Status)
	}
	page := be.get("/fleet")
	card := page[strings.Index(page, `data-instance="127.0.0.2"`):]
	card = card[:strings.Index(card, "</section>")+1]
	for _, want := range []string{"Not used by this testbench", "pill-neutral", "disabled"} {
		if !strings.Contains(card, want) {
			t.Errorf("unused card missing %q", want)
		}
	}
	if !strings.Contains(page, "1 / 1 instances up") {
		t.Error("the health pill counts the unused instance")
	}
	if body := be.post("/fleet/instances/127.0.0.2/start", nil); !strings.Contains(body, "not used by this testbench") {
		t.Errorf("starting an unused instance: %s", body)
	}
}
