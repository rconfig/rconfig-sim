package configs

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func baseTestConfig(t *testing.T, count int) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Count:          count,
		OutputDir:      filepath.Join(dir, "configs"),
		ManifestPath:   filepath.Join(dir, "manifest.csv"),
		IPBase:         "10.50.0.1",
		IPCount:        20,
		PortStart:      10000,
		DevicesPerIP:   2500,
		Seed:           42,
		Distribution:   "small:40,medium:40,large:15,huge:5",
		Username:       "admin",
		Password:       "admin",
		EnablePassword: "enable123",
	}
}

func TestRunSmallRun(t *testing.T) {
	cfg := baseTestConfig(t, 100)
	sum, err := Run(cfg, io.Discard)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sum.Count != 100 {
		t.Fatalf("want count=100, got %d", sum.Count)
	}
	// Expected stratified counts: 40/40/15/5
	expected := map[string]int{"small": 40, "medium": 40, "large": 15, "huge": 5}
	for b, want := range expected {
		got := sum.PerBucket[b].Realised
		if got != want {
			t.Errorf("bucket %s: want %d realised, got %d", b, want, got)
		}
	}

	// Confirm we rendered the right number of files.
	entries, err := os.ReadDir(cfg.OutputDir)
	if err != nil {
		t.Fatalf("read output dir: %v", err)
	}
	if len(entries) != 100 {
		t.Fatalf("want 100 cfg files, got %d", len(entries))
	}

	// Confirm manifest parses and has 101 rows (header + 100 devices).
	f, err := os.Open(cfg.ManifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if len(rows) != 101 {
		t.Fatalf("want 101 rows (header+100), got %d", len(rows))
	}
	wantHeader := []string{"hostname", "ip", "port", "vendor", "template",
		"username", "password", "enable_password", "config_file", "size_bucket"}
	for i, h := range wantHeader {
		if rows[0][i] != h {
			t.Errorf("header[%d]: want %q, got %q", i, h, rows[0][i])
		}
	}
}

func TestRunDeterministic(t *testing.T) {
	cfg1 := baseTestConfig(t, 20)
	if _, err := Run(cfg1, io.Discard); err != nil {
		t.Fatalf("run1: %v", err)
	}
	cfg2 := baseTestConfig(t, 20)
	cfg2.Seed = cfg1.Seed
	if _, err := Run(cfg2, io.Discard); err != nil {
		t.Fatalf("run2: %v", err)
	}
	// Compare every config file byte-for-byte.
	for i := 0; i < 20; i++ {
		name := filepath.Join("configs", filepath.Base(mustGlobIndex(t, cfg1.OutputDir, i)))
		b1, err := os.ReadFile(filepath.Join(filepath.Dir(cfg1.OutputDir), name))
		if err != nil {
			t.Fatalf("read run1 %d: %v", i, err)
		}
		b2, err := os.ReadFile(filepath.Join(filepath.Dir(cfg2.OutputDir), name))
		if err != nil {
			t.Fatalf("read run2 %d: %v", i, err)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("device %d: runs differ (same seed should produce identical bytes)", i)
		}
	}
}

func mustGlobIndex(t *testing.T, dir string, idx int) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if idx >= len(entries) {
		t.Fatalf("idx %d out of range (%d entries)", idx, len(entries))
	}
	return entries[idx].Name()
}

func TestParseDistribution(t *testing.T) {
	good := "small:40,medium:40,large:15,huge:5"
	w, err := parseDistribution(good)
	if err != nil {
		t.Fatalf("parse %q: %v", good, err)
	}
	for _, b := range []string{"small", "medium", "large", "huge"} {
		if _, ok := w[b]; !ok {
			t.Errorf("bucket %s missing", b)
		}
	}
	if _, err := parseDistribution("small:50,medium:40"); err == nil {
		t.Error("expected error on non-100-sum distribution")
	}
	if _, err := parseDistribution("tiny:100"); err == nil {
		t.Error("expected error on unknown bucket")
	}
}

func TestStratifiedCountsExact(t *testing.T) {
	weights := map[string]int{"small": 40, "medium": 40, "large": 15, "huge": 5}
	c := stratifiedCounts(100, weights)
	want := map[string]int{"small": 40, "medium": 40, "large": 15, "huge": 5}
	for b, w := range want {
		if c[b] != w {
			t.Errorf("bucket %s: want %d, got %d", b, w, c[b])
		}
	}
	// Sum should always equal the total, even with weird rounding.
	c2 := stratifiedCounts(17, weights)
	total := 0
	for _, v := range c2 {
		total += v
	}
	if total != 17 {
		t.Errorf("want sum=17, got %d", total)
	}
}
