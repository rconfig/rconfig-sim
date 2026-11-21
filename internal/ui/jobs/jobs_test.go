package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
)

type rig struct {
	t       *testing.T
	root    string // data dir
	envDir  string
	args    string // where the fake generator records its arguments
	runner  *Runner
	mu      sync.Mutex
	calls   []string
	unhappy map[string]bool // instances that never come back healthy
	free    int64
	removed []string // builds retention told Options.Removed about
}

// newRig builds an adopted install in a temp directory: two instances
// (127.0.0.1 and .2, 3 ports each) on build B0 of 6 devices.
func newRig(t *testing.T) *rig {
	t.Helper()
	tmp := t.TempDir()
	r := &rig{t: t, root: filepath.Join(tmp, "data"), envDir: filepath.Join(tmp, "etc"),
		args: filepath.Join(tmp, "gen-args"), unhappy: map[string]bool{}, free: 1 << 40}
	gen := filepath.Join(tmp, "fake-gen")
	uitest.WriteFakeGen(t, gen)
	t.Setenv("FAKE_GEN_ARGS", r.args)

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
	must(t, writeSettings(b0, &Settings{Name: "B0", Count: 6, Seed: &seed, Distribution: "sm:100",
		Options: map[string]string{"rne-dual-home-pct": "10"}}))
	must(t, os.Symlink("builds/B0", filepath.Join(r.root, "current")))

	must(t, os.MkdirAll(r.envDir, 0o755))
	for i := 1; i <= 2; i++ {
		env := fmt.Sprintf("LISTEN_IP=127.0.0.%d\nPORT_START=10000\nPORT_COUNT=3\nMETRICS_ADDR=127.0.0.%d:9100\nMANIFEST=%s\n",
			i, i, filepath.Join(r.root, "current", "manifest.csv"))
		must(t, os.WriteFile(filepath.Join(r.envDir, fmt.Sprintf("127.0.0.%d.env", i)), []byte(env), 0o660))
	}

	r.runner = NewRunner(Options{
		EnvDir: r.envDir, GenBin: gen, LogDir: filepath.Join(tmp, "jobs"),
		Units:   systemd.New("systemctl", r.systemctl),
		Healthy: r.healthy, Pause: time.Millisecond, Wait: 200 * time.Millisecond,
		Now:       func() time.Time { return time.Date(2026, 10, 31, 9, 15, 0, 0, time.UTC) },
		Free:      func(string) (int64, error) { return r.free, nil },
		Catalogue: catalogue.NewSource(gen, nil),
		Removed:   func(name string) { r.mu.Lock(); r.removed = append(r.removed, name); r.mu.Unlock() },
	})
	t.Cleanup(r.runner.Close)
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (r *rig) systemctl(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	return nil, nil
}

func (r *rig) healthy(_ context.Context, inst string) error {
	if r.unhappy[inst] {
		return errors.New("down")
	}
	return nil
}

func (r *rig) restarts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *rig) current() string {
	target, err := os.Readlink(filepath.Join(r.root, "current"))
	if err != nil {
		r.t.Fatal(err)
	}
	return target
}

func (r *rig) builds() []string {
	entries, _ := os.ReadDir(filepath.Join(r.root, "builds"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func wait(t *testing.T, j *Job) {
	t.Helper()
	select {
	case <-j.Done():
	case <-time.After(10 * time.Second):
		lines, _ := j.Lines(0)
		t.Fatalf("job did not finish:\n%s", strings.Join(lines, "\n"))
	}
}

func discover(t *testing.T, dir string) []fleet.Instance {
	t.Helper()
	insts, err := fleet.Discover(dir)
	must(t, err)
	return insts
}

func TestLayoutOf(t *testing.T) {
	r := newRig(t)
	l, err := LayoutOf(discover(t, r.envDir), r.envDir)
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != r.root || l.IPBase != "127.0.0.1" || l.IPCount != 2 || l.DevicesPerIP != 3 || l.PortStart != 10000 {
		t.Fatalf("layout: %+v", l)
	}

	bad := func(name, content string) error {
		dir := t.TempDir()
		for _, f := range []string{"127.0.0.1.env", name} {
			b, _ := os.ReadFile(filepath.Join(r.envDir, "127.0.0.1.env"))
			if f == name {
				b = []byte(content)
			}
			must(t, os.WriteFile(filepath.Join(dir, f), b, 0o660))
		}
		_, err := LayoutOf(discover(t, dir), dir)
		return err
	}
	man := "MANIFEST=" + filepath.Join(r.root, "current", "manifest.csv") + "\n"
	for desc, c := range map[string]struct{ name, env, want string }{
		"gap":        {"127.0.0.3.env", "PORT_COUNT=3\nMETRICS_ADDR=127.0.0.3:9100\n" + man, "consecutive"},
		"port start": {"127.0.0.2.env", "PORT_START=11000\nPORT_COUNT=3\nMETRICS_ADDR=127.0.0.2:9100\n" + man, "PORT_START"},
		"port count": {"127.0.0.2.env", "PORT_COUNT=4\nMETRICS_ADDR=127.0.0.2:9100\n" + man, "PORT_COUNT"},
		"manifest":   {"127.0.0.2.env", "PORT_COUNT=3\nMETRICS_ADDR=127.0.0.2:9100\nMANIFEST=/x/current/manifest.csv\n", "MANIFEST"},
	} {
		if err := bad(c.name, c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want an error about %s, got %v", desc, c.want, err)
		}
	}

	// Legacy install: MANIFEST not under current/, or current not a symlink.
	legacy := t.TempDir()
	must(t, os.WriteFile(filepath.Join(legacy, "127.0.0.1.env"), []byte("PORT_COUNT=3\nMETRICS_ADDR=127.0.0.1:9100\n"), 0o660))
	var na *NotAdoptedError
	if _, err := LayoutOf(discover(t, legacy), legacy); !errors.As(err, &na) {
		t.Fatalf("default MANIFEST must be not adopted, got %v", err)
	}
	plain := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(plain, "data", "current"), 0o755))
	must(t, os.WriteFile(filepath.Join(plain, "127.0.0.1.env"),
		[]byte("PORT_COUNT=3\nMETRICS_ADDR=127.0.0.1:9100\nMANIFEST="+filepath.Join(plain, "data", "current", "manifest.csv")+"\n"), 0o660))
	if _, err := LayoutOf(discover(t, plain), plain); !errors.As(err, &na) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a current directory (not symlink) must be not adopted, got %v", err)
	}
}

func TestCountBounds(t *testing.T) {
	r := newRig(t)
	l, _ := LayoutOf(discover(t, r.envDir), r.envDir)
	if lo, hi := l.CountBounds(); lo != 4 || hi != 6 {
		t.Fatalf("bounds: %d..%d", lo, hi)
	}
	for _, n := range []int{4, 5, 6} {
		if err := l.CheckCount(n); err != nil {
			t.Errorf("%d: %v", n, err)
		}
	}
	var ce *CountError
	if err := l.CheckCount(3); !errors.As(err, &ce) || ce.Steps != nil || !strings.Contains(err.Error(), "Active devices") {
		t.Fatalf("3 devices would leave 127.0.0.2 empty: %v", err)
	}
	err := l.CheckCount(9)
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "2 instances") {
		t.Fatalf("9: %v", err)
	}
	steps := strings.Join(ce.Steps, "\n")
	for _, want := range []string{
		"ip-aliases.sh --interface <iface> --base-ip 127.0.0.3 --count 1",
		filepath.Join(r.envDir, "127.0.0.3.env"),
		"LISTEN_IP=127.0.0.3", "METRICS_ADDR=127.0.0.3:9100", "host-keys/127.0.0.3.key",
		"systemctl enable rcfg-sim@127.0.0.3",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps missing %q:\n%s", want, steps)
		}
	}
	if strings.Contains(steps, "enable --now") {
		t.Error("a new instance has no devices until the build: enable, do not start")
	}
	if strings.Contains(steps, "ADMIN_ADDR") {
		t.Error("the template's admin API is off, so the copy's must stay off")
	}

	// With admin APIs on, each new instance gets the next free loopback port.
	for i := 1; i <= 2; i++ {
		f, err := os.OpenFile(filepath.Join(r.envDir, fmt.Sprintf("127.0.0.%d.env", i)), os.O_APPEND|os.O_WRONLY, 0)
		must(t, err)
		_, err = fmt.Fprintf(f, "ADMIN_ADDR=127.0.0.1:1910%d\n", i)
		must(t, errors.Join(err, f.Close()))
	}
	if l, err = LayoutOf(discover(t, r.envDir), r.envDir); err != nil {
		t.Fatal(err)
	}
	_ = errors.As(l.CheckCount(12), &ce)
	steps = strings.Join(ce.Steps, "\n")
	for _, want := range []string{"ADMIN_ADDR=127.0.0.1:19103|", "ADMIN_ADDR=127.0.0.1:19104|"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps missing %q:\n%s", want, steps)
		}
	}
}

func TestEstimateBuild(t *testing.T) {
	cat, err := catalogue.Decode(uitest.CatalogueJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	e := EstimateBuild(map[string]int{"sm": 500, "ciena-6500-tl1": 500}, cat)
	if e.ConfigBytes != 500*25_600+500*1_700 {
		t.Fatalf("config bytes: %d", e.ConfigBytes)
	}
	// Whole 4 KiB blocks: sm is 7 blocks, the TL1 file 1, plus manifest rows.
	if e.DiskBytes != 500*7*4096+500*4096+1000*manifestRow {
		t.Fatalf("disk bytes: %d", e.DiskBytes)
	}
	if e.NeedFree != int64(float64(e.DiskBytes)*1.10) || e.Generate <= 0 || len(e.PerModel) != 2 || e.PerModel[0].Model != "sm" {
		t.Fatalf("estimate: %+v", e)
	}
}

func TestBuildRollbackAndRetention(t *testing.T) {
	r := newRig(t)
	// Two older builds: with --keep-builds 3 retention keeps the new one, B0
	// (just replaced, so most recently used) and the newer of these two.
	for i, name := range []string{"A-older", "A-oldest"} {
		dir := filepath.Join(r.root, "builds", name)
		must(t, os.MkdirAll(filepath.Join(dir, "configs"), 0o755))
		must(t, writeSettings(dir, &Settings{Name: name, LastUsed: time.Date(2026, 10, 1-i, 0, 0, 0, 0, time.UTC)}))
	}

	j, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 3, "md": 2}, Seed: 99, Distribution: "sm:60,md:40"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, j)
	if j.Err() != nil {
		lines, _ := j.Lines(0)
		t.Fatalf("build failed: %v\n%s", j.Err(), strings.Join(lines, "\n"))
	}
	name := "20261031-0915"
	if r.current() != "builds/"+name {
		t.Fatalf("current -> %s", r.current())
	}
	if b := r.builds(); !slices.Equal(b, []string{name, "A-older", "B0"}) {
		t.Fatalf("retention: builds %v, want the new one, A-older and B0", b)
	}

	// The generator was given the request, the layout, the live build's
	// credentials and its carried-over options.
	args, _ := os.ReadFile(r.args)
	dir := filepath.Join(r.root, "builds", name)
	for _, want := range []string{"--counts md:2,sm:3", "--seed 99", "--ip-base 127.0.0.1", "--ip-count 2",
		"--devices-per-ip 3", "--port-start 10000", "--username ops", "--password s3cret", "--enable-password en4ble",
		"--rne-dual-home-pct 10", "--output-dir " + filepath.Join(dir, "configs"), "--manifest " + filepath.Join(dir, "manifest.csv")} {
		if !strings.Contains(string(args), want) {
			t.Errorf("generator args missing %q: %s", want, args)
		}
	}
	// Manifest paths point into the build's own directory, and everything
	// is readable by rcfg-sim.
	man, _ := os.ReadFile(filepath.Join(r.root, "current", "manifest.csv"))
	if !strings.Contains(string(man), filepath.Join(dir, "configs", "device-0.cfg")) {
		t.Fatalf("manifest paths: %s", man)
	}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		fi, _ := d.Info()
		want := os.FileMode(0o644)
		if d.IsDir() {
			want = 0o755
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", p, fi.Mode().Perm(), want)
		}
		return nil
	})
	st, err := ReadSettings(dir)
	if err != nil || st.Previous != "B0" || st.Count != 5 || *st.Seed != 99 || st.Options["rne-dual-home-pct"] != "10" {
		t.Fatalf("build.json: %+v %v", st, err)
	}
	if got := r.restarts(); !slices.Equal(got, []string{"restart rcfg-sim@127.0.0.1.service", "restart rcfg-sim@127.0.0.2.service"}) {
		t.Fatalf("restarts: %v", got)
	}
	lines, done := j.Lines(0)
	if !done || !strings.Contains(strings.Join(lines, "\n"), "wrote "+filepath.Join(dir, "configs", "device-4.cfg")) {
		t.Fatalf("job log must stream generator output:\n%s", strings.Join(lines, "\n"))
	}
	if logs, _ := filepath.Glob(filepath.Join(filepath.Dir(r.args), "jobs", "*-build.log")); len(logs) != 1 {
		t.Fatalf("job log on disk: %v", logs)
	}

	status := r.runner.Status()
	if status.Live.Name != name || status.Previous == nil || status.Previous.Name != "B0" {
		t.Fatalf("status: %+v", status)
	}

	// Roll back.
	rb, err := r.runner.StartRollback()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, rb)
	if rb.Err() != nil || r.current() != "builds/B0" || len(r.restarts()) != 4 {
		t.Fatalf("rollback: %v, current %s, restarts %v", rb.Err(), r.current(), r.restarts())
	}
	if b := r.builds(); !slices.Equal(b, []string{name, "A-older", "B0"}) {
		t.Fatalf("rollback must keep the newer build: %v", b)
	}
}

func TestGeneratorFailureLeavesLiveBuild(t *testing.T) {
	r := newRig(t)
	t.Setenv("FAKE_GEN_FAIL", "1")
	j, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, j)
	if j.Err() == nil || !strings.Contains(j.Err().Error(), "live build is untouched") || j.OfferRollback() {
		t.Fatalf("job: %v (offer rollback %v)", j.Err(), j.OfferRollback())
	}
	if r.current() != "builds/B0" || !slices.Equal(r.builds(), []string{"B0"}) || len(r.restarts()) != 0 {
		t.Fatalf("after a failed generator: current %s, builds %v, restarts %v", r.current(), r.builds(), r.restarts())
	}
}

func TestRefusedBeforeAnythingIsWritten(t *testing.T) {
	r := newRig(t)
	cases := map[string]struct {
		req  Request
		free int64
		want string
	}{
		"too few":    {Request{Counts: map[string]int{"sm": 3}, Seed: 1}, 1 << 40, "would leave an instance with none"},
		"too many":   {Request{Counts: map[string]int{"sm": 7}, Seed: 1}, 1 << 40, "need more IP aliases"},
		"model":      {Request{Counts: map[string]int{"nope": 6}, Seed: 1}, 1 << 40, "unknown model"},
		"no devices": {Request{Counts: map[string]int{"sm": 0}, Seed: 1}, 1 << 40, "no devices"},
		"disk":       {Request{Counts: map[string]int{"sm": 6}, Seed: 1}, 1000, "not enough disk"},
	}
	for name, c := range cases {
		r.free = c.free
		if _, err := r.runner.StartBuild(c.req); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	if _, err := os.Stat(r.args); !os.IsNotExist(err) {
		t.Fatal("the generator ran for a refused build")
	}
	if r.current() != "builds/B0" || !slices.Equal(r.builds(), []string{"B0"}) || r.runner.Current() != nil {
		t.Fatal("a refused build changed something")
	}
}

func TestNotAdoptedBuildsNothing(t *testing.T) {
	r := newRig(t)
	for _, f := range []string{"127.0.0.1.env", "127.0.0.2.env"} {
		p := filepath.Join(r.envDir, f)
		b, _ := os.ReadFile(p)
		must(t, os.WriteFile(p, []byte(strings.ReplaceAll(string(b), filepath.Join(r.root, "current", "manifest.csv"), "/opt/rcfg-sim/manifest.csv")), 0o660))
	}
	var na *NotAdoptedError
	if st := r.runner.Status(); !errors.As(st.Err, &na) {
		t.Fatalf("status: %v", st.Err)
	}
	if _, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 1}); !errors.As(err, &na) {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(r.args); !os.IsNotExist(err) {
		t.Fatal("generator ran")
	}
}

func TestFailedRestartOffersRollback(t *testing.T) {
	r := newRig(t)
	r.unhappy["127.0.0.2"] = true
	j, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, j)
	if j.Err() == nil || !j.OfferRollback() || !strings.Contains(j.Err().Error(), "Roll back") {
		t.Fatalf("job: %v, offer %v", j.Err(), j.OfferRollback())
	}
	// No retention after a failed restart: the older builds stay.
	if !slices.Contains(r.builds(), "B0") {
		t.Fatal("B0 removed after a failed restart")
	}
	r.unhappy["127.0.0.2"] = false
	rb, err := r.runner.StartRollback()
	if err != nil {
		t.Fatal(err)
	}
	wait(t, rb)
	if rb.Err() != nil || r.current() != "builds/B0" {
		t.Fatalf("rollback: %v, current %s", rb.Err(), r.current())
	}
}

func TestOneJobAtATime(t *testing.T) {
	r := newRig(t)
	t.Setenv("FAKE_GEN_SLEEP", "0.5")
	j, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 2}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second build: %v", err)
	}
	if _, err := r.runner.StartRollback(); !errors.Is(err, ErrBusy) {
		t.Fatalf("rollback during a build: %v", err)
	}
	wait(t, j)
}

func TestNoPreviousNoRollback(t *testing.T) {
	r := newRig(t)
	if _, err := r.runner.StartRollback(); err == nil || !strings.Contains(err.Error(), "no previous build") {
		t.Fatalf("rollback with no previous build: %v", err)
	}
}

// TestAdoptScript runs deploy/adopt-data.sh against scratch files (never
// /opt), then checks the result is a layout the runner accepts.
func TestAdoptScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	g, err := user.LookupGroupId(fmt.Sprint(os.Getgid()))
	if err != nil {
		t.Skip("cannot name the current group")
	}
	tmp := t.TempDir()
	opt := filepath.Join(tmp, "opt")
	cfgDir, manifest, envDir, data := filepath.Join(opt, "configs"), filepath.Join(opt, "manifest.csv"), filepath.Join(tmp, "etc"), filepath.Join(opt, "data")
	must(t, os.MkdirAll(cfgDir, 0o755))
	must(t, os.MkdirAll(envDir, 0o755))
	var man strings.Builder
	man.WriteString("hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n")
	for i := 0; i < 4; i++ {
		f := filepath.Join(cfgDir, fmt.Sprintf("device-%d.cfg", i))
		must(t, os.WriteFile(f, []byte("cfg\n"), 0o644))
		fmt.Fprintf(&man, "h-%d,127.0.0.%d,%d,Cisco,cisco_ios,admin,admin,enable123,%s,sm\n", i, 1+i/2, 10000+i%2, f)
	}
	must(t, os.WriteFile(manifest, []byte(man.String()), 0o644))
	must(t, os.WriteFile(filepath.Join(envDir, "127.0.0.1.env"), []byte("# one\nPORT_COUNT=2\nMETRICS_ADDR=127.0.0.1:9100\nMANIFEST="+manifest+"\n"), 0o660))
	must(t, os.WriteFile(filepath.Join(envDir, "127.0.0.2.env"), []byte("PORT_COUNT=2\nMETRICS_ADDR=127.0.0.2:9100\nMANIFEST="+manifest+"\n"), 0o660))

	script := filepath.Join("..", "..", "..", "deploy", "adopt-data.sh")
	run := func(extra ...string) (string, error) {
		args := append([]string{script, "--data-dir", data, "--manifest", manifest, "--configs-dir", cfgDir, "--env-dir", envDir, "--group", g.Name}, extra...)
		out, err := exec.Command("bash", args...).CombinedOutput()
		return string(out), err
	}

	// A dry run changes nothing.
	if out, err := run("--dry-run"); err != nil || !strings.Contains(out, "would: mv") {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("dry run created the data directory")
	}

	if out, err := run(); err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	insts := discover(t, envDir)
	l, err := LayoutOf(insts, envDir)
	if err != nil {
		t.Fatalf("adopted layout: %v", err)
	}
	if l.Root != data {
		t.Fatalf("root %s", l.Root)
	}
	live, err := ReadSettings(filepath.Join(data, "current"))
	if err != nil || !live.Adopted || live.Count != 4 || live.Seed != nil {
		t.Fatalf("build.json: %+v %v", live, err)
	}
	newMan, _ := os.ReadFile(filepath.Join(data, "current", "manifest.csv"))
	buildDir := filepath.Join(data, "builds", live.Name)
	if !strings.Contains(string(newMan), filepath.Join(buildDir, "configs", "device-3.cfg")) || strings.Contains(string(newMan), cfgDir+"/") {
		t.Fatalf("manifest not rewritten:\n%s", newMan)
	}
	if b, err := os.ReadFile(filepath.Join(buildDir, "configs", "device-0.cfg")); err != nil || string(b) != "cfg\n" {
		t.Fatalf("configs not moved: %v", err)
	}
	if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
		t.Fatal("old configs directory still there")
	}
	env1, _ := os.ReadFile(filepath.Join(envDir, "127.0.0.1.env"))
	if !strings.HasPrefix(string(env1), "# one\n") || !strings.Contains(string(env1), "MANIFEST="+filepath.Join(data, "current", "manifest.csv")) {
		t.Fatalf("env file: %s", env1)
	}
	if creds, err := credentialsOf(filepath.Join(data, "current", "manifest.csv")); err != nil || creds.Username != "admin" {
		t.Fatalf("credentials: %+v %v", creds, err)
	}

	// Run twice: refused.
	if out, err := run(); err == nil || !strings.Contains(out, "already adopted") {
		t.Fatalf("second run: %v\n%s", err, out)
	}
}

func TestAdoptScriptRefusesMixedManifests(t *testing.T) {
	g, err := user.LookupGroupId(fmt.Sprint(os.Getgid()))
	if err != nil {
		t.Skip("cannot name the current group")
	}
	tmp := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(tmp, "configs"), 0o755))
	must(t, os.WriteFile(filepath.Join(tmp, "manifest.csv"), []byte("h\nx,1.1.1.1,1,a,b,c,d,e,"+filepath.Join(tmp, "configs", "x.cfg")+",sm\n"), 0o644))
	must(t, os.MkdirAll(filepath.Join(tmp, "etc"), 0o755))
	must(t, os.WriteFile(filepath.Join(tmp, "etc", "1.1.1.1.env"), []byte("MANIFEST=/elsewhere.csv\n"), 0o660))
	out, err := exec.Command("bash", filepath.Join("..", "..", "..", "deploy", "adopt-data.sh"),
		"--data-dir", filepath.Join(tmp, "data"), "--manifest", filepath.Join(tmp, "manifest.csv"),
		"--configs-dir", filepath.Join(tmp, "configs"), "--env-dir", filepath.Join(tmp, "etc"), "--group", g.Name).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "every instance must share") {
		t.Fatalf("want a refusal, got %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tmp, "data")); !os.IsNotExist(err) {
		t.Fatal("a refused adoption wrote something")
	}
}

// TestUnwritableDataDirRefused: a build or rollback is refused up front
// when the data directory (from MANIFEST) cannot be written.
func TestUnwritableDataDirRefused(t *testing.T) {
	// checkWritable itself: these tests run as root, which ignores
	// permissions, so use a path that cannot hold files at all.
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "builds"), []byte("not a dir"), 0o644))
	var we *WritableError
	if err := checkWritable(root); !errors.As(err, &we) || we.Dir != filepath.Join(root, "builds") {
		t.Fatalf("checkWritable: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".write-check-*")); len(left) != 0 {
		t.Fatalf("check files left behind: %v", left)
	}
	r := newRig(t)
	if err := checkWritable(r.root); err != nil {
		t.Fatalf("a writable data dir: %v", err)
	}

	// The runner refuses before planning.
	r.runner.opt.Writable = func(dir string) error { return &WritableError{Dir: dir, Err: os.ErrPermission} }
	_, err := r.runner.Plan(Request{Counts: map[string]int{"sm": 6}, Seed: 1})
	if !errors.As(err, &we) || !strings.Contains(err.Error(), "/opt/rcfg-sim/data") {
		t.Fatalf("plan: want a WritableError, got %v", err)
	}
	if _, err := r.runner.StartBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 1}); !errors.As(err, &we) {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(r.args); !os.IsNotExist(err) {
		t.Fatal("generator ran with an unwritable data directory")
	}
}

func TestHashSettings(t *testing.T) {
	l := Layout{IPBase: "10.0.0.1", IPCount: 2, PortStart: 10000, DevicesPerIP: 3}
	counts := map[string]int{"sm": 4, "md": 2, "lg": 0}
	a := HashSettings(counts, 1, map[string]string{"x": "1", "y": "2"}, l)
	// Map order, zero counts and option order do not matter.
	if b := HashSettings(map[string]int{"md": 2, "sm": 4}, 1, map[string]string{"y": "2", "x": "1"}, l); a != b {
		t.Fatal("same settings, different hash")
	}
	l2 := l
	l2.PortStart = 20000
	for name, h := range map[string]string{
		"count":  HashSettings(map[string]int{"sm": 5, "md": 2}, 1, map[string]string{"x": "1", "y": "2"}, l),
		"model":  HashSettings(map[string]int{"sm": 4, "lg": 2}, 1, map[string]string{"x": "1", "y": "2"}, l),
		"seed":   HashSettings(counts, 2, map[string]string{"x": "1", "y": "2"}, l),
		"option": HashSettings(counts, 1, map[string]string{"x": "1"}, l),
		"layout": HashSettings(counts, 1, map[string]string{"x": "1", "y": "2"}, l2),
	} {
		if h == a {
			t.Errorf("%s: changing it must change the hash", name)
		}
	}
}

func TestEnsureBuildReusesKeptBuilds(t *testing.T) {
	r := newRig(t)
	// ensure runs EnsureBuild and reports whether current changed and
	// whether the generator ran.
	ensure := func(req Request) (changed, generated bool) {
		t.Helper()
		_ = os.Remove(r.args)
		j, err := r.runner.RunJob("testbench", func(j *Job) (err error) {
			changed, err = r.runner.EnsureBuild(j, req)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, j)
		if j.Err() != nil {
			lines, _ := j.Lines(0)
			t.Fatalf("ensure: %v\n%s", j.Err(), strings.Join(lines, "\n"))
		}
		_, err = os.Stat(r.args)
		return changed, err == nil
	}

	a := Request{Counts: map[string]int{"sm": 6}, Seed: 1, Options: map[string]string{}}
	if changed, gen := ensure(a); !changed || !gen {
		t.Fatalf("first: changed %v, generated %v; want both", changed, gen)
	}
	first := r.current()
	if changed, gen := ensure(a); changed || gen || r.current() != first {
		t.Fatal("same settings again: want no change and no generation")
	}
	if len(r.restarts()) != 0 {
		t.Fatalf("EnsureBuild must not restart: %v", r.restarts())
	}

	b := Request{Counts: map[string]int{"sm": 6}, Seed: 2, Options: map[string]string{}}
	if changed, gen := ensure(b); !changed || !gen || r.current() == first {
		t.Fatalf("other settings: changed %v, generated %v; want both", changed, gen)
	}
	// Back to a: the kept build is switched to, not regenerated.
	if changed, gen := ensure(a); !changed || gen || r.current() != first {
		t.Fatalf("back to a: changed %v, generated %v, current %s; want a switch to %s", changed, gen, r.current(), first)
	}
	s, err := ReadSettings(filepath.Join(r.root, "current"))
	if err != nil || s.SettingsHash == "" || !s.LastUsed.Equal(time.Date(2026, 10, 31, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("build.json: %+v %v", s, err)
	}
	// --keep-builds 3: the live one and the two most recently used (b, B0).
	if got := len(r.builds()); got != 3 {
		t.Fatalf("builds %v, want 3", r.builds())
	}

	// KeptBuild finds builds by settings; a kept, non-live one is not live.
	if name, _, live, ok := r.runner.KeptBuild(a); !ok || !live || "builds/"+name != r.current() {
		t.Fatalf("KeptBuild(a): %s live=%v ok=%v", name, live, ok)
	}
	if _, _, live, ok := r.runner.KeptBuild(b); !ok || live {
		t.Fatalf("KeptBuild(b): live=%v ok=%v", live, ok)
	}
	if _, _, _, ok := r.runner.KeptBuild(Request{Counts: map[string]int{"sm": 6}, Seed: 3, Options: map[string]string{}}); ok {
		t.Fatal("KeptBuild for settings never built")
	}
	if _, _, ok := r.runner.BuildDir("../B0"); ok {
		t.Fatal("BuildDir accepted a path")
	}

	// StartKeptBuild generates without touching current or restarting.
	c := Request{Counts: map[string]int{"sm": 6}, Seed: 3, Options: map[string]string{}}
	j, err := r.runner.StartKeptBuild(c)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, j)
	if j.Err() != nil || r.current() != first || len(r.restarts()) != 0 {
		t.Fatalf("kept build: %v, current %s, restarts %v", j.Err(), r.current(), r.restarts())
	}
	if _, _, live, ok := r.runner.KeptBuild(c); !ok || live {
		t.Fatal("kept build not found")
	}
	// Retention removed one build to keep three, and said so.
	r.mu.Lock()
	removed := slices.Clone(r.removed)
	r.mu.Unlock()
	if len(removed) == 0 || len(r.builds()) != 3 {
		t.Fatalf("removed %v, builds %v", removed, r.builds())
	}
}
