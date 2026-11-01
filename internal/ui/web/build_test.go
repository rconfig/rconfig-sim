package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

type buildEnv struct {
	*env
	root   string
	runner *jobs.Runner
	free   int64
	mu     sync.Mutex
}

// newBuildEnv is an install of two instances (3 ports each) on build B0,
// adopted unless legacy, with a fake generator and fake systemctl.
func newBuildEnv(t *testing.T, legacy bool) *buildEnv {
	t.Helper()
	tmp := t.TempDir()
	root, envDir := filepath.Join(tmp, "data"), filepath.Join(tmp, "etc")
	gen := filepath.Join(tmp, "gen")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(gen, []byte(uitest.FakeGenScript), 0o755))
	t.Setenv("FAKE_GEN_ARGS", filepath.Join(tmp, "gen-args"))
	b0 := filepath.Join(root, "builds", "B0")
	must(os.MkdirAll(filepath.Join(b0, "configs"), 0o755))
	man := "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n"
	for i := 0; i < 6; i++ {
		man += fmt.Sprintf("old-%d,127.0.0.%d,%d,Cisco,cisco_ios,admin,admin,enable123,%s/configs/d%d.cfg,sm\n", i, 1+i/3, 10000+i%3, b0, i)
	}
	must(os.WriteFile(filepath.Join(b0, "manifest.csv"), []byte(man), 0o644))
	must(os.WriteFile(filepath.Join(b0, "build.json"), []byte(`{"name":"B0","created":"2026-10-30T16:40:00Z","adopted":true,"count":6}`), 0o644))
	must(os.Symlink("builds/B0", filepath.Join(root, "current")))
	must(os.MkdirAll(envDir, 0o755))
	manifest := filepath.Join(root, "current", "manifest.csv")
	if legacy {
		manifest = "/opt/rcfg-sim/manifest.csv"
	}
	for i := 1; i <= 2; i++ {
		// Metrics on a closed port: these instances are never polled.
		must(os.WriteFile(filepath.Join(envDir, fmt.Sprintf("127.0.0.%d.env", i)),
			[]byte(fmt.Sprintf("PORT_COUNT=3\nMETRICS_ADDR=127.0.0.%d:1\nMANIFEST=%s\n", i, manifest)), 0o660))
	}

	be := &buildEnv{root: root, free: 1 << 40}
	sysctl := &fakeSystemctl{}
	units := systemd.New("systemctl", sysctl.run)
	be.runner = jobs.NewRunner(jobs.Options{
		EnvDir: envDir, GenBin: gen, LogDir: filepath.Join(tmp, "jobs"), Units: units,
		Healthy: func(context.Context, string) error { return nil }, Pause: time.Millisecond,
		Free: func(string) (int64, error) { be.mu.Lock(); defer be.mu.Unlock(); return be.free, nil },
	})
	fl := fleet.New(fleet.Options{EnvDir: envDir})
	auth := testAuth(t)
	srv := New(Options{Fleet: fl, Units: units, Presets: presets.NewStore(filepath.Join(tmp, "presets")), Jobs: be.runner, Auth: auth, Poll: time.Second})
	hs := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); hs.Close(); be.runner.Close() })
	be.env = &env{t: t, dir: envDir, fleet: fl, sysctl: sysctl, srv: srv, http: hs, auth: auth, session: auth.NewSession()}
	return be
}

func buildForm(count, seed string, weights map[string]string) url.Values {
	f := url.Values{"count": {count}, "seed": {seed}}
	for k, v := range weights {
		f.Set("w_"+k, v)
	}
	return f
}

func (be *buildEnv) streamJob(id string) string {
	be.t.Helper()
	resp, err := be.client().Get(be.http.URL + "/build/jobs/" + id + "/stream")
	if err != nil {
		be.t.Fatal(err)
	}
	defer resp.Body.Close()
	ch := make(chan sseEvent, 256)
	go readSSE(resp.Body, ch)
	var lines strings.Builder
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				be.t.Fatal("stream ended without done")
			}
			switch ev.name {
			case "line":
				lines.WriteString(ev.data)
			case "state":
				lines.WriteString("\nSTATE: " + ev.data)
			case "done":
				return lines.String()
			}
		case <-timeout:
			be.t.Fatalf("job stream did not finish; so far:\n%s", lines.String())
		}
	}
}

func jobID(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `sse-connect="/build/jobs/`)
	if i < 0 {
		t.Fatalf("no job stream in:\n%s", body)
	}
	rest := body[i+len(`sse-connect="/build/jobs/`):]
	return rest[:strings.Index(rest, "/stream")]
}

func TestBuildPageLegacyShowsAdoption(t *testing.T) {
	be := newBuildEnv(t, true)
	body := be.get("/build")
	for _, want := range []string{"Adopt this install first", "sudo make adopt-data", "adopt-data.sh", "not &lt;data&gt;/current/manifest.csv"} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy build page missing %q", want)
		}
	}
	if strings.Contains(body, `id="build-form"`) {
		t.Error("a legacy install must not show the build form")
	}
	if body := be.post("/build/run", buildForm("6", "1", map[string]string{"sm": "100"})); !strings.Contains(body, "not adopted") {
		t.Fatalf("run on a legacy install: %s", body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(be.root), "gen-args")); !os.IsNotExist(err) {
		t.Fatal("generator ran on a legacy install")
	}
}

func TestBuildPageAndEstimate(t *testing.T) {
	be := newBuildEnv(t, false)
	body := be.get("/build")
	for _, want := range []string{`id="build-form"`, "B0", "Adopted from a pre-UI install", `name="w_sm"`, `name="w_cisco-ons15454-tl1"`,
		`min="4"`, `max="6"`, "Active devices on the Fleet screen", "No previous build", `hx-post="/build/estimate"`,
		"htmx-ext-sse-2.2.4.min.js", `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("build page missing %q", want)
		}
	}
	for desc, c := range map[string]struct {
		form url.Values
		want string
	}{
		"ready":     {buildForm("5", "1", map[string]string{"sm": "60", "md": "40"}), "Ready to build"},
		"too few":   {buildForm("3", "1", map[string]string{"sm": "100"}), "Active devices"},
		"too many":  {buildForm("8", "1", map[string]string{"sm": "100"}), "ip-aliases.sh --interface &lt;iface&gt; --base-ip 127.0.0.3 --count 1"},
		"total":     {buildForm("5", "1", map[string]string{"sm": "90"}), "90% of 100%"},
		"bad count": {buildForm("x", "1", map[string]string{"sm": "100"}), "whole number"},
	} {
		if got := be.post("/build/estimate", c.form); !strings.Contains(got, c.want) {
			t.Errorf("%s: estimate missing %q:\n%s", desc, c.want, got)
		}
	}
	be.mu.Lock()
	be.free = 10
	be.mu.Unlock()
	if got := be.post("/build/estimate", buildForm("6", "1", map[string]string{"sm": "100"})); !strings.Contains(got, "not enough disk") || !strings.Contains(got, "text-st-error") {
		t.Fatalf("disk: %s", got)
	}
}

func TestBuildRunStreamAndRollback(t *testing.T) {
	be := newBuildEnv(t, false)
	body := be.post("/build/run", buildForm("6", "11", map[string]string{"sm": "50", "ciena-6500-tl1": "50"}))
	id := jobID(t, body)
	if !strings.Contains(body, `sse-close="done"`) || !strings.Contains(body, `sse-swap="line"`) {
		t.Fatalf("job panel: %s", body)
	}
	log := be.streamJob(id)
	for _, want := range []string{"Build of 6 devices, seed 11", "wrote ", "current -&gt; builds/", "restarting", "is live", "STATE: ", "Finished."} {
		if !strings.Contains(log, want) {
			t.Errorf("job log missing %q:\n%s", want, log)
		}
	}
	if target, _ := os.Readlink(filepath.Join(be.root, "current")); target == "builds/B0" {
		t.Fatal("current not swapped")
	}

	page := be.get("/build")
	if !strings.Contains(page, "Previous build <span class=\"num text-fg\">B0</span>") || !strings.Contains(page, `hx-post="/build/rollback"`) {
		t.Fatalf("rollback not offered after a build")
	}
	if !strings.Contains(page, "11") || !strings.Contains(page, "sm:50,ciena-6500-tl1:50") {
		t.Fatal("the form must start from the live build's settings")
	}

	rb := be.post("/build/rollback", url.Values{})
	log = be.streamJob(jobID(t, rb))
	if !strings.Contains(log, "Rollback from builds/") || !strings.Contains(log, "Finished.") {
		t.Fatalf("rollback log:\n%s", log)
	}
	if target, _ := os.Readlink(filepath.Join(be.root, "current")); target != "builds/B0" {
		t.Fatalf("current after rollback: %s", target)
	}
	// A finished job's page shows the log without a stream.
	if page := be.get("/build"); !strings.Contains(page, "Rollback from builds/") || strings.Contains(page, "sse-connect=\"/build/jobs/") {
		t.Fatal("finished job not shown statically")
	}
}

func TestBuildAndFleetRestartExclusive(t *testing.T) {
	be := newBuildEnv(t, false)
	be.srv.rollMu.Lock()
	be.srv.rolling.Running = true
	be.srv.rollMu.Unlock()
	if body := be.post("/build/run", buildForm("6", "1", map[string]string{"sm": "100"})); !strings.Contains(body, "rolling restart is running") {
		t.Fatalf("build during a fleet rolling restart: %s", body)
	}
	be.srv.rollMu.Lock()
	be.srv.rolling.Running = false
	be.srv.rollMu.Unlock()

	t.Setenv("FAKE_GEN_SLEEP", "0.5")
	id := jobID(t, be.post("/build/run", buildForm("6", "1", map[string]string{"sm": "100"})))
	if body := be.post("/fleet/rolling-restart", url.Values{"pause": {"0"}}); !strings.Contains(body, "build or rollback is running") {
		t.Fatalf("fleet rolling restart during a build: %s", body)
	}
	if body := be.post("/build/run", buildForm("6", "2", map[string]string{"sm": "100"})); !strings.Contains(body, "already running") {
		t.Fatalf("second build: %s", body)
	}
	be.streamJob(id)
}

func TestJobPanelRender(t *testing.T) {
	running := renderString(t, views.JobPanel(views.JobModel{ID: "j1", Kind: "build"}))
	for _, want := range []string{`hx-ext="sse"`, `sse-connect="/build/jobs/j1/stream"`, `sse-close="done"`, `sse-swap="line"`, `hx-swap="beforeend"`, `sse-swap="state"`} {
		if !strings.Contains(running, want) {
			t.Errorf("running panel missing %q", want)
		}
	}
	failed := renderString(t, views.JobPanel(views.JobModel{ID: "j2", Kind: "build", Finished: true, Err: "boom", OfferRollback: true, Lines: []string{"a <b>"}}))
	if !strings.Contains(failed, "boom") || !strings.Contains(failed, `hx-post="/build/rollback"`) || !strings.Contains(failed, "a &lt;b&gt;") || strings.Contains(failed, "sse-connect") {
		t.Fatalf("failed panel: %s", failed)
	}
}
