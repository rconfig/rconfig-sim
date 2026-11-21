// Package runs keeps the record of every testbench run: a snapshot of the
// testbench as started, start and end, how it ended, the event timeline,
// and totals from the instances' counters, overall and per instance. One
// JSON file per run in --data-dir/runs (docs/UI-ARCHITECTURE.md, Phase 6).
package runs

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// How a run ended.
const (
	Running     = "running"
	Stopped     = "stopped"
	AutoStopped = "auto-stopped"
	Failed      = "failed"
)

// Event is one entry in a run's timeline.
type Event struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // start, build, restart, change, instance, running, stop, failed, ui_gap, ...
	Text string    `json:"text"`
	// Start and End bound a ui_gap: the UI was not running, so traffic in
	// between is not in the totals.
	Start time.Time `json:"start,omitzero"`
	End   time.Time `json:"end,omitzero"`
}

// UIGap is the event kind for a span the UI was down during a run.
const UIGap = "ui_gap"

// Totals are counter growth over a run.
type Totals struct {
	Sessions   map[string]float64 `json:"sessions"` // ended sessions by outcome
	Faults     map[string]float64 `json:"faults"`   // faults injected by type
	Bytes      float64            `json:"bytes"`
	PeakActive float64            `json:"peak_active"` // most sessions open at one poll
}

func newTotals() *Totals {
	t := &Totals{Sessions: map[string]float64{}, Faults: map[string]float64{}}
	for _, o := range fleet.Outcomes {
		t.Sessions[o] = 0
	}
	for _, f := range fault.AllTypes {
		t.Faults[f.String()] = 0
	}
	return t
}

func (t *Totals) add(d fleet.Counters) {
	t.Bytes += d.Bytes
	for k, v := range d.Sessions {
		t.Sessions[k] += v
	}
	for k, v := range d.Faults {
		t.Faults[k] += v
	}
}

// SessionsTotal is every ended session.
func (t Totals) SessionsTotal() float64 {
	var n float64
	for _, v := range t.Sessions {
		n += v
	}
	return n
}

// Run is one run's record.
type Run struct {
	ID            string             `json:"id"`
	TestbenchSlug string             `json:"testbench_slug"`
	Testbench     json.RawMessage    `json:"testbench"` // the testbench file as started
	Name          string             `json:"name"`      // the testbench's name and version, for lists
	Version       int                `json:"version"`
	Started       time.Time          `json:"started"`
	Ended         time.Time          `json:"ended,omitzero"`
	Outcome       string             `json:"outcome"` // Running, Stopped, AutoStopped or Failed
	Reason        string             `json:"reason,omitempty"`
	Events        []Event            `json:"events"`
	Totals        *Totals            `json:"totals"`
	Instances     map[string]*Totals `json:"instances"`
	// LastSample is the poll the totals were last added from; a UI restart
	// records the gap from here.
	LastSample time.Time `json:"last_sample,omitzero"`
	// Build and ManifestSHA256 are the build the run started on, so its
	// Devices tab shows that build while it is kept.
	Build          string `json:"build,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	// Notes and Tags are the operator's, editable after the run.
	Notes string   `json:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// MaxNotes and MaxTags bound what a run keeps.
const (
	MaxNotes = 4000
	MaxTags  = 20
)

// SetNotes sets the notes and tags, tidied: tags trimmed, empty and
// repeated ones dropped. It refuses notes over MaxNotes or more than
// MaxTags tags, or a tag over 64 characters.
func (r *Run) SetNotes(notes string, tags []string) error {
	notes = strings.TrimSpace(notes)
	if len(notes) > MaxNotes {
		return fmt.Errorf("notes are limited to %d characters", MaxNotes)
	}
	var clean []string
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || slices.Contains(clean, t) {
			continue
		}
		if len(t) > 64 {
			return fmt.Errorf("tag %q is longer than 64 characters", t[:20]+"...")
		}
		clean = append(clean, t)
	}
	if len(clean) > MaxTags {
		return fmt.Errorf("at most %d tags", MaxTags)
	}
	r.Notes, r.Tags = notes, clean
	return nil
}

// HasTag reports whether the run carries tag.
func (r *Run) HasTag(tag string) bool { return slices.Contains(r.Tags, tag) }

// SplitTags splits a comma-separated tag list.
func SplitTags(s string) []string { return strings.Split(s, ",") }

// Incomplete reports whether the totals miss traffic: the UI was down for
// part of the run (a ui_gap event).
func (r *Run) Incomplete() bool {
	for _, e := range r.Events {
		if e.Kind == UIGap {
			return true
		}
	}
	return false
}

// Duration is how long the run lasted, or has lasted so far at now.
func (r *Run) Duration(now time.Time) time.Duration {
	end := r.Ended
	if end.IsZero() {
		end = now
	}
	return end.Sub(r.Started).Round(time.Second)
}

// AddSample adds one poll's counter growth. Counter resets (an instance
// restart) are already handled in the deltas (fleet.CounterDelta).
func (r *Run) AddSample(s fleet.Sample) {
	var active float64
	for name, in := range s.Instances {
		t := r.Instances[name]
		if t == nil {
			t = newTotals()
			r.Instances[name] = t
		}
		t.add(in.Delta)
		r.Totals.add(in.Delta)
		t.PeakActive = max(t.PeakActive, in.ActiveSessions)
		active += in.ActiveSessions
	}
	r.Totals.PeakActive = max(r.Totals.PeakActive, active)
	if s.At.After(r.LastSample) {
		r.LastSample = s.At
	}
}

// Store keeps run files and the last Keep of them.
type Store struct {
	dir  string
	keep int
	now  func() time.Time

	mu sync.Mutex // one writer at a time per store
}

// NewStore returns a Store over dir keeping the last keep runs (0 means
// 200).
func NewStore(dir string, keep int, now func() time.Time) *Store {
	if keep <= 0 {
		keep = 200
	}
	if now == nil {
		now = time.Now
	}
	return &Store{dir: dir, keep: keep, now: now}
}

var idChars = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(-[0-9]+)?$`)

// New creates and saves a running run for the testbench snapshot.
func (s *Store) New(slug, name string, version int, snapshot []byte) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	base := now.Format("20060102-150405")
	id := base
	for i := 2; ; i++ {
		if _, err := os.Stat(s.path(id)); errors.Is(err, fs.ErrNotExist) {
			break
		}
		id = base + "-" + strconv.Itoa(i)
	}
	r := &Run{ID: id, TestbenchSlug: slug, Testbench: json.RawMessage(snapshot), Name: name, Version: version,
		Started: now, Outcome: Running, Totals: newTotals(), Instances: map[string]*Totals{}}
	if err := s.write(r); err != nil {
		return nil, err
	}
	s.prune(id)
	return r, nil
}

// Update changes a stored run under the store's lock.
func (s *Store) Update(id string, fn func(*Run) error) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if err := fn(r); err != nil {
		return nil, err
	}
	return r, s.write(r)
}

// Save writes r.
func (s *Store) Save(r *Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(r)
}

func (s *Store) write(r *Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := s.path(r.ID)
	tmp, err := os.CreateTemp(s.dir, ".run-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".json") }

// Get reads one run.
func (s *Store) Get(id string) (*Run, error) {
	if !idChars.MatchString(id) {
		return nil, fmt.Errorf("no run %q", id)
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no run %q", id)
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("run %s: %w", id, err)
	}
	if r.Totals == nil {
		r.Totals = newTotals()
	}
	if r.Instances == nil {
		r.Instances = map[string]*Totals{}
	}
	return &r, nil
}

// List returns every run, newest first. Unreadable files are skipped and
// reported in the error.
func (s *Store) List() ([]*Run, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var out []*Run
	var errs []error
	for _, id := range slices.Backward(ids) {
		r, err := s.Get(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, r)
	}
	return out, errors.Join(errs...)
}

// ids are the run ids on disk, oldest first (ids sort by start time).
func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok && idChars.MatchString(id) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, compareIDs)
	return ids, nil
}

// compareIDs orders ids by start time, then by their -N suffix. Every id
// starts with the 15-character start time (idChars).
func compareIDs(a, b string) int {
	if c := strings.Compare(a[:15], b[:15]); c != 0 {
		return c
	}
	an, _ := strconv.Atoi(strings.TrimPrefix(a[15:], "-"))
	bn, _ := strconv.Atoi(strings.TrimPrefix(b[15:], "-"))
	return an - bn
}

// prune deletes the oldest runs beyond Keep, never keep (the new run).
func (s *Store) prune(keep string) {
	ids, err := s.ids()
	if err != nil {
		return
	}
	for len(ids) > s.keep {
		if ids[0] != keep {
			_ = os.Remove(s.path(ids[0]))
		}
		ids = ids[1:]
	}
}

// WriteCSV writes a run's totals as CSV: one row per instance, then the
// total.
func WriteCSV(w io.Writer, r *Run) error {
	cw := csv.NewWriter(w)
	head := []string{"run", "testbench", "version", "instance"}
	for _, o := range fleet.Outcomes {
		head = append(head, "sessions_"+o)
	}
	for _, f := range fault.AllTypes {
		head = append(head, "faults_"+f.String())
	}
	head = append(head, "bytes", "peak_active")
	if err := cw.Write(head); err != nil {
		return err
	}
	row := func(name string, t *Totals) []string {
		out := []string{r.ID, r.Name, strconv.Itoa(r.Version), name}
		for _, o := range fleet.Outcomes {
			out = append(out, num(t.Sessions[o]))
		}
		for _, f := range fault.AllTypes {
			out = append(out, num(t.Faults[f.String()]))
		}
		return append(out, num(t.Bytes), num(t.PeakActive))
	}
	for _, name := range slices.Sorted(maps.Keys(r.Instances)) {
		if err := cw.Write(row(name, r.Instances[name])); err != nil {
			return err
		}
	}
	if err := cw.Write(row("total", r.Totals)); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
