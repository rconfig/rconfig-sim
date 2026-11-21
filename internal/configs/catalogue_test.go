package configs

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/drivers"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestCatalogueGolden pins the catalogue JSON (rcfg-sim-gen catalogue
// --json). Run with -update after an intended change.
func TestCatalogueGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteCatalogueJSON(&buf); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "catalogue.golden.json")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/configs -run CatalogueGolden -update)", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("catalogue changed; if intended, run with -update:\n%s", buf.String())
	}
}

func TestCatalogueCoversRegistry(t *testing.T) {
	c := GetCatalogue()
	fams := map[string]bool{}
	for _, f := range c.Families {
		fams[f.ID] = true
	}
	if len(c.Models) != len(registry) {
		t.Fatalf("%d models in the catalogue, %d in the registry", len(c.Models), len(registry))
	}
	for _, m := range c.Models {
		if !fams[m.Family] || m.Description == "" || m.AvgBytes <= 0 || m.Vendor == "" {
			t.Errorf("model %s: incomplete catalogue entry %+v", m.ID, m)
		}
		d, ok := drivers.Lookup(m.Driver)
		if !ok || d.Protocol != m.Protocol {
			t.Errorf("model %s: driver %q not in the driver metadata", m.ID, m.Driver)
		}
		if m.Family == FamilyIP && m.Protocol != drivers.CLI || m.Family == FamilyOptical && m.Protocol != drivers.TL1 {
			t.Errorf("model %s: family %s with protocol %s", m.ID, m.Family, m.Protocol)
		}
	}
	var gne CatalogueModel
	for _, m := range c.Models {
		if m.ID == "ciena-6500-tl1-gne" {
			gne = m
		}
	}
	if len(gne.Options) != 1 || gne.Options[0].Name != "rne_dual_home_pct" || gne.Options[0].Flag != "--rne-dual-home-pct" {
		t.Fatalf("GNE options %+v", gne.Options)
	}
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"requires_ssh_auth":false`) {
		t.Fatal("drivers missing from the JSON")
	}
}

func TestParseCounts(t *testing.T) {
	c, total, err := ParseCounts(" sm:6000, md:4000 ,ciena-6500-tl1-gne:2000")
	if err != nil || total != 12000 || c["sm"] != 6000 || c["ciena-6500-tl1-gne"] != 2000 || c["xl"] != 0 {
		t.Fatalf("%v %d %v", c, total, err)
	}
	for _, bad := range []string{"", "sm", "sm:x", "sm:-1", "nope:1", "sm:1,sm:2", "sm:0"} {
		if _, _, err := ParseCounts(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func dirHash(t *testing.T, cfg Config) [32]byte {
	t.Helper()
	h := sha256.New()
	entries, err := os.ReadDir(cfg.OutputDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(cfg.OutputDir, e.Name()))
		h.Write([]byte(e.Name()))
		h.Write(b)
	}
	m, _ := os.ReadFile(cfg.ManifestPath)
	// Manifest paths differ by temp dir; compare without them.
	h.Write([]byte(strings.ReplaceAll(string(m), filepath.Dir(cfg.OutputDir), "")))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// TestCountsExact: --counts makes exactly the requested devices per model,
// and the same counts as a distribution would give produce identical
// output, so a build is the same whichever form asked for it.
func TestCountsExact(t *testing.T) {
	cfg := baseTestConfig(t, 0)
	cfg.Counts, cfg.Count, cfg.Distribution = "sm:7,md:3,ciena-6500-tl1-gne:2", 12, ""
	sum, err := Run(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.ExactCounts || sum.PerBucket["sm"].Realised != 7 || sum.PerBucket["md"].Realised != 3 || sum.PerBucket["ciena-6500-tl1-gne"].Realised != 2 {
		t.Fatalf("realised %+v", sum.PerBucket)
	}
	if !strings.Contains(sum.String(), "per-model counts:") {
		t.Fatalf("summary:\n%s", sum)
	}

	byDist := baseTestConfig(t, 20) // sm:40,md:40,lg:15,xl:5 of 20 = 8,8,3,1
	if _, err := Run(byDist, io.Discard); err != nil {
		t.Fatal(err)
	}
	byCounts := baseTestConfig(t, 20)
	byCounts.Distribution, byCounts.Counts = "", "sm:8,md:8,lg:3,xl:1"
	if _, err := Run(byCounts, io.Discard); err != nil {
		t.Fatal(err)
	}
	if dirHash(t, byDist) != dirHash(t, byCounts) {
		t.Fatal("equal counts from --distribution and --counts produced different output")
	}

	bad := baseTestConfig(t, 5)
	bad.Counts = "sm:7"
	if _, err := Run(bad, io.Discard); err == nil || !strings.Contains(err.Error(), "adds up to 7") {
		t.Fatalf("count mismatch: %v", err)
	}
}

// TestCatalogueAvgBytes generates a sample of each affordable model and
// checks its catalogue average size within 35% (the UI's build estimate
// uses these).
func TestCatalogueAvgBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("generates configs")
	}
	for _, m := range []string{"sm", "md", "lg", "xl", "ciena-6500-tl1", "ciena-6500-tl1-gne", "infinera-dtnx-tl1", "cisco-ons15454-tl1"} {
		n := 40
		if m == "lg" || m == "xl" {
			n = 10
		}
		dir := t.TempDir()
		if _, err := Run(Config{Count: n, OutputDir: filepath.Join(dir, "c"), ManifestPath: filepath.Join(dir, "m.csv"),
			IPBase: "10.0.0.1", IPCount: 1, PortStart: 10000, DevicesPerIP: n, Seed: 42, Distribution: m + ":100",
			Username: "a", Password: "a", EnablePassword: "a"}, io.Discard); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		var total int64
		entries, _ := os.ReadDir(filepath.Join(dir, "c"))
		for _, e := range entries {
			fi, _ := e.Info()
			total += fi.Size()
		}
		avg, want := float64(total)/float64(n), float64(registry[m].avgBytes)
		if avg < want*0.65 || avg > want*1.35 {
			t.Errorf("%s: generated average %.0f bytes, catalogue says %.0f", m, avg, want)
		}
	}
}
