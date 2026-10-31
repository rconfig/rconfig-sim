// Package presets stores named failure presets: a set of fault rates plus
// device profile rules and seed. Three read-only presets ship in the binary;
// presets the operator saves are JSON files in <data-dir>/presets/.
package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
)

// Preset is one named set of faults and profiles.
type Preset struct {
	Name     string             `json:"name"`
	Faults   map[string]float64 `json:"faults"`
	Profiles Profiles           `json:"profiles"`
	BuiltIn  bool               `json:"-"`
}

// Profiles is a --device-profiles rule list and its seed.
type Profiles struct {
	Seed  int64    `json:"seed"`
	Rules []string `json:"rules"`
}

// builtIns are the presets in section 10.2 of docs/UI-ARCHITECTURE.md.
var builtIns = []Preset{
	{Name: "Flaky WAN", Faults: map[string]float64{"disconnect_mid": 0.03, "slow_response": 0.05},
		Profiles: Profiles{Seed: 42, Rules: []string{"latency:300-1500ms@15%"}}},
	{Name: "Auth storm", Faults: map[string]float64{"auth_fail": 0.15},
		Profiles: Profiles{Seed: 42, Rules: []string{}}},
	{Name: "Slow core", Faults: map[string]float64{"slow_response": 0.02},
		Profiles: Profiles{Seed: 42, Rules: []string{"latency:2000-8000ms@5%", "hang:600s@1%"}}},
}

// Store reads and writes presets.
type Store struct {
	dir string
}

// NewStore returns a Store keeping saved presets in dir (created on first
// save).
func NewStore(dir string) *Store { return &Store{dir: dir} }

// Validate checks a preset's name, fault rates and profile rules.
func Validate(p Preset) error {
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 64 {
		return errors.New("preset name must be 1 to 64 characters")
	}
	if _, err := fault.NewSetFromRates(p.Faults); err != nil {
		return err
	}
	for _, r := range p.Profiles.Rules {
		if strings.Contains(r, ",") || strings.TrimSpace(r) == "" {
			return fmt.Errorf("rule %q: want one profile@percent item", r)
		}
	}
	_, err := profile.Parse(strings.Join(p.Profiles.Rules, ","))
	return err
}

// List returns the built-in presets, then the saved ones sorted by name.
// Unreadable files are skipped and reported in the error, which is
// returned alongside whatever did load.
func (s *Store) List() ([]Preset, error) {
	out := slices.Clone(builtIns)
	for i := range out {
		out[i].BuiltIn = true
	}
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return out, err
	}
	var saved []Preset
	var errs []error
	for _, f := range files {
		p, err := readFile(f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		saved = append(saved, p)
	}
	slices.SortFunc(saved, func(a, b Preset) int { return strings.Compare(a.Name, b.Name) })
	return append(out, saved...), errors.Join(errs...)
}

// Get returns the preset called name.
func (s *Store) Get(name string) (Preset, bool) {
	all, _ := s.List()
	for _, p := range all {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// Save writes p, replacing a saved preset of the same name. Built-in names
// are refused.
func (s *Store) Save(p Preset) error {
	p.Name = strings.TrimSpace(p.Name)
	if isBuiltIn(p.Name) {
		return fmt.Errorf("%q is a built-in preset; choose another name", p.Name)
	}
	if p.Profiles.Rules == nil {
		p.Profiles.Rules = []string{}
	}
	if err := Validate(p); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".preset-*")
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
	return os.Rename(tmp.Name(), s.path(p.Name))
}

// Delete removes a saved preset. Built-ins cannot be deleted.
func (s *Store) Delete(name string) error {
	if isBuiltIn(name) {
		return fmt.Errorf("%q is a built-in preset", name)
	}
	err := os.Remove(s.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no saved preset %q", name)
	}
	return err
}

func isBuiltIn(name string) bool {
	for _, b := range builtIns {
		if strings.EqualFold(b.Name, name) {
			return true
		}
	}
	return false
}

var unsafeChars = regexp.MustCompile(`[^a-z0-9]+`)

// path maps a name to a file: lower-cased, non-alphanumerics to "-", so
// the name can never reach outside the directory.
func (s *Store) path(name string) string {
	slug := strings.Trim(unsafeChars.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "preset"
	}
	return filepath.Join(s.dir, slug+".json")
}

func readFile(path string) (Preset, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Preset{}, err
	}
	var p Preset
	if err := json.Unmarshal(b, &p); err != nil {
		return Preset{}, fmt.Errorf("%s: %w", path, err)
	}
	if p.Profiles.Rules == nil {
		p.Profiles.Rules = []string{}
	}
	if err := Validate(p); err != nil {
		return Preset{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}
