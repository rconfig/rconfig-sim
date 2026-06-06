package configs

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestCienaDeterministic mirrors TestRunDeterministic for the Ciena model: the
// same seed must produce byte-identical TL1 inventory payloads.
func TestCienaDeterministic(t *testing.T) {
	mk := func() string {
		cfg := baseTestConfig(t, 20)
		cfg.IPCount = 1
		cfg.DevicesPerIP = 20
		cfg.Distribution = "ciena-6500-tl1:100"
		if _, err := Run(cfg, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		return cfg.OutputDir
	}
	dirA := mk()
	dirB := mk()
	for i := 0; i < 20; i++ {
		fa := filepath.Join(dirA, "device-"+pad(i)+".cfg")
		fb := filepath.Join(dirB, "device-"+pad(i)+".cfg")
		a, err := os.ReadFile(fa)
		if err != nil {
			t.Fatalf("read %s: %v", fa, err)
		}
		b, err := os.ReadFile(fb)
		if err != nil {
			t.Fatalf("read %s: %v", fb, err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("device %d not deterministic across runs", i)
		}
		if !bytes.Contains(a, []byte("TYPE=6500-7SLOT")) {
			t.Errorf("device %d payload missing 6500 shelf line: %q", i, a)
		}
	}
}

func pad(i int) string {
	s := []byte("00000")
	for p := len(s) - 1; i > 0 && p >= 0; p-- {
		s[p] = byte('0' + i%10)
		i /= 10
	}
	return string(s)
}

func TestParseDistributionCiena(t *testing.T) {
	if _, err := parseDistribution("ciena-6500-tl1:100"); err != nil {
		t.Errorf("ciena-only distribution should parse: %v", err)
	}
	if _, err := parseDistribution("sm:50,ciena-6500-tl1:50"); err != nil {
		t.Errorf("mixed Cisco/Ciena distribution should parse: %v", err)
	}
	if _, err := parseDistribution("tiny:100"); err == nil {
		t.Error("unknown model name should error")
	}
}

// TestManifestVendorColumns asserts the vendor/template columns now reflect the
// per-device model: Ciena rows carry Ciena/ciena_tl1, Cisco rows still carry
// Cisco/cisco_ios, and the header is unchanged (10 columns, same order).
func TestManifestVendorColumns(t *testing.T) {
	cfg := baseTestConfig(t, 40)
	cfg.IPCount = 1
	cfg.DevicesPerIP = 40
	cfg.Distribution = "sm:50,ciena-6500-tl1:50"
	if _, err := Run(cfg, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	f, err := os.Open(cfg.ManifestPath)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	wantHeader := []string{"hostname", "ip", "port", "vendor", "template", "username", "password", "enable_password", "config_file", "size_bucket"}
	if len(rows) == 0 || len(rows[0]) != len(wantHeader) {
		t.Fatalf("header shape changed: %v", rows[0])
	}
	for i, h := range wantHeader {
		if rows[0][i] != h {
			t.Fatalf("header[%d] = %q, want %q", i, rows[0][i], h)
		}
	}

	var sawCisco, sawCiena bool
	for _, row := range rows[1:] {
		vendor, template, bucket := row[3], row[4], row[9]
		switch bucket {
		case "ciena-6500-tl1":
			sawCiena = true
			if vendor != "Ciena" || template != "ciena_tl1" {
				t.Errorf("ciena row: vendor=%q template=%q, want Ciena/ciena_tl1", vendor, template)
			}
		case "sm":
			sawCisco = true
			if vendor != "Cisco" || template != "cisco_ios" {
				t.Errorf("cisco row: vendor=%q template=%q, want Cisco/cisco_ios", vendor, template)
			}
		}
	}
	if !sawCisco || !sawCiena {
		t.Fatalf("expected both Cisco and Ciena rows; sawCisco=%v sawCiena=%v", sawCisco, sawCiena)
	}
}
