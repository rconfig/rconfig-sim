package testbench

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
)

//go:embed shipped/*.json
var shippedFS embed.FS

// Store keeps testbenches as JSON files in one directory
// (--data-dir/testbenches), plus the shipped ones, which are read-only.
// Files are checked against the generator's catalogue, which also
// migrates Phase 6 files (with a build section) as they load.
type Store struct {
	dir string
	cat *catalogue.Source
	lib *scenarios.Library
}

// NewStore returns a Store over dir, created on first save.
func NewStore(dir string, cat *catalogue.Source) *Store { return &Store{dir: dir, cat: cat} }

// UseScenarios gives the store the scenario library that scenario
// testbenches are checked against (Phase 9). Without it a scenario
// testbench needs attention.
func (s *Store) UseScenarios(l *scenarios.Library) { s.lib = l }

// Scenarios is the scenario library, or nil.
func (s *Store) Scenarios() *scenarios.Library { return s.lib }

// Scenario returns the package a scenario testbench uses.
func (s *Store) Scenario(tb Testbench) (scenarios.Scenario, error) {
	r := tb.Devices.Scenario
	if tb.Devices.Source != SourceScenario || r == nil {
		return scenarios.Scenario{}, errors.New("not a scenario testbench")
	}
	if s.lib == nil {
		return scenarios.Scenario{}, errors.New("no scenario library")
	}
	return s.lib.Get(r.ID)
}

// Check fills a scenario testbench's details (name, device count, SSH
// login) from the library, and refuses one whose package is missing,
// invalid or at another version. Other testbenches pass unchanged.
func (s *Store) Check(tb *Testbench) error {
	r := tb.Devices.Scenario
	if tb.Devices.Source != SourceScenario || r == nil {
		return nil
	}
	sc, err := s.Scenario(*tb)
	if err != nil {
		return err
	}
	r.Name, r.Devices, r.SSHAuth = sc.Name, len(sc.Devices), sc.SSHAuthMode()
	if p := sc.Problem(); p != "" {
		return fmt.Errorf("scenario %s cannot be used: %s", sc.ID, p)
	}
	if sc.Version != r.Version {
		return fmt.Errorf("scenario %s is now version %d; this testbench was made for version %d. Edit and save it to use the new version", sc.ID, sc.Version, r.Version)
	}
	if a := tb.Runtime.ActiveDevices; !a.All && a.N > r.Devices {
		return fmt.Errorf("runtime.active_devices %d is more than the scenario's %d devices", a.N, r.Devices)
	}
	return nil
}

// annotate runs Check on a loaded testbench, recording a failure as its
// Problem.
func (s *Store) annotate(tb *Testbench) {
	if r := tb.Devices.Scenario; r != nil {
		c := *r // never share the ref with another copy
		tb.Devices.Scenario = &c
	}
	if err := s.Check(tb); err != nil && tb.Problem == "" {
		tb.Problem = err.Error()
	}
}

// Catalogue is the device catalogue the store checks against.
func (s *Store) Catalogue() (*catalogue.Catalogue, error) { return s.cat.Get() }

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is a testbench name's file name: lower-cased, non-alphanumerics to
// "-", so a name can never reach outside the directory.
func Slug(name string) string {
	s := strings.Trim(slugChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "testbench"
	}
	return s
}

// Shipped returns the embedded testbenches, in file order, checked
// against cat. One the catalogue does not support (an older generator)
// still loads, with Problem set.
func Shipped(cat *catalogue.Catalogue) []Testbench {
	entries, _ := fs.ReadDir(shippedFS, "shipped")
	var out []Testbench
	for _, e := range entries {
		b, _ := shippedFS.ReadFile(path.Join("shipped", e.Name()))
		tb := load(b, strings.TrimSuffix(e.Name(), ".json"), cat)
		tb.Shipped = true
		out = append(out, tb)
	}
	return out
}

// Decode reads one testbench, refusing unknown fields, migrates a Phase 6
// build section to a device source, and validates it. Import uses it, so
// it is strict; stored testbenches load through load instead.
func Decode(b []byte, cat *catalogue.Catalogue) (Testbench, error) {
	tb, err := decode(b, cat)
	if err != nil {
		return Testbench{}, err
	}
	return tb, nil
}

// decode is Decode, but returns as much of the testbench as it read
// alongside the error.
func decode(b []byte, cat *catalogue.Catalogue) (Testbench, error) {
	var tb Testbench
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tb); err != nil {
		// Keep what a lenient read gets (the name, at least) for the list.
		var loose Testbench
		if json.Unmarshal(b, &loose) != nil {
			loose = Testbench{}
		}
		loose.Name = strings.TrimSpace(loose.Name)
		return loose, fmt.Errorf("not a testbench: %w", err)
	}
	if dec.More() {
		return Testbench{}, errors.New("not a testbench: trailing data after the JSON object")
	}
	tb.Name = strings.TrimSpace(tb.Name)
	if tb.Build != nil {
		if tb.Devices.Source != "" {
			return tb, errors.New("not a testbench: both the old build section and devices")
		}
		d, err := migrate(*tb.Build, cat)
		if d.Source != "" {
			tb.Devices, tb.Build = d, nil
		}
		if err != nil {
			return tb, err
		}
	}
	return tb, Validate(tb, cat)
}

// load reads a stored testbench leniently: one this catalogue no longer
// accepts (an unknown generator option, say) still loads, with Problem
// saying why, so the list shows it under Needs attention rather than
// dropping it. It cannot start until it is fixed and saved.
func load(b []byte, slug string, cat *catalogue.Catalogue) Testbench {
	tb, err := decode(b, cat)
	if err != nil {
		tb.Problem = err.Error()
	}
	if tb.Name == "" {
		tb.Name = slug
	}
	tb.Slug = slug
	return tb
}

// shippedSlugs lists the shipped testbenches' slugs, whatever the
// catalogue supports, so their names stay reserved.
func shippedSlugs(*catalogue.Catalogue) []string {
	entries, _ := fs.ReadDir(shippedFS, "shipped")
	var out []string
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".json"))
	}
	return out
}

// Encode is a testbench's file and export form.
func Encode(tb Testbench) []byte {
	if tb.Runtime.Faults == nil {
		tb.Runtime.Faults = map[string]float64{}
	}
	if tb.Runtime.Profiles.Rules == nil {
		tb.Runtime.Profiles.Rules = []string{}
	}
	b, _ := json.MarshalIndent(tb, "", "  ")
	return append(b, '\n')
}

// List returns the shipped testbenches, then the saved ones by name.
// A file that does not validate is listed with Problem set; one that
// cannot be read at all is skipped and reported in the error, returned
// alongside whatever did load.
func (s *Store) List() ([]Testbench, error) {
	cat, err := s.cat.Get()
	if err != nil {
		return nil, err
	}
	out := Shipped(cat)
	for i := range out {
		s.annotate(&out[i])
	}
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return out, err
	}
	var saved []Testbench
	var errs []error
	for _, f := range files {
		tb, err := readFile(f, cat)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.annotate(&tb)
		saved = append(saved, tb)
	}
	slices.SortFunc(saved, func(a, b Testbench) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return append(out, saved...), errors.Join(errs...)
}

// Get returns the testbench with this slug.
func (s *Store) Get(slug string) (Testbench, error) {
	cat, err := s.cat.Get()
	if err != nil {
		return Testbench{}, err
	}
	for _, tb := range Shipped(cat) {
		if tb.Slug == slug {
			s.annotate(&tb)
			return tb, nil
		}
	}
	if slug != Slug(slug) {
		return Testbench{}, fmt.Errorf("no testbench %q", slug)
	}
	tb, err := readFile(filepath.Join(s.dir, slug+".json"), cat)
	if errors.Is(err, fs.ErrNotExist) {
		return Testbench{}, fmt.Errorf("no testbench %q", slug)
	}
	if err == nil {
		s.annotate(&tb)
	}
	return tb, err
}

// ErrExists is returned when a new testbench's name is taken.
var ErrExists = errors.New("a testbench with that name already exists")

// Create saves a new testbench (Save as, Duplicate, Import). Its version
// is kept if set, else 1.
func (s *Store) Create(tb Testbench) (Testbench, error) {
	tb.Name = strings.TrimSpace(tb.Name)
	slug := Slug(tb.Name)
	if s.taken(slug) {
		return Testbench{}, fmt.Errorf("%w: %q", ErrExists, tb.Name)
	}
	if tb.Version < 1 {
		tb.Version = 1
	}
	return tb, s.write(slug, &tb)
}

// Save replaces the saved testbench at slug with tb and bumps its version.
// A changed name renames the file, refused if the new name is taken.
// Shipped testbenches cannot be saved; Duplicate them.
func (s *Store) Save(slug string, tb Testbench) (Testbench, error) {
	old, err := s.Get(slug)
	if err != nil {
		return Testbench{}, err
	}
	if old.Shipped {
		return Testbench{}, fmt.Errorf("%q is a shipped testbench and is read-only; duplicate it to change it", old.Name)
	}
	tb.Name = strings.TrimSpace(tb.Name)
	newSlug := Slug(tb.Name)
	if newSlug != slug && s.taken(newSlug) {
		return Testbench{}, fmt.Errorf("%w: %q", ErrExists, tb.Name)
	}
	tb.Version = max(old.Version, tb.Version) + 1
	if err := s.write(newSlug, &tb); err != nil {
		return Testbench{}, err
	}
	if newSlug != slug {
		if err := os.Remove(filepath.Join(s.dir, slug+".json")); err != nil {
			return tb, fmt.Errorf("saved as %s, but could not remove the old file: %w", newSlug, err)
		}
	}
	return tb, nil
}

// Delete removes a saved testbench.
func (s *Store) Delete(slug string) error {
	tb, err := s.Get(slug)
	if err != nil {
		return err
	}
	if tb.Shipped {
		return fmt.Errorf("%q is a shipped testbench and cannot be deleted", tb.Name)
	}
	return os.Remove(filepath.Join(s.dir, slug+".json"))
}

// DuplicateName is a free name for a copy of name.
func (s *Store) DuplicateName(name string) string {
	for i := 1; ; i++ {
		n := name + " (copy)"
		if i > 1 {
			n = fmt.Sprintf("%s (copy %d)", name, i)
		}
		if len(n) > 64 {
			n = fmt.Sprintf("Copy %d of %s", i, name)
			n = n[:min(len(n), 64)]
		}
		if !s.taken(Slug(n)) {
			return n
		}
	}
}

func (s *Store) taken(slug string) bool {
	cat, _ := s.cat.Get()
	for _, tb := range shippedSlugs(cat) {
		if tb == slug {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(s.dir, slug+".json"))
	return err == nil
}

// write validates tb and writes it atomically (temp file, rename).
func (s *Store) write(slug string, tb *Testbench) error {
	cat, err := s.cat.Get()
	if err != nil {
		return err
	}
	if err := Validate(*tb, cat); err != nil {
		return err
	}
	if err := s.Check(tb); err != nil {
		return err
	}
	tb.Slug, tb.Shipped, tb.Problem = slug, false, ""
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(s.dir, slug+".json"), Encode(*tb))
}

// writeAtomic writes b to p through a temp file in the same directory and
// a rename, so a reader never sees half a file.
func writeAtomic(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func readFile(p string, cat *catalogue.Catalogue) (Testbench, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return Testbench{}, err
	}
	return load(b, strings.TrimSuffix(filepath.Base(p), ".json"), cat), nil
}
