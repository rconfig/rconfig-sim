// Package catalogue is the UI's view of the generator's device catalogue
// (D14): families, models with their options, and drivers, read from
// `rcfg-sim-gen catalogue --json`. The UI never hardcodes a model list; a
// model added to the generator appears here with no UI change.
package catalogue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Family is a device family: ip or optical (D13).
type Family struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Option is a generator option that applies to a model.
type Option struct {
	Name    string `json:"name"`
	Flag    string `json:"flag"`
	Type    string `json:"type"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Default int    `json:"default"`
	Help    string `json:"help"`
}

// FlagName is the option's flag without its dashes, as build.json keeps
// generator options.
func (o Option) FlagName() string { return strings.TrimLeft(o.Flag, "-") }

// Model is one generator model.
type Model struct {
	ID          string   `json:"id"`
	Family      string   `json:"family"`
	Vendor      string   `json:"vendor"`
	Driver      string   `json:"driver"`
	Protocol    string   `json:"protocol"`
	Description string   `json:"description"`
	AvgBytes    int64    `json:"avg_bytes"`
	Options     []Option `json:"options"`
}

// Driver is one device driver.
type Driver struct {
	ID              string `json:"id"`
	Vendor          string `json:"vendor"`
	Protocol        string `json:"protocol"`
	RequiresSSHAuth bool   `json:"requires_ssh_auth"`
}

// Catalogue is the generator's catalogue.
type Catalogue struct {
	Families []Family `json:"families"`
	Models   []Model  `json:"models"`
	Drivers  []Driver `json:"drivers"`
}

// Decode reads catalogue JSON and checks it is usable.
func Decode(b []byte) (*Catalogue, error) {
	var c Catalogue
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("catalogue: %w", err)
	}
	if len(c.Families) == 0 || len(c.Models) == 0 {
		return nil, errors.New("catalogue: no families or no models")
	}
	for _, m := range c.Models {
		if _, ok := c.Family(m.Family); !ok {
			return nil, fmt.Errorf("catalogue: model %s has unknown family %q", m.ID, m.Family)
		}
	}
	return &c, nil
}

// Family returns the family with this id.
func (c *Catalogue) Family(id string) (Family, bool) {
	for _, f := range c.Families {
		if f.ID == id {
			return f, true
		}
	}
	return Family{}, false
}

// Model returns the model with this id.
func (c *Catalogue) Model(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// ModelsIn lists a family's models, in catalogue order.
func (c *Catalogue) ModelsIn(family string) []Model {
	var out []Model
	for _, m := range c.Models {
		if m.Family == family {
			out = append(out, m)
		}
	}
	return out
}

// ModelIDs lists every model id, in catalogue order (the generator's).
func (c *Catalogue) ModelIDs() []string {
	out := make([]string, len(c.Models))
	for i, m := range c.Models {
		out[i] = m.ID
	}
	return out
}

// Driver returns the driver with this id.
func (c *Catalogue) Driver(id string) (Driver, bool) {
	for _, d := range c.Drivers {
		if d.ID == id {
			return d, true
		}
	}
	return Driver{}, false
}

// FamilyOfTemplate is the family of the models that use a driver (the
// manifest template column), for the Devices screen: "" when no model
// uses it (a scenario-only driver such as junos).
func (c *Catalogue) FamilyOfTemplate(driver string) string {
	for _, m := range c.Models {
		if m.Driver == driver {
			return m.Family
		}
	}
	return ""
}

// FamilyOfModel is a model's family, "" if unknown.
func (c *Catalogue) FamilyOfModel(id string) string {
	m, _ := c.Model(id)
	return m.Family
}

// FamilyOptions lists the options of a family's models, once each.
func (c *Catalogue) FamilyOptions(family string) []Option {
	var out []Option
	for _, m := range c.ModelsIn(family) {
		for _, o := range m.Options {
			if !slices.ContainsFunc(out, func(x Option) bool { return x.Name == o.Name }) {
				out = append(out, o)
			}
		}
	}
	return out
}

// OptionModels lists the models an option applies to.
func (c *Catalogue) OptionModels(name string) []string {
	var out []string
	for _, m := range c.Models {
		for _, o := range m.Options {
			if o.Name == name {
				out = append(out, m.ID)
			}
		}
	}
	return out
}

// ParseDistribution validates a --distribution string with the
// generator's rules (known models, whole weights >= 0, total 100) and
// returns every model's weight.
func (c *Catalogue) ParseDistribution(s string) (map[string]int, error) {
	weights := map[string]int{}
	total := 0
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, w, ok := strings.Cut(tok, ":")
		if !ok {
			return nil, fmt.Errorf("distribution token %q: want model:weight", tok)
		}
		name = strings.TrimSpace(name)
		if _, ok := c.Model(name); !ok {
			return nil, fmt.Errorf("unknown model %q (valid: %s)", name, strings.Join(c.ModelIDs(), ", "))
		}
		n, err := strconv.Atoi(strings.TrimSpace(w))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("distribution token %q: weight must be a whole number >= 0", tok)
		}
		weights[name] = n
		total += n
	}
	if total != 100 {
		return nil, fmt.Errorf("distribution weights must sum to 100, got %d", total)
	}
	for _, id := range c.ModelIDs() {
		if _, ok := weights[id]; !ok {
			weights[id] = 0
		}
	}
	return weights, nil
}

// Counts turns a distribution into exact per-model counts the way the
// generator does (truncate, then the remainder to the highest weight, ties
// to the earlier model), so a build from the Build screen and a testbench
// with the same counts are the same build.
func (c *Catalogue) Counts(total int, weights map[string]int) map[string]int {
	counts := map[string]int{}
	given := 0
	top, topW := "", -1
	for _, id := range c.ModelIDs() {
		n := total * weights[id] / 100
		if n > 0 {
			counts[id] = n
		}
		given += n
		if weights[id] > topW {
			top, topW = id, weights[id]
		}
	}
	if rem := total - given; rem > 0 {
		counts[top] += rem
	}
	return counts
}

// CountsArg is a --counts value in catalogue order, models with no devices
// left out.
func (c *Catalogue) CountsArg(counts map[string]int) string {
	var parts []string
	for _, id := range c.ModelIDs() {
		if counts[id] > 0 {
			parts = append(parts, id+":"+strconv.Itoa(counts[id]))
		}
	}
	return strings.Join(parts, ",")
}

// CheckCounts validates per-model counts against the catalogue.
func (c *Catalogue) CheckCounts(counts map[string]int) (int, error) {
	total := 0
	for id, n := range counts {
		if _, ok := c.Model(id); !ok {
			return 0, fmt.Errorf("unknown model %q", id)
		}
		if n < 0 {
			return 0, fmt.Errorf("model %s: count must be >= 0", id)
		}
		total += n
	}
	if total == 0 {
		return 0, errors.New("no devices: at least one model needs a count above 0")
	}
	return total, nil
}

// Runner runs the generator binary; tests replace it.
type Runner func(ctx context.Context, bin string, args ...string) ([]byte, error)

// ExecRunner runs bin for real.
func ExecRunner(ctx context.Context, bin string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Source runs `rcfg-sim-gen catalogue --json` once, and again whenever the
// binary changes (size or modification time), so a new generator's models
// appear without a UI restart.
type Source struct {
	bin string
	run Runner

	mu    sync.Mutex
	cat   *Catalogue
	mod   time.Time
	size  int64
	err   error
	tried time.Time
}

// NewSource returns a Source for the generator at bin; run nil means
// ExecRunner.
func NewSource(bin string, run Runner) *Source {
	if run == nil {
		run = ExecRunner
	}
	return &Source{bin: bin, run: run}
}

// retryAfter is how long a failed read is remembered before trying again.
const retryAfter = 10 * time.Second

// Get returns the catalogue, reading it again when the binary changed.
func (s *Source) Get() (*Catalogue, error) {
	if s.bin == "" { // Static
		return s.cat, nil
	}
	fi, statErr := os.Stat(s.bin)
	s.mu.Lock()
	defer s.mu.Unlock()
	if statErr == nil && s.cat != nil && fi.ModTime().Equal(s.mod) && fi.Size() == s.size {
		return s.cat, nil
	}
	if s.cat == nil && s.err != nil && time.Since(s.tried) < retryAfter {
		return nil, s.err
	}
	if statErr != nil {
		if s.cat != nil {
			return s.cat, nil // keep the last good one while the binary is replaced
		}
		s.err, s.tried = fmt.Errorf("generator %s: %w", s.bin, statErr), time.Now()
		return nil, s.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := s.run(ctx, s.bin, "catalogue", "--json")
	if err == nil {
		var c *Catalogue
		if c, err = Decode(out); err == nil {
			s.cat, s.mod, s.size, s.err = c, fi.ModTime(), fi.Size(), nil
			return c, nil
		}
	}
	s.err, s.tried = fmt.Errorf("read the device catalogue from %s: %w (is it an rcfg-sim-gen with the catalogue command?)", s.bin, err), time.Now()
	if s.cat != nil {
		return s.cat, nil
	}
	return nil, s.err
}

// Static is a Source that always returns c, for tests.
func Static(c *Catalogue) *Source {
	s := &Source{bin: "", run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("static") }}
	s.cat = c
	return s
}
