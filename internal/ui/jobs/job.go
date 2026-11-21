package jobs

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
)

// Options configure a Runner.
type Options struct {
	EnvDir  string
	GenBin  string
	LogDir  string // job logs, <data-dir>/jobs
	Units   *systemd.Ctl
	Healthy func(ctx context.Context, instance string) error
	Pause   time.Duration // between instances in the rolling restart; zero means 5s
	Wait    time.Duration // for each instance's /healthz; zero means 60s
	Logger  *slog.Logger
	Now     func() time.Time                 // tests
	Free    func(path string) (int64, error) // tests; default FreeBytes
	// Writable checks the data directory before a build or rollback.
	// Tests only; default checkWritable.
	Writable func(root string) error
	// Catalogue is the generator's device catalogue (D14): the models a
	// build may ask for and their average sizes. Required.
	Catalogue *catalogue.Source
	// KeepBuilds is how many builds retention keeps (--keep-builds). Zero
	// means 3. The live build is always kept.
	KeepBuilds int
	// Removed, when set, is told the name of each build retention deletes
	// (the Devices screen drops its index of it).
	Removed func(name string)
}

// Runner runs one job at a time.
type Runner struct {
	opt    Options
	ctx    context.Context
	cancel context.CancelFunc

	mu  sync.Mutex
	job *Job // the latest job, running or finished
}

// NewRunner returns a Runner. Close stops a running job.
func NewRunner(opt Options) *Runner {
	if opt.Pause <= 0 {
		opt.Pause = 5 * time.Second
	}
	if opt.Wait <= 0 {
		opt.Wait = 60 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Free == nil {
		opt.Free = FreeBytes
	}
	if opt.Writable == nil {
		opt.Writable = checkWritable
	}
	if opt.KeepBuilds <= 0 {
		opt.KeepBuilds = 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{opt: opt, ctx: ctx, cancel: cancel}
}

// Close cancels a running job and waits for it.
func (r *Runner) Close() {
	r.cancel()
	if j := r.Current(); j != nil {
		<-j.done
	}
}

// Current returns the latest job, or nil.
func (r *Runner) Current() *Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}

// Job returns the latest job if its id matches.
func (r *Runner) Job(id string) (*Job, bool) {
	j := r.Current()
	if j == nil || j.ID != id {
		return nil, false
	}
	return j, true
}

// Running reports whether a job is in progress.
func (r *Runner) Running() bool {
	j := r.Current()
	return j != nil && !j.Finished()
}

// Status is the Build screen's view of the install.
type Status struct {
	Layout   Layout
	Live     *Settings // the live build's build.json
	Previous *Settings // the build rollback would go to; nil if none
	Err      error     // layout problem, *NotAdoptedError, or unreadable build
}

// Status reads the layout and the live and previous builds.
func (r *Runner) Status() Status {
	var st Status
	insts, err := fleet.Discover(r.opt.EnvDir)
	if err != nil {
		st.Err = err
		return st
	}
	st.Layout, st.Err = LayoutOf(insts, r.opt.EnvDir)
	if st.Err != nil {
		return st
	}
	live, err := ReadSettings(filepath.Join(st.Layout.Root, "current"))
	if err != nil {
		st.Err = fmt.Errorf("live build: %w", err)
		return st
	}
	st.Live = live
	if live.Previous != "" {
		if prev, err := ReadSettings(filepath.Join(st.Layout.Root, "builds", live.Previous)); err == nil {
			st.Previous = prev
		}
	}
	return st
}

// Request is what the Build screen (or a testbench) asks for: exactly how
// many devices of each model, and the seed.
type Request struct {
	Counts map[string]int
	Seed   int64
	// Distribution is what the Build screen showed, kept in build.json for
	// the next form; it does not change the build.
	Distribution string
	// Options are the other rcfg-sim-gen flags. Nil carries over the live
	// build's, as the Build screen does; a testbench passes its own.
	Options map[string]string
}

// Plan is a validated build, nothing written yet.
type Plan struct {
	Layout    Layout
	Live      *Settings
	Next      Settings
	Creds     Credentials
	Estimate  Estimate
	Free      int64
	Restart   time.Duration // the rolling restart's pauses, at least
	Unknowns  bool          // the live build's seed or distribution is unknown (adopted)
	Instances int
}

// Plan validates a request against the layout, the live build and the
// disk, in the order of section 7.5, and returns the plan or the first
// reason to refuse. Nothing is written.
func (r *Runner) Plan(req Request) (*Plan, error) {
	return r.plan(req, true)
}

// plan is Plan, with the disk check optional: reusing a kept build needs
// no space.
func (r *Runner) plan(req Request, checkDisk bool) (*Plan, error) {
	st := r.Status()
	if st.Err != nil {
		return nil, st.Err
	}
	l := st.Layout
	if err := r.opt.Writable(l.Root); err != nil {
		return nil, err
	}
	cat, err := r.opt.Catalogue.Get()
	if err != nil {
		return nil, err
	}
	total, err := cat.CheckCounts(req.Counts)
	if err != nil {
		return nil, err
	}
	if err := l.CheckCount(total); err != nil {
		return nil, err
	}
	creds, err := r.generatedCredentials(l, st.Live)
	if err != nil {
		return nil, err
	}
	p := &Plan{
		Layout: l, Live: st.Live, Creds: creds, Instances: l.IPCount,
		Unknowns: st.Live.Scenario == "" && (st.Live.Seed == nil || (st.Live.Distribution == "" && st.Live.Counts == nil)),
		Restart:  time.Duration(l.IPCount-1) * r.opt.Pause,
	}
	seed := req.Seed
	opts := maps.Clone(st.Live.Options)
	if req.Options != nil {
		opts = maps.Clone(req.Options)
	}
	p.Next = Settings{
		Count: total, Seed: &seed, Distribution: req.Distribution, Counts: nonZero(req.Counts),
		IPBase: l.IPBase, IPCount: l.IPCount, PortStart: l.PortStart, DevicesPerIP: l.DevicesPerIP,
		Options: opts,
	}
	p.Next.SettingsHash = HashSettings(req.Counts, seed, opts, l)
	p.Estimate = EstimateBuild(req.Counts, cat)
	if !checkDisk {
		return p, nil
	}
	free, err := r.opt.Free(l.Root)
	if err != nil {
		return nil, fmt.Errorf("free space on %s: %w", l.Root, err)
	}
	p.Free = free
	if free < p.Estimate.NeedFree {
		return p, &DiskError{Need: p.Estimate.NeedFree, Free: free, Path: l.Root}
	}
	return p, nil
}

// generatedCredentials are the logins a new generated build writes: the
// live build's, so rConfig's stored credentials keep working. A scenario's
// manifest has its own (often empty) credentials, so while a scenario is
// live they come from the most recently used generated build, or the
// generator's defaults when none is kept.
func (r *Runner) generatedCredentials(l Layout, live *Settings) (Credentials, error) {
	if live.Scenario == "" {
		c, err := credentialsOf(l.Manifest)
		if err != nil {
			return Credentials{}, fmt.Errorf("live manifest: %w", err)
		}
		return c, nil
	}
	entries, _ := os.ReadDir(filepath.Join(l.Root, "builds"))
	var best *Settings
	for _, e := range entries {
		s, err := ReadSettings(filepath.Join(l.Root, "builds", e.Name()))
		if err != nil || s.Scenario != "" || (best != nil && !s.LastUsed.After(best.LastUsed)) {
			continue
		}
		best = s
	}
	if best != nil {
		if c, err := credentialsOf(filepath.Join(l.Root, "builds", best.Name, "manifest.csv")); err == nil {
			return c, nil
		}
	}
	return DefaultCredentials, nil
}

// DiskError refuses a build the filesystem cannot hold.
type DiskError struct {
	Need, Free int64
	Path       string
}

func (e *DiskError) Error() string {
	return fmt.Sprintf("not enough disk on %s: the build needs about %s (estimate plus 10%%), %s is free. Free space, or roll back and let retention remove old builds",
		e.Path, HumanBytes(e.Need), HumanBytes(e.Free))
}

// HumanBytes formats a byte count.
func HumanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ErrBusy refuses a job while another runs.
var ErrBusy = errors.New("a build or rollback is already running")

// StartBuild plans the request and, if it passes, runs it in the
// background.
func (r *Runner) StartBuild(req Request) (*Job, error) {
	if r.Running() {
		return nil, ErrBusy
	}
	p, err := r.Plan(req)
	if err != nil {
		return nil, err
	}
	return r.start("build", func(j *Job) error { return r.build(j, p) })
}

// StartRollback points current back at the previous build and restarts
// the fleet, in the background.
func (r *Runner) StartRollback() (*Job, error) {
	if r.Running() {
		return nil, ErrBusy
	}
	st := r.Status()
	if st.Err != nil {
		return nil, st.Err
	}
	if st.Previous == nil {
		return nil, errors.New("no previous build to roll back to")
	}
	if err := r.opt.Writable(st.Layout.Root); err != nil {
		return nil, err
	}
	return r.start("rollback", func(j *Job) error { return r.rollback(j, st) })
}

func (r *Runner) start(kind string, run func(*Job) error) (*Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.job != nil && !r.job.Finished() {
		return nil, ErrBusy
	}
	j, err := newJob(kind, r.opt.Now(), r.opt.LogDir)
	if err != nil {
		return nil, err
	}
	j.ctx, j.cancel = context.WithCancel(r.ctx)
	r.job = j
	go func() {
		err := run(j)
		j.finish(err)
		r.opt.Logger.Info("job finished", "id", j.ID, "kind", kind, "err", err)
	}()
	return j, nil
}

// build runs section 7.5 steps 5 to 8.
func (r *Runner) build(j *Job, p *Plan) error {
	l := p.Layout
	j.Logf("Build of %d devices (%s), seed %d, across %d instances", p.Next.Count, countsText(p.Next.Counts), *p.Next.Seed, l.IPCount)
	j.Logf("Estimate: %s on disk, %s of configs, about %s to generate", HumanBytes(p.Estimate.DiskBytes), HumanBytes(p.Estimate.ConfigBytes), p.Estimate.Generate.Round(time.Second))

	name, err := r.generateLive(j, p)
	if err != nil {
		return err
	}
	j.setRollback(true)

	if err := r.restart(j, l); err != nil {
		return fmt.Errorf("rolling restart stopped: %w. Roll back to return the fleet to builds/%s", err, p.Live.Name)
	}
	r.retain(j, l.Root, name)
	j.Logf("Build %s is live", name)
	return nil
}

// generateLive generates p into a new build directory and makes it live
// (swaps current), without restarting anything.
func (r *Runner) generateLive(j *Job, p *Plan) (string, error) {
	l := p.Layout
	name, dir, err := r.generateKept(j, p, p.Live.Name)
	if err != nil {
		return "", err
	}
	if err := swapCurrent(l.Root, name); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("swap current: %w", err)
	}
	r.touchOutgoing(j, l.Root, p.Live.Name)
	j.Logf("current -> builds/%s (was builds/%s)", name, p.Live.Name)
	return name, nil
}

// generateKept generates p into a new build directory with its build.json,
// leaving current alone. previous is recorded for rollback.
func (r *Runner) generateKept(j *Job, p *Plan, previous string) (string, string, error) {
	name, dir, err := r.newBuildDir(p.Layout.Root)
	if err != nil {
		return "", "", err
	}
	now := r.opt.Now().UTC()
	p.Next.Name, p.Next.Created, p.Next.LastUsed, p.Next.Previous = name, now, now, previous
	j.Logf("Generating into %s", dir)
	if err := r.generate(j, p, dir); err != nil {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			j.Logf("Could not remove %s: %v", dir, rmErr)
		}
		return "", "", fmt.Errorf("generator failed, nothing changed (the live build is untouched): %w", err)
	}
	if err := writeSettings(dir, &p.Next); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("write build.json: %w", err)
	}
	return name, dir, nil
}

// StartKeptBuild generates the build req describes and keeps it without
// making it live or restarting anything, so a testbench's Devices tab can
// show its devices before it runs. Nothing is generated when a kept build
// already has these settings.
func (r *Runner) StartKeptBuild(req Request) (*Job, error) {
	if _, err := r.plan(req, true); err != nil {
		return nil, err
	}
	return r.start("build (kept, not live)", func(j *Job) error {
		p, err := r.plan(req, true)
		if err != nil {
			return err
		}
		if kept := r.findKept(p.Layout.Root, p.Next.SettingsHash); kept != "" {
			j.Logf("builds/%s already has these settings", kept)
			return nil
		}
		name, _, err := r.generateKept(j, p, "")
		if err != nil {
			return err
		}
		r.retain(j, p.Layout.Root, p.Live.Name)
		j.Logf("Build %s is kept; the live build is still %s", name, p.Live.Name)
		return nil
	})
}

// KeptBuild returns the kept build whose settings req describes, its
// directory, and whether it is the live one. ok is false when no kept
// build has those settings.
func (r *Runner) KeptBuild(req Request) (name, dir string, live, ok bool) {
	p, err := r.plan(req, false)
	if err != nil {
		return "", "", false, false
	}
	if p.Live.SettingsHash == p.Next.SettingsHash {
		return p.Live.Name, filepath.Join(p.Layout.Root, "current"), true, true
	}
	if kept := r.findKept(p.Layout.Root, p.Next.SettingsHash); kept != "" {
		return kept, filepath.Join(p.Layout.Root, "builds", kept), false, true
	}
	return "", "", false, false
}

// BuildDir returns a kept build's directory by name, and whether it is the
// live build. ok is false when it is no longer kept.
func (r *Runner) BuildDir(name string) (dir string, live, ok bool) {
	st := r.Status()
	if st.Err != nil || name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", false, false
	}
	if st.Live != nil && st.Live.Name == name {
		return filepath.Join(st.Layout.Root, "current"), true, true
	}
	dir = filepath.Join(st.Layout.Root, "builds", name)
	if _, err := os.Stat(filepath.Join(dir, "manifest.csv")); err != nil {
		return "", false, false
	}
	return dir, false, true
}

// EnsureBuild makes the build req describes live, without restarting:
// nothing when the live build has the same settings hash, a switch to a
// kept build that has it, or a new generation otherwise (Phase 6 item 2).
// It reports whether current changed. Run it inside a job (RunJob).
func (r *Runner) EnsureBuild(j *Job, req Request) (changed bool, err error) {
	p, err := r.plan(req, false)
	if err != nil {
		return false, err
	}
	l, want := p.Layout, p.Next.SettingsHash
	if p.Live.SettingsHash == want {
		j.Logf("Build %s already has these settings", p.Live.Name)
		_ = r.touch(l.Root, p.Live.Name, "")
		return false, nil
	}
	if kept := r.findKept(l.Root, want); kept != "" {
		if err := swapCurrent(l.Root, kept); err != nil {
			return false, fmt.Errorf("swap current: %w", err)
		}
		r.touchOutgoing(j, l.Root, p.Live.Name)
		if err := r.touch(l.Root, kept, p.Live.Name); err != nil {
			j.Logf("build.json of %s: %v", kept, err)
		}
		j.Logf("current -> builds/%s, a kept build with these settings (no generation)", kept)
		r.retain(j, l.Root, kept)
		return true, nil
	}
	if p, err = r.plan(req, true); err != nil {
		return false, err
	}
	name, err := r.generateLive(j, p)
	if err != nil {
		return false, err
	}
	r.retain(j, l.Root, name)
	return true, nil
}

// InstallBuild makes a build that is not generated live, without
// restarting, as a scenario testbench start does (Phase 9): nothing when
// the live build has settings hash next.SettingsHash, a switch to a kept
// build that has it, or else a new build directory builds/<name> that
// install fills (copying files and writing manifest.csv), recorded with
// next as its build.json. It reports whether current changed. Run it
// inside a job (RunJob).
func (r *Runner) InstallBuild(j *Job, name string, next Settings, install func(dir string, l Layout) error) (changed bool, err error) {
	st := r.Status()
	if st.Err != nil {
		return false, st.Err
	}
	l, want := st.Layout, next.SettingsHash
	if err := r.opt.Writable(l.Root); err != nil {
		return false, err
	}
	if st.Live.SettingsHash == want {
		j.Logf("Build %s already has these settings", st.Live.Name)
		_ = r.touch(l.Root, st.Live.Name, "")
		return false, nil
	}
	if kept := r.findKept(l.Root, want); kept != "" {
		if err := swapCurrent(l.Root, kept); err != nil {
			return false, fmt.Errorf("swap current: %w", err)
		}
		r.touchOutgoing(j, l.Root, st.Live.Name)
		if err := r.touch(l.Root, kept, st.Live.Name); err != nil {
			j.Logf("build.json of %s: %v", kept, err)
		}
		j.Logf("current -> builds/%s, a kept build with these settings (nothing installed)", kept)
		r.retain(j, l.Root, kept)
		return true, nil
	}
	if name == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." || name == st.Live.Name {
		return false, fmt.Errorf("bad build name %q", name)
	}
	dir := filepath.Join(l.Root, "builds", name)
	// A leftover with this name but no matching build.json (a failed
	// install) is replaced.
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	j.Logf("Installing into %s", dir)
	fail := func(err error) (bool, error) {
		_ = os.RemoveAll(dir)
		return false, err
	}
	if err := install(dir, l); err != nil {
		return fail(fmt.Errorf("install failed, nothing changed (the live build is untouched): %w", err))
	}
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	}); err != nil {
		return fail(fmt.Errorf("permissions: %w", err))
	}
	now := r.opt.Now().UTC()
	next.Name, next.Created, next.LastUsed, next.Previous = name, now, now, st.Live.Name
	if err := writeSettings(dir, &next); err != nil {
		return fail(fmt.Errorf("write build.json: %w", err))
	}
	if err := swapCurrent(l.Root, name); err != nil {
		return fail(fmt.Errorf("swap current: %w", err))
	}
	r.touchOutgoing(j, l.Root, st.Live.Name)
	j.Logf("current -> builds/%s (was builds/%s)", name, st.Live.Name)
	r.retain(j, l.Root, name)
	return true, nil
}

// findKept returns a kept build whose settings hash is want, or "".
func (r *Runner) findKept(root, want string) string {
	entries, err := os.ReadDir(filepath.Join(root, "builds"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if s, err := ReadSettings(filepath.Join(root, "builds", e.Name())); err == nil && s.SettingsHash == want {
			return e.Name()
		}
	}
	return ""
}

// touch marks a build as just used, and when previous is set records it as
// the build to roll back to.
func (r *Runner) touch(root, name, previous string) error {
	dir := filepath.Join(root, "builds", name)
	s, err := ReadSettings(dir)
	if err != nil {
		return err
	}
	s.LastUsed = r.opt.Now().UTC()
	if previous != "" && previous != name {
		s.Previous = previous
	}
	return writeSettings(dir, s)
}

// touchOutgoing marks the build that was live until now as just used, so
// retention keeps it for a rollback.
func (r *Runner) touchOutgoing(j *Job, root, name string) {
	if name == "" {
		return
	}
	if err := r.touch(root, name, ""); err != nil && !errors.Is(err, fs.ErrNotExist) {
		j.Logf("build.json of %s: %v", name, err)
	}
}

// RunJob runs fn as a job under the one-job-at-a-time rule shared with
// builds and rollbacks (testbench lifecycle actions use it).
func (r *Runner) RunJob(kind string, fn func(*Job) error) (*Job, error) {
	return r.start(kind, fn)
}

// RollingRestart restarts the named instances one at a time, waiting for
// each to answer /healthz, inside a job. Nil names means every instance of
// the current layout.
func (r *Runner) RollingRestart(j *Job, names []string) error {
	st := r.Status()
	if st.Err != nil {
		return st.Err
	}
	l := st.Layout
	if names != nil {
		l.Instances = names
	}
	return r.restart(j, l)
}

// newBuildDir makes data/builds/<timestamp>, readable by rcfg-sim.
func (r *Runner) newBuildDir(root string) (name, dir string, err error) {
	base := r.opt.Now().UTC().Format("20060102-1504")
	builds := filepath.Join(root, "builds")
	for i := 1; ; i++ {
		name = base
		if i > 1 {
			name = base + "-" + strconv.Itoa(i)
		}
		dir = filepath.Join(builds, name)
		err = os.Mkdir(dir, 0o755)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		return name, dir, os.Chmod(dir, 0o755)
	}
}

// generate runs rcfg-sim-gen into dir and checks what it wrote.
func (r *Runner) generate(j *Job, p *Plan, dir string) error {
	n := p.Next
	args := []string{
		"--counts", countsText(n.Counts),
		"--seed", strconv.FormatInt(*n.Seed, 10),
		"--output-dir", filepath.Join(dir, "configs"),
		"--manifest", filepath.Join(dir, "manifest.csv"),
		"--ip-base", n.IPBase,
		"--ip-count", strconv.Itoa(n.IPCount),
		"--port-start", strconv.Itoa(n.PortStart),
		"--devices-per-ip", strconv.Itoa(n.DevicesPerIP),
		"--username", p.Creds.Username,
		"--password", p.Creds.Password,
		"--enable-password", p.Creds.EnablePassword,
	}
	keys := slices.Sorted(maps.Keys(n.Options))
	for _, k := range keys {
		args = append(args, "--"+k, n.Options[k])
	}
	cmd := exec.CommandContext(j.ctx, r.opt.GenBin, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		j.Logf("  %s", sc.Text())
	}
	if err := cmd.Wait(); err != nil {
		return err
	}
	// Build output must be readable by rcfg-sim whatever this process's
	// umask (the controller unit runs with UMask=0007).
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o755)
		}
		return os.Chmod(path, 0o644)
	}); err != nil {
		return fmt.Errorf("permissions: %w", err)
	}
	rows, err := countRows(filepath.Join(dir, "manifest.csv"))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if rows != n.Count {
		return fmt.Errorf("manifest has %d devices, want %d", rows, n.Count)
	}
	return nil
}

func countRows(manifest string) (int, error) {
	f, err := os.Open(manifest)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := -1 // header
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return max(n, 0), sc.Err()
}

// swapCurrent points root/current at builds/name atomically: a temp
// symlink renamed over it.
func swapCurrent(root, name string) error {
	var b [6]byte
	_, _ = rand.Read(b[:])
	tmp := filepath.Join(root, ".current-"+hex.EncodeToString(b[:]))
	if err := os.Symlink(filepath.Join("builds", name), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, "current")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// restart does the rolling restart (section 7.4), logging each step.
// Instances with no devices in the live manifest are not used by it: they
// are stopped instead, since rcfg-sim will not start without devices.
func (r *Runner) restart(j *Job, l Layout) error {
	insts, _ := fleet.Discover(r.opt.EnvDir)
	used, unused := fleet.Split(l.Manifest, insts, l.Instances)
	for _, n := range unused {
		if state, err := r.opt.Units.IsActive(j.ctx, n); err == nil && state == "inactive" {
			continue
		}
		j.Logf("  %s: not used by this build, stopping", n)
		if err := r.opt.Units.Do(j.ctx, "stop", n); err != nil {
			return fmt.Errorf("stop %s: %w", n, err)
		}
	}
	j.Logf("Rolling restart of %d instances (pause %s)", len(used), r.opt.Pause)
	return r.opt.Units.RollingRestart(j.ctx, used, r.opt.Wait, r.opt.Pause, r.opt.Healthy,
		func(s systemd.RollingStep) {
			if s.Err != nil {
				j.Logf("  %s: %s: %v", s.Instance, s.Phase, s.Err)
			} else {
				j.Logf("  %s: %s", s.Instance, s.Phase)
			}
		})
}

// retain keeps --keep-builds builds, the most recently used, and deletes
// the rest, least recently used first. live is never deleted. Run only
// after a successful build or switch.
func (r *Runner) retain(j *Job, root, live string) {
	entries, err := os.ReadDir(filepath.Join(root, "builds"))
	if err != nil {
		j.Logf("Retention: %v", err)
		return
	}
	type built struct {
		name string
		used time.Time
	}
	var all []built
	for _, e := range entries {
		if !e.IsDir() || e.Name() == live {
			continue
		}
		b := built{name: e.Name()}
		if s, err := ReadSettings(filepath.Join(root, "builds", e.Name())); err == nil {
			b.used = s.LastUsed
			if b.used.IsZero() {
				b.used = s.Created
			}
		}
		all = append(all, b)
	}
	// Most recently used first; the live build takes one of the places.
	slices.SortStableFunc(all, func(a, b built) int { return b.used.Compare(a.used) })
	for i, b := range all {
		if i < r.opt.KeepBuilds-1 {
			continue
		}
		p := filepath.Join(root, "builds", b.name)
		if err := os.RemoveAll(p); err != nil {
			j.Logf("Retention: could not remove %s: %v", p, err)
			continue
		}
		j.Logf("Retention: removed builds/%s (least recently used)", b.name)
		if r.opt.Removed != nil {
			r.opt.Removed(b.name)
		}
	}
}

// rollback points current at the previous build and restarts the fleet.
func (r *Runner) rollback(j *Job, st Status) error {
	l := st.Layout
	j.Logf("Rollback from builds/%s to builds/%s", st.Live.Name, st.Previous.Name)
	if err := swapCurrent(l.Root, st.Previous.Name); err != nil {
		return fmt.Errorf("swap current: %w", err)
	}
	j.Logf("current -> builds/%s", st.Previous.Name)
	if err := r.touch(l.Root, st.Previous.Name, ""); err != nil {
		j.Logf("build.json of %s: %v", st.Previous.Name, err)
	}
	if err := r.restart(j, l); err != nil {
		return fmt.Errorf("rolling restart stopped: %w", err)
	}
	j.Logf("Build %s is live again; builds/%s is kept", st.Previous.Name, st.Live.Name)
	return nil
}

// Job is one build or rollback: its log, streamed to the browser, and its
// outcome.
type Job struct {
	ID      string
	Kind    string // "build" or "rollback"
	Started time.Time

	mu       sync.Mutex
	lines    []string
	err      error
	finished bool
	rollback bool // the fleet may be on a new build: offer rollback
	done     chan struct{}
	logFile  *os.File
	ctx      context.Context // ended by Runner.Close or when the job finishes
	cancel   context.CancelFunc
}

func newJob(kind string, now time.Time, logDir string) (*Job, error) {
	j := &Job{ID: now.UTC().Format("20060102-150405") + "-" + kind, Kind: kind, Started: now, done: make(chan struct{})}
	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o750); err != nil {
			return nil, fmt.Errorf("job log: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(logDir, j.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, fmt.Errorf("job log: %w", err)
		}
		j.logFile = f
	}
	return j, nil
}

// Logf appends a line to the job log.
func (j *Job) Logf(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
	if j.logFile != nil {
		_, _ = io.WriteString(j.logFile, line+"\n")
	}
}

func (j *Job) setRollback(v bool) {
	j.mu.Lock()
	j.rollback = v
	j.mu.Unlock()
}

func (j *Job) finish(err error) {
	if err != nil {
		j.Logf("FAILED: %v", err)
	} else {
		j.Logf("Done")
	}
	j.mu.Lock()
	j.err, j.finished = err, true
	if err == nil {
		j.rollback = false
	}
	if j.logFile != nil {
		_ = j.logFile.Close()
	}
	j.mu.Unlock()
	j.cancel()
	close(j.done)
}

// Lines returns the log lines from index from on, and whether the job has
// finished (so no more will come).
func (j *Job) Lines(from int) ([]string, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if from > len(j.lines) {
		from = len(j.lines)
	}
	return slices.Clone(j.lines[from:]), j.finished
}

// Finished reports whether the job is over.
func (j *Job) Finished() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished
}

// Err is the job's failure, nil while running or on success.
func (j *Job) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// OfferRollback reports whether a failed build left the fleet on the new
// build, so the screen should offer rollback.
func (j *Job) OfferRollback() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.finished && j.err != nil && j.rollback
}

// Done is closed when the job finishes.
func (j *Job) Done() <-chan struct{} { return j.done }

// Context ends when the runner closes; long steps inside a job use it.
func (j *Job) Context() context.Context { return j.ctx }

// countsText is counts as the generator's --counts takes them, in a stable
// (sorted) order.
func countsText(counts map[string]int) string {
	var parts []string
	for _, m := range slices.Sorted(maps.Keys(counts)) {
		if counts[m] > 0 {
			parts = append(parts, m+":"+strconv.Itoa(counts[m]))
		}
	}
	return strings.Join(parts, ",")
}

func nonZero(counts map[string]int) map[string]int {
	out := map[string]int{}
	for m, n := range counts {
		if n > 0 {
			out[m] = n
		}
	}
	return out
}
