package catalogue

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
)

func real(t *testing.T) *Catalogue {
	t.Helper()
	c, err := Decode(uitest.CatalogueJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDecodeAndLookups(t *testing.T) {
	c := real(t)
	if f, ok := c.Family("optical"); !ok || f.Name != "Optical transport (TL1)" {
		t.Fatalf("optical family %+v", f)
	}
	if got := len(c.ModelsIn("ip")); got != 9 {
		t.Fatalf("%d ip models", got)
	}
	var optical []string
	for _, m := range c.ModelsIn("optical") {
		optical = append(optical, m.ID)
	}
	if !slices.Equal(optical, []string{"ciena-6500-tl1", "ciena-6500-tl1-gne", "infinera-dtnx-tl1", "cisco-ons15454-tl1"}) {
		t.Fatalf("optical models %v", optical)
	}
	if c.FamilyOfTemplate("ciena_tl1") != "optical" || c.FamilyOfTemplate("cisco_ios") != "ip" || c.FamilyOfTemplate("junos") != "" {
		t.Fatal("family of template")
	}
	opts := c.FamilyOptions("optical")
	if len(opts) != 1 || opts[0].FlagName() != "rne-dual-home-pct" || len(c.FamilyOptions("ip")) != 0 {
		t.Fatalf("options %+v", opts)
	}
	if d, ok := c.Driver("ciena_tl1"); !ok || d.RequiresSSHAuth {
		t.Fatal("ciena driver")
	}
	if _, err := Decode([]byte(`{"families":[{"id":"ip"}],"models":[{"id":"x","family":"nope"}]}`)); err == nil {
		t.Fatal("unknown family accepted")
	}
}

// TestCountsMatchGenerator: the UI's distribution-to-counts rounding is the
// generator's (its per-model targets for the same distribution), so a
// build is the same whichever asked for it.
func TestCountsMatchGenerator(t *testing.T) {
	c := real(t)
	for _, tc := range []struct {
		total int
		dist  string
	}{{20, "sm:40,md:40,lg:15,xl:5"}, {7, "sm:33,md:33,lg:34"}, {9, "sm:70,md:30"}, {11, "sm:50,ciena-6500-tl1:50"}, {13, "md:25,sm:25,ciena-6500-tl1-gne:50"}} {
		w, err := c.ParseDistribution(tc.dist)
		if err != nil {
			t.Fatal(err)
		}
		got := c.Counts(tc.total, w)
		dir := t.TempDir()
		sum, err := configs.Run(configs.Config{Count: tc.total, OutputDir: filepath.Join(dir, "c"), ManifestPath: filepath.Join(dir, "m.csv"),
			IPBase: "10.0.0.1", IPCount: 1, PortStart: 10000, DevicesPerIP: 100, Seed: 1, Distribution: tc.dist}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range c.ModelIDs() {
			if got[m] != sum.PerBucket[m].Target {
				t.Errorf("%v: %s: UI %d, generator %d", tc, m, got[m], sum.PerBucket[m].Target)
			}
		}
	}
	if _, err := c.ParseDistribution("sm:50"); err == nil {
		t.Fatal("total 50 accepted")
	}
	if _, err := c.CheckCounts(map[string]int{"sm": 0}); err == nil {
		t.Fatal("no devices accepted")
	}
	if _, err := c.CheckCounts(map[string]int{"nope": 1}); err == nil {
		t.Fatal("unknown model accepted")
	}
	if got := c.CountsArg(map[string]int{"md": 2, "sm": 1, "lg": 0}); got != "sm:1,md:2" {
		t.Fatalf("counts arg %q", got)
	}
}

// TestSourceFollowsTheBinary: a model added to the generator appears with
// no UI change, once the binary changes (the registry-extension test).
func TestSourceFollowsTheBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "rcfg-sim-gen")
	uitest.WriteFakeGen(t, bin)
	src := NewSource(bin, nil)
	c, err := src.Get()
	if err != nil || len(c.Models) != 13 {
		t.Fatalf("first read: %d models, %v", len(c.Models), err)
	}

	var raw map[string]any
	_ = json.Unmarshal(uitest.CatalogueJSON(t), &raw)
	raw["models"] = append(raw["models"].([]any), map[string]any{
		"id": "arista-7050", "family": "ip", "vendor": "Arista", "driver": "eos", "protocol": "cli",
		"description": "Test model.", "avg_bytes": 1000, "options": []any{}})
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(bin+".catalogue.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Same binary: still the cached catalogue.
	if c, _ := src.Get(); len(c.Models) != 13 {
		t.Fatal("re-read without a binary change")
	}
	// The binary changes: the new model appears.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	c, err = src.Get()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Model("arista-7050"); !ok || len(c.ModelsIn("ip")) != 10 {
		t.Fatalf("new model missing: %d ip models", len(c.ModelsIn("ip")))
	}

	bad := NewSource(filepath.Join(dir, "missing"), nil)
	if _, err := bad.Get(); err == nil {
		t.Fatal("missing binary: want an error")
	}
	_ = fmt.Sprint
}
