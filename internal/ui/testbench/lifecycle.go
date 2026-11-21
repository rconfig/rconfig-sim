package testbench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/runs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
)

// Lifecycle states, as the header pill shows them. Idle means no active
// testbench (no active.json).
const (
	Idle     = "idle"
	Starting = "starting"
	Running  = "running"
	Stopping = "stopping"
	Failed   = "failed"
)

// ActiveRun is the active testbench, persisted in --data-dir/active.json so
// a UI restart resumes it.
type ActiveRun struct {
	Slug     string          `json:"slug"`
	Name     string          `json:"name"`
	Version  int             `json:"version"`
	Snapshot json.RawMessage `json:"snapshot"` // the testbench as started
	RunID    string          `json:"run_id"`
	State    string          `json:"state"`
	Started  time.Time       `json:"started"`
	StopAt   time.Time       `json:"stop_at,omitzero"` // auto-stop; zero for none
	Modified bool            `json:"modified,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Elapsed is how long the testbench has been active at now.
func (a ActiveRun) Elapsed(now time.Time) time.Duration {
	if a.Started.IsZero() {
		return 0
	}
	return now.Sub(a.Started).Round(time.Second)
}

// Testbench decodes the snapshot.
func (a ActiveRun) Testbench(cat *catalogue.Catalogue) (Testbench, error) {
	tb, err := Decode(a.Snapshot, cat)
	tb.Slug = a.Slug
	return tb, err
}

// ErrActive refuses a start while another testbench is active; the caller
// asks the operator, then starts again with replace.
var ErrActive = errors.New("another testbench is active")

// ErrIdle refuses an action that needs an active testbench.
var ErrIdle = errors.New("no testbench is active")

// ManagerOptions configure a Manager.
type ManagerOptions struct {
	DataDir string // active.json lives here
	Store   *Store
	Runs    *runs.Store
	Jobs    *jobs.Runner
	Fleet   *fleet.Fleet
	Units   *systemd.Ctl
	// StartTimeout bounds the wait for every instance to match after a
	// start (--start-timeout). Zero means 120s. It starts after any rolling
	// restart.
	StartTimeout time.Duration
	// WaitEvery is how often the wait polls the fleet. Zero means 1s.
	WaitEvery time.Duration
	// SaveEvery is how often a running run's totals are written. Zero
	// means 30s.
	SaveEvery time.Duration
	Logger    *slog.Logger
	Now       func() time.Time
}

// Manager runs at most one active testbench.
type Manager struct {
	opt ManagerOptions

	mu       sync.Mutex
	active   *ActiveRun // nil when idle
	run      *runs.Run  // the active run's record; nil when idle or after a failure
	timer    *time.Timer
	lastSave time.Time
	lastUp   map[string]bool // instance up at the last poll, for instance events
}

// NewManager returns a Manager and resumes active.json, if any: a running
// testbench keeps its run and auto-stop timer (stopping at once if the time
// has passed); one caught mid-start or mid-stop is marked failed.
func NewManager(opt ManagerOptions) *Manager {
	if opt.StartTimeout <= 0 {
		opt.StartTimeout = 120 * time.Second
	}
	if opt.WaitEvery <= 0 {
		opt.WaitEvery = time.Second
	}
	if opt.SaveEvery <= 0 {
		opt.SaveEvery = 30 * time.Second
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	m := &Manager{opt: opt}
	m.resume()
	opt.Fleet.Observe(m.observe)
	return m
}

func (m *Manager) activePath() string { return filepath.Join(m.opt.DataDir, "active.json") }

func (m *Manager) resume() {
	b, err := os.ReadFile(m.activePath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			m.opt.Logger.Error("read active testbench", "err", err)
		}
		return
	}
	var a ActiveRun
	if err := json.Unmarshal(b, &a); err != nil {
		m.opt.Logger.Error("read active testbench", "path", m.activePath(), "err", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = &a
	if r, err := m.opt.Runs.Get(a.RunID); err == nil && r.Outcome == runs.Running {
		m.run = r
	}
	switch a.State {
	case Running:
		if m.run != nil {
			// Traffic while the UI was down is not in the totals.
			from := m.run.LastSample
			if from.IsZero() {
				from = m.run.Started
			}
			now := m.opt.Now().UTC()
			m.run.Events = append(m.run.Events, runs.Event{At: now, Kind: runs.UIGap, Start: from, End: now,
				Text: fmt.Sprintf("The UI was not running from %s to %s; traffic in between is not in the totals",
					from.Format(time.RFC3339), now.Format(time.RFC3339))})
		}
		m.event("resume", "UI restarted; the testbench is still active")
		m.opt.Logger.Info("testbench resumed", "testbench", a.Name, "run", a.RunID)
		if !a.StopAt.IsZero() {
			m.armTimer(a.StopAt)
		}
	case Starting, Stopping:
		m.failLocked(fmt.Sprintf("the UI restarted while the testbench was %s; check the fleet, then start or stop it again", a.State))
	}
}

// Active returns the active testbench; State is Idle when there is none.
func (m *Manager) Active() ActiveRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return ActiveRun{State: Idle}
	}
	return *m.active
}

// Start starts the testbench at slug as a job (section Phase 6, item 3).
// With another testbench active it returns ErrActive unless replace is
// set, in which case the job stops that one first.
func (m *Manager) Start(slug string, replace bool) (*jobs.Job, error) {
	tb, err := m.opt.Store.Get(slug)
	if err != nil {
		return nil, err
	}
	if tb.Problem != "" {
		return nil, fmt.Errorf("%q needs attention: %s", tb.Name, tb.Problem)
	}
	st := m.opt.Jobs.Status()
	if why := Fit(tb, st.Layout, st.Err); len(why) > 0 {
		return nil, fmt.Errorf("%q does not fit this host: %s", tb.Name, strings.Join(why, "; "))
	}
	cur := m.Active()
	if (cur.State == Starting || cur.State == Stopping) || (cur.State != Idle && !replace) {
		return nil, fmt.Errorf("%w: %s (%s)", ErrActive, cur.Name, cur.State)
	}
	return m.opt.Jobs.RunJob("testbench start", func(j *jobs.Job) error {
		if cur.State != Idle {
			j.Logf("Stopping %s first", cur.Name)
			if err := m.stop(j, runs.Stopped, "stopped to start "+tb.Name); err != nil {
				return err
			}
		}
		return m.start(j, tb)
	})
}

func (m *Manager) start(j *jobs.Job, tb Testbench) error {
	snap := Encode(tb)
	r, err := m.opt.Runs.New(tb.Slug, tb.Name, tb.Version, snap)
	if err != nil {
		return fmt.Errorf("create run record: %w", err)
	}
	now := m.opt.Now().UTC()
	m.mu.Lock()
	m.active = &ActiveRun{Slug: tb.Slug, Name: tb.Name, Version: tb.Version, Snapshot: snap, RunID: r.ID, State: Starting, Started: now}
	m.run = r
	m.lastUp = nil
	m.event("start", fmt.Sprintf("Starting %s v%d", tb.Name, tb.Version))
	m.persistLocked()
	m.mu.Unlock()

	err = m.bringUp(j, tb)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.failLocked(err.Error())
		return err
	}
	m.active.State = Running
	if d := tb.DurationValue(); d > 0 {
		m.active.StopAt = m.opt.Now().UTC().Add(d)
		m.armTimer(m.active.StopAt)
		m.event("running", fmt.Sprintf("Running; auto-stop at %s", m.active.StopAt.Format(time.RFC3339)))
	} else {
		m.event("running", "Running")
	}
	m.persistLocked()
	j.Logf("%s is running", tb.Name)
	return nil
}

// bringUp is start steps (a) to (d).
func (m *Manager) bringUp(j *jobs.Job, tb Testbench) error {
	// (a) Switch to, build or install the matching build.
	changed, keys, err := m.ensureBuild(j, tb)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	st := m.opt.Jobs.Status()
	if st.Err != nil {
		return st.Err
	}
	if changed {
		m.eventf("build", "Build %s is live", st.Live.Name)
	} else {
		m.eventf("build", "Build %s already matches", st.Live.Name)
	}
	// The run records its build, so its Devices tab shows it while kept.
	sha, err := fileSHA256(st.Layout.Manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	m.mu.Lock()
	if m.run != nil {
		m.run.Build, m.run.ManifestSHA256 = st.Live.Name, sha
	}
	m.mu.Unlock()

	// (b) Apply runtime values and overrides through the fan-out.
	want, err := Resolve(tb, st.Layout)
	if err != nil {
		return err
	}
	ctx := j.Context()
	m.opt.Fleet.Poll(ctx)
	var down, up, unused []string
	for _, v := range m.opt.Fleet.Views() {
		if _, ok := want[v.Name]; !ok {
			if v.Status == fleet.StatusUnused {
				unused = append(unused, v.Name)
			}
			continue
		}
		switch v.Status {
		case fleet.StatusDown:
			down = append(down, v.Name)
		case fleet.StatusUp:
			up = append(up, v.Name)
		default:
			return fmt.Errorf("instance %s is %s: %s", v.Name, v.Status, v.Detail)
		}
	}
	// Instances this testbench does not use are stopped: rcfg-sim will not
	// run without devices (Phase 9).
	for _, n := range unused {
		if err := m.opt.Units.Do(ctx, "stop", n); err != nil {
			return fmt.Errorf("stop %s: %w", n, err)
		}
	}
	if len(unused) > 0 {
		j.Logf("Not used by this testbench, stopped: %s", strings.Join(unused, ", "))
		m.eventf("instance", "Not used by this testbench: %s", strings.Join(unused, ", "))
	}
	var bad []string
	for _, res := range m.opt.Fleet.ApplyInstances(ctx, want) {
		if !res.OK() {
			bad = append(bad, fmt.Sprintf("%s: %v", res.Instance, errors.Join(res.Err, res.EnvErr)))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("apply settings: %s", strings.Join(bad, "; "))
	}
	j.Logf("Settings applied to %d instances (%d down: env file only)", len(want), len(down))
	m.eventf("apply", "Settings applied to %d instances", len(want))
	// The SSH login (and for a scenario its commands and passwords) are not
	// runtime settings: write them to the env files, and restart the up
	// instances they changed on (Phase 8 item 3, Phase 9 item 4).
	if tb.Devices.Source == SourceScenario {
		if err := m.saveOwned(j, st.Layout.EnvDir, st.Layout.Instances); err != nil {
			return fmt.Errorf("save env keys before the scenario: %w", err)
		}
	}
	keysChanged, err := m.opt.Fleet.SetKeys(st.Layout.Instances, keys)
	if err != nil {
		return fmt.Errorf("env files: %w", err)
	}
	if len(keysChanged) > 0 {
		j.Logf("%s set on %s (restart needed)", keyNames(keys), strings.Join(keysChanged, ", "))
		m.eventf("apply", "%s set on %d instances", keyNames(keys), len(keysChanged))
	}
	if tb.Devices.Source != SourceScenario {
		restored, err := m.restoreOwned(j, st.Layout.Instances)
		if err != nil {
			return fmt.Errorf("put back env keys after a scenario: %w", err)
		}
		if len(restored) > 0 {
			m.eventf("apply", "Put back the pre-scenario commands and passwords on %d instances", len(restored))
			for _, n := range restored {
				if !slices.Contains(keysChanged, n) {
					keysChanged = append(keysChanged, n)
				}
			}
		}
	}

	// (c) Start the down instances; rolling-restart the rest if the build
	// changed.
	for _, n := range down {
		j.Logf("Starting %s", n)
		if err := m.opt.Units.Do(ctx, "start", n); err != nil {
			return fmt.Errorf("start %s: %w", n, err)
		}
	}
	if len(down) > 0 {
		m.eventf("instance", "Started %s", strings.Join(down, ", "))
	}
	restart := up
	if !changed {
		restart = intersect(up, keysChanged)
	}
	if len(restart) > 0 {
		if err := m.opt.Jobs.RollingRestart(j, restart); err != nil {
			return fmt.Errorf("rolling restart: %w", err)
		}
		why := "the new build"
		if !changed {
			why = "the " + keyNames(keys) + " change"
		}
		m.eventf("restart", "Rolling restart of %d instances for %s", len(restart), why)
	}

	// (d) Wait until every instance is up, matches, and shows no drift.
	return m.waitMatch(j, want)
}

// ensureBuild makes tb's build live (step a) and returns whether current
// changed and the env keys the instances need that the admin API cannot
// set. A generated fleet switches to or generates its build; a scenario
// installs its package as a build (Phase 9 item 4) and owns COMMANDS_ROOT,
// PASSWORD and ENABLE_PASSWORD while it runs; bringUp saves their values
// first and the next generated start puts them back (see owned.go).
// Every start sets ALLOW_EMPTY=true.
func (m *Manager) ensureBuild(j *jobs.Job, tb Testbench) (bool, map[string]string, error) {
	before := m.opt.Jobs.Status()
	if before.Err != nil {
		return false, nil, before.Err
	}
	// ALLOW_EMPTY on every instance, so one left without devices idles
	// rather than failing after a reboot (Phase 9 item 5).
	keys := map[string]string{envfile.SSHAuth: tb.Devices.SSHAuth(), envfile.AllowEmpty: "true"}
	if tb.Devices.Source == SourceScenario {
		sc, err := m.opt.Store.Scenario(tb)
		if err != nil {
			return false, nil, err
		}
		if p := sc.Problem(); p != "" {
			return false, nil, fmt.Errorf("scenario %s cannot be used: %s", sc.ID, p)
		}
		l := before.Layout
		hash, err := scenarios.Hash(sc, l)
		if err != nil {
			return false, nil, fmt.Errorf("scenario %s: %w", sc.ID, err)
		}
		next := jobs.Settings{Count: len(sc.Devices), IPBase: l.IPBase, IPCount: l.IPCount, PortStart: l.PortStart,
			DevicesPerIP: l.DevicesPerIP, SettingsHash: hash, Scenario: fmt.Sprintf("%s v%d", sc.ID, sc.Version)}
		changed, err := m.opt.Jobs.InstallBuild(j, scenarios.BuildName(sc, hash), next, func(dir string, l jobs.Layout) error {
			return scenarios.Install(sc, dir, l)
		})
		if err != nil {
			return false, nil, err
		}
		maps.Copy(keys, ScenarioKeys(sc, l))
		return changed, keys, nil
	}
	cat, err := m.opt.Store.Catalogue()
	if err != nil {
		return false, nil, err
	}
	changed, err := m.opt.Jobs.EnsureBuild(j, tb.Devices.Request(cat))
	if err != nil {
		return false, nil, err
	}
	return changed, keys, nil
}

// ScenarioKeys are the env keys a scenario testbench runs with: its SSH
// login, its command files (through current, so they follow the live
// build) and, for any_password, empty passwords.
func ScenarioKeys(sc scenarios.Scenario, l jobs.Layout) map[string]string {
	pw, en := sc.Credentials()
	keys := map[string]string{envfile.SSHAuth: sc.SSHAuthMode(), envfile.Password: pw, envfile.EnablePassword: en, envfile.CommandsRoot: ""}
	if len(sc.Commands) > 0 {
		keys[envfile.CommandsRoot] = filepath.Join(l.Root, "current", "commands")
	}
	return keys
}

// keyNames is the env keys of a change, for the log.
func keyNames(keys map[string]string) string {
	names := slices.DeleteFunc(slices.Sorted(maps.Keys(keys)), func(k string) bool { return k == envfile.AllowEmpty })
	if len(names) == 1 && names[0] == envfile.SSHAuth {
		return "SSH login"
	}
	return strings.Join(names, ", ")
}

// waitMatch polls until every instance in want is up, its live settings
// match its entry and it shows no drift, or StartTimeout passes.
func (m *Manager) waitMatch(j *jobs.Job, want map[string]fleet.Desired) error {
	ctx := j.Context()
	deadline := time.Now().Add(m.opt.StartTimeout)
	j.Logf("Waiting up to %s for every instance to match", m.opt.StartTimeout)
	for {
		m.opt.Fleet.Poll(ctx)
		miss := mismatches(m.opt.Fleet.Views(), want)
		if len(miss) == 0 {
			j.Logf("All %d instances match", len(want))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s these instances did not match: %s", m.opt.StartTimeout, strings.Join(miss, "; "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.opt.WaitEvery):
		}
	}
}

// waitSettled polls until every named instance is up and shows no drift
// against its env file, or StartTimeout passes.
func (m *Manager) waitSettled(j *jobs.Job, names []string) error {
	ctx := j.Context()
	deadline := time.Now().Add(m.opt.StartTimeout)
	j.Logf("Waiting up to %s for every instance to be up with no drift", m.opt.StartTimeout)
	for {
		m.opt.Fleet.Poll(ctx)
		var miss []string
		views := map[string]fleet.View{}
		for _, v := range m.opt.Fleet.Views() {
			views[v.Name] = v
		}
		for _, n := range names {
			v, ok := views[n]
			switch {
			case !ok:
				miss = append(miss, n+" is missing")
			case v.Status != fleet.StatusUp:
				miss = append(miss, fmt.Sprintf("%s is %s", n, v.Status))
			case len(v.Drift) > 0:
				miss = append(miss, fmt.Sprintf("%s drifts from its env file (%s)", n, strings.Join(v.Drift, ", ")))
			}
		}
		if len(miss) == 0 {
			j.Logf("All %d instances are up with no drift", len(names))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("after %s: %s", m.opt.StartTimeout, strings.Join(miss, "; "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.opt.WaitEvery):
		}
	}
}

// mismatches lists each instance in want that is not up, differs from its
// entry, or drifts from its env file.
func mismatches(views []fleet.View, want map[string]fleet.Desired) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range views {
		w, ok := want[v.Name]
		if !ok {
			continue
		}
		seen[v.Name] = true
		switch {
		case v.Status != fleet.StatusUp:
			out = append(out, fmt.Sprintf("%s is %s", v.Name, v.Status))
		case len(fleet.Drift(w, *v.State)) > 0:
			out = append(out, fmt.Sprintf("%s live %s differs from the testbench", v.Name, strings.Join(fleet.Drift(w, *v.State), ", ")))
		case len(v.Drift) > 0:
			out = append(out, fmt.Sprintf("%s drifts from its env file (%s)", v.Name, strings.Join(v.Drift, ", ")))
		}
	}
	for _, n := range slices.Sorted(maps.Keys(want)) {
		if !seen[n] {
			out = append(out, n+" is missing")
		}
	}
	return out
}

// Stop stops every instance and closes the run, as a job.
func (m *Manager) Stop() (*jobs.Job, error) {
	if a := m.Active(); a.State == Idle {
		return nil, ErrIdle
	}
	return m.opt.Jobs.RunJob("testbench stop", func(j *jobs.Job) error {
		return m.stop(j, runs.Stopped, "")
	})
}

func (m *Manager) stop(j *jobs.Job, outcome, why string) error {
	m.mu.Lock()
	if m.active == nil {
		m.mu.Unlock()
		return nil
	}
	name := m.active.Name
	m.active.State = Stopping
	m.stopTimer()
	m.persistLocked()
	m.mu.Unlock()

	st := m.opt.Jobs.Status()
	var errs []error
	if st.Err == nil {
		for _, n := range st.Layout.Instances {
			j.Logf("Stopping %s", n)
			if err := m.opt.Units.Do(j.Context(), "stop", n); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w", n, err))
			}
		}
	} else {
		errs = append(errs, st.Err)
	}
	m.opt.Fleet.Poll(j.Context())

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := errors.Join(errs...); err != nil {
		m.event("stop", "Stop incomplete: "+err.Error())
	}
	text := "Stopped"
	if outcome == runs.AutoStopped {
		text = "Auto-stopped after the testbench duration"
	}
	if why != "" {
		text += ": " + why
	}
	m.event("stop", text)
	m.closeRunLocked(outcome, "")
	m.active = nil
	if err := os.Remove(m.activePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.opt.Logger.Error("remove active testbench", "err", err)
	}
	j.Logf("%s stopped", name)
	return errors.Join(errs...)
}

// Restart rolling-restarts every instance with the same testbench and
// records it in the same run.
func (m *Manager) Restart() (*jobs.Job, error) {
	if a := m.Active(); a.State != Running {
		return nil, fmt.Errorf("%w (state %s)", ErrIdle, a.State)
	}
	return m.opt.Jobs.RunJob("testbench restart", func(j *jobs.Job) error {
		m.event("restart", "Rolling restart requested")
		if err := m.opt.Jobs.RollingRestart(j, nil); err != nil {
			m.event("restart", "Rolling restart stopped: "+err.Error())
			return err
		}
		// The env files hold any changes made during the run, so wait for
		// every instance to be up with no drift against its env file, not
		// for the saved testbench's values.
		st := m.opt.Jobs.Status()
		if st.Err != nil {
			return st.Err
		}
		insts, _ := fleet.Discover(st.Layout.EnvDir)
		used, _ := fleet.Split(st.Layout.Manifest, insts, st.Layout.Instances)
		if err := m.waitSettled(j, used); err != nil {
			m.event("restart", "Rolling restart done, but "+err.Error())
			return err
		}
		m.event("restart", "Rolling restart done")
		return nil
	})
}

// Changed records a setting change made during the run (Fleet or Failures
// screen) and marks the testbench modified.
func (m *Manager) Changed(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || (m.active.State != Running && m.active.State != Failed) {
		return
	}
	m.event("change", text)
	if !m.active.Modified {
		m.active.Modified = true
		m.persistLocked()
	}
}

// Revert re-applies the active testbench's settings, undoing changes made
// during the run, as a job.
func (m *Manager) Revert() (*jobs.Job, error) {
	a := m.Active()
	if a.State != Running {
		return nil, fmt.Errorf("%w (state %s)", ErrIdle, a.State)
	}
	cat, err := m.opt.Store.Catalogue()
	if err != nil {
		return nil, err
	}
	tb, err := a.Testbench(cat)
	if err != nil {
		return nil, err
	}
	if err := m.opt.Store.Check(&tb); err != nil {
		return nil, err
	}
	return m.opt.Jobs.RunJob("testbench revert", func(j *jobs.Job) error {
		st := m.opt.Jobs.Status()
		if st.Err != nil {
			return st.Err
		}
		want, err := Resolve(tb, st.Layout)
		if err != nil {
			return err
		}
		m.opt.Fleet.Poll(j.Context())
		var bad []string
		for _, res := range m.opt.Fleet.ApplyInstances(j.Context(), want) {
			if !res.OK() {
				bad = append(bad, fmt.Sprintf("%s: %v", res.Instance, errors.Join(res.Err, res.EnvErr)))
			}
		}
		if len(bad) > 0 {
			return fmt.Errorf("revert: %s", strings.Join(bad, "; "))
		}
		m.mu.Lock()
		m.event("change", "Reverted to the testbench's settings")
		if m.active != nil {
			m.active.Modified = false
			m.persistLocked()
		}
		m.mu.Unlock()
		return nil
	})
}

// SaveToTestbench writes the fleet's current settings (from the env files)
// back into the active testbench and bumps its version. A shipped
// testbench is saved as a new copy instead. It returns the saved
// testbench.
func (m *Manager) SaveToTestbench() (Testbench, error) {
	a := m.Active()
	if a.State != Running {
		return Testbench{}, fmt.Errorf("%w (state %s)", ErrIdle, a.State)
	}
	cat, err := m.opt.Store.Catalogue()
	if err != nil {
		return Testbench{}, err
	}
	tb, err := a.Testbench(cat)
	if err != nil {
		return Testbench{}, err
	}
	if err := m.opt.Store.Check(&tb); err != nil {
		return Testbench{}, err
	}
	st := m.opt.Jobs.Status()
	if st.Err != nil {
		return Testbench{}, st.Err
	}
	insts, err := fleet.Discover(st.Layout.EnvDir)
	if err != nil {
		return Testbench{}, err
	}
	cur := map[string]fleet.Desired{}
	devices := instanceDevices(tb.Devices.Total(), st.Layout)
	for _, in := range insts {
		if devices[in.Name] == 0 {
			continue // not used by this testbench
		}
		d, err := fleet.DesiredFromEnv(in)
		if err != nil {
			return Testbench{}, fmt.Errorf("%s: %w", in.EnvPath, err)
		}
		cur[in.Name] = d
	}
	tb.Runtime, tb.Overrides = FromInstances(cur, devices, st.Layout.Instances)

	var saved Testbench
	if orig, err := m.opt.Store.Get(a.Slug); err == nil && !orig.Shipped {
		saved, err = m.opt.Store.Save(a.Slug, tb)
		if err != nil {
			return Testbench{}, err
		}
	} else {
		tb.Name = m.opt.Store.DuplicateName(tb.Name)
		tb.Version = 0
		if saved, err = m.opt.Store.Create(tb); err != nil {
			return Testbench{}, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		m.active.Slug, m.active.Name, m.active.Version = saved.Slug, saved.Name, saved.Version
		m.active.Snapshot = Encode(saved)
		m.active.Modified = false
		m.event("change", fmt.Sprintf("Saved to testbench %s v%d", saved.Name, saved.Version))
		m.persistLocked()
	}
	return saved, nil
}

// FromInstances turns per-instance settings back into fleet-wide runtime
// values and overrides: the runtime is the first instance's settings, and
// every instance that differs gets an override for just what differs.
// Active devices become "all" when every instance has all its devices
// active, else the fleet total, with overrides where the split differs.
func FromInstances(cur map[string]fleet.Desired, devices map[string]int, order []string) (Runtime, map[string]Override) {
	var names []string
	for _, n := range order {
		if _, ok := cur[n]; ok {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return Runtime{Faults: map[string]float64{}, Profiles: Profiles{Rules: []string{}}, ActiveDevices: Active{All: true}}, nil
	}
	first := cur[names[0]]
	rt := Runtime{
		Faults:      nonZero(first.Faults),
		Profiles:    Profiles{Seed: first.ProfileSeed, Rules: rules(first.ProfileRules)},
		DelayMS:     DelayMS{Min: first.DelayMinMS, Max: first.DelayMaxMS},
		MaxSessions: first.MaxSessions,
	}
	all, total := true, 0
	weights := make([]int, len(names))
	for i, n := range names {
		total += cur[n].ActiveDevices
		weights[i] = devices[n]
		if cur[n].ActiveDevices != devices[n] {
			all = false
		}
	}
	split := weights
	if all {
		rt.ActiveDevices = Active{All: true}
	} else {
		rt.ActiveDevices = Active{N: total}
		split, _ = fleet.SplitActive(total, weights)
	}
	overrides := map[string]Override{}
	for i, n := range names {
		d := cur[n]
		var o Override
		set := false
		if !sameRates(d.Faults, rt.Faults) {
			o.Faults, set = nonZero(d.Faults), true
		}
		if d.ProfileSeed != rt.Profiles.Seed || !slices.Equal(rules(d.ProfileRules), rt.Profiles.Rules) {
			o.Profiles, set = &Profiles{Seed: d.ProfileSeed, Rules: rules(d.ProfileRules)}, true
		}
		if d.DelayMinMS != rt.DelayMS.Min || d.DelayMaxMS != rt.DelayMS.Max {
			o.DelayMS, set = &DelayMS{Min: d.DelayMinMS, Max: d.DelayMaxMS}, true
		}
		if d.MaxSessions != rt.MaxSessions {
			ms := d.MaxSessions
			o.MaxSessions, set = &ms, true
		}
		if split != nil && d.ActiveDevices != split[i] {
			o.ActiveDevices, set = &Active{N: d.ActiveDevices}, true
		}
		if set {
			overrides[n] = o
		}
	}
	if len(overrides) == 0 {
		overrides = nil
	}
	return rt, overrides
}

func nonZero(m map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

func sameRates(a, b map[string]float64) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// Annotate sets the active run's notes and tags; false when id is not the
// active run (the caller then edits the stored run).
func (m *Manager) Annotate(id, notes string, tags []string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run == nil || m.run.ID != id {
		return false, nil
	}
	if err := m.run.SetNotes(notes, tags); err != nil {
		return true, err
	}
	return true, m.opt.Runs.Save(m.run)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Run returns a copy of the active run's record, or nil.
func (m *Manager) Run() *runs.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run == nil {
		return nil
	}
	r := *m.run
	return &r
}

// observe adds each poll's counters to the active run and records
// instances going down or coming back while it runs.
func (m *Manager) observe(s fleet.Sample) {
	views := m.opt.Fleet.Views()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.run == nil || m.active == nil {
		return
	}
	m.run.AddSample(s)
	// The gap a later resume records is measured on the manager's clock.
	m.run.LastSample = m.opt.Now().UTC()
	if m.active.State == Running {
		up := map[string]bool{}
		for _, v := range views {
			up[v.Name] = v.Status == fleet.StatusUp
			if was, ok := m.lastUp[v.Name]; ok && was != up[v.Name] {
				if up[v.Name] {
					m.event("instance", v.Name+" is up again")
				} else {
					m.event("instance", fmt.Sprintf("%s is %s: %s", v.Name, v.Status, v.Detail))
				}
			}
		}
		m.lastUp = up
	}
	if now := m.opt.Now(); now.Sub(m.lastSave) >= m.opt.SaveEvery {
		m.lastSave = now
		if err := m.opt.Runs.Save(m.run); err != nil {
			m.opt.Logger.Error("save run", "run", m.run.ID, "err", err)
		}
	}
}

func (m *Manager) armTimer(at time.Time) {
	m.stopTimer()
	d := max(at.Sub(m.opt.Now()), 0)
	m.timer = time.AfterFunc(d, m.autoStop)
}

func (m *Manager) stopTimer() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
}

// autoStop stops the testbench when its duration is up. If another job is
// running it tries again shortly.
func (m *Manager) autoStop() {
	a := m.Active()
	if a.State != Running {
		return
	}
	_, err := m.opt.Jobs.RunJob("testbench auto-stop", func(j *jobs.Job) error {
		return m.stop(j, runs.AutoStopped, "")
	})
	if err != nil {
		m.opt.Logger.Warn("auto-stop deferred", "err", err)
		m.mu.Lock()
		if m.active != nil && m.active.State == Running {
			m.timer = time.AfterFunc(5*time.Second, m.autoStop)
		}
		m.mu.Unlock()
	}
}

// failLocked marks the active testbench failed and closes its run.
func (m *Manager) failLocked(why string) {
	if m.active == nil {
		return
	}
	m.stopTimer()
	m.active.State, m.active.Error = Failed, why
	m.event("failed", why)
	m.closeRunLocked(runs.Failed, why)
	m.persistLocked()
	m.opt.Logger.Warn("testbench failed", "testbench", m.active.Name, "err", why)
}

func (m *Manager) closeRunLocked(outcome, reason string) {
	if m.run == nil {
		return
	}
	m.run.Ended, m.run.Outcome, m.run.Reason = m.opt.Now().UTC(), outcome, reason
	if err := m.opt.Runs.Save(m.run); err != nil {
		m.opt.Logger.Error("save run", "run", m.run.ID, "err", err)
	}
	m.run = nil
}

// Dismiss clears a failed testbench without stopping anything.
func (m *Manager) Dismiss() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil || m.active.State != Failed {
		return fmt.Errorf("only a failed testbench can be dismissed")
	}
	m.active = nil
	if err := os.Remove(m.activePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) eventf(kind, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event(kind, fmt.Sprintf(format, args...))
}

// event appends to the active run's timeline. Called with m.mu held.
func (m *Manager) event(kind, text string) {
	if m.run == nil {
		return
	}
	m.run.Events = append(m.run.Events, runs.Event{At: m.opt.Now().UTC(), Kind: kind, Text: text})
	if err := m.opt.Runs.Save(m.run); err != nil {
		m.opt.Logger.Error("save run", "run", m.run.ID, "err", err)
	}
}

// persistLocked writes active.json atomically. Called with m.mu held.
func (m *Manager) persistLocked() {
	if m.active == nil {
		return
	}
	b, err := json.MarshalIndent(m.active, "", "  ")
	if err == nil {
		if err = os.MkdirAll(m.opt.DataDir, 0o750); err == nil {
			err = writeAtomic(m.activePath(), append(b, '\n'))
		}
	}
	if err != nil {
		m.opt.Logger.Error("write active testbench", "err", err)
	}
}

// Close stops the auto-stop timer and saves the active run's totals, so
// a later resume records the gap from now.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopTimer()
	if m.run != nil {
		if err := m.opt.Runs.Save(m.run); err != nil {
			m.opt.Logger.Error("save run", "run", m.run.ID, "err", err)
		}
	}
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}
