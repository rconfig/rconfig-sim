package configs

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// generateOptical renders n devices of one model and returns their configs in index order.
func generateOptical(t *testing.T, n int, distribution string) [][]byte {
	t.Helper()

	cfg := baseTestConfig(t, n)
	cfg.IPCount = 1
	cfg.DevicesPerIP = n
	cfg.Distribution = distribution

	if _, err := Run(cfg, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	out := make([][]byte, n)
	for i := 0; i < n; i++ {
		data, err := os.ReadFile(filepath.Join(cfg.OutputDir, "device-"+pad(i)+".cfg"))
		if err != nil {
			t.Fatalf("read device %d: %v", i, err)
		}
		out[i] = data
	}

	return out
}

// Mirrors TestCienaDeterministic: the same seed must produce byte-identical output, or a
// fleet cannot be regenerated reproducibly.
func TestInfineraDeterministic(t *testing.T) {
	a := generateOptical(t, 10, infineraModelName+":100")
	b := generateOptical(t, 10, infineraModelName+":100")

	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("infinera device %d not deterministic across runs", i)
		}
		if !bytes.Contains(a[i], []byte("\n;;NODE ")) {
			t.Errorf("infinera device %d has no remote-node section", i)
		}
	}
}

func TestCiscoONSDeterministic(t *testing.T) {
	a := generateOptical(t, 10, ciscoONSModelName+":100")
	b := generateOptical(t, 10, ciscoONSModelName+":100")

	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("cisco ONS device %d not deterministic across runs", i)
		}
		if !bytes.Contains(a[i], []byte("\n;;ENE ")) {
			t.Errorf("cisco ONS device %d has no end-NE section", i)
		}
	}
}

// RTRV-TIDMAP pages every 10 records, so an Infinera node needs more neighbours than that or
// the paging path the driver exists to reproduce is never exercised.
func TestInfineraNodeHasEnoughRemotesToPage(t *testing.T) {
	for i, data := range generateOptical(t, 12, infineraModelName+":100") {
		if n := bytes.Count(data, []byte("\n;;NODE ")); n <= 10 {
			t.Errorf("device %d has %d remote nodes, want more than one page (>10)", i, n)
		}
	}
}

// The two new models must parse in a distribution string, and appear in the manifest with the
// vendor and driver id the runtime resolves against.
func TestOpticalModelsInManifest(t *testing.T) {
	for _, tc := range []struct{ model, vendor, driver string }{
		{infineraModelName, "Infinera", "infinera_tl1"},
		{ciscoONSModelName, "Cisco", "cisco_ons_tl1"},
	} {
		cfg := baseTestConfig(t, 2)
		cfg.IPCount = 1
		cfg.DevicesPerIP = 2
		cfg.Distribution = tc.model + ":100"

		if _, err := Run(cfg, io.Discard); err != nil {
			t.Fatalf("%s: run: %v", tc.model, err)
		}

		raw, err := os.ReadFile(cfg.ManifestPath)
		if err != nil {
			t.Fatalf("%s: read manifest: %v", tc.model, err)
		}

		if !bytes.Contains(raw, []byte(","+tc.vendor+","+tc.driver+",")) {
			t.Errorf("%s: manifest missing %s/%s pair:\n%s", tc.model, tc.vendor, tc.driver, raw)
		}
		// An empty hostname means modelHostname has no case for this data type.
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n"))[1:] {
			if bytes.HasPrefix(line, []byte(",")) {
				t.Errorf("%s: manifest row has an empty hostname: %s", tc.model, line)
			}
		}
	}
}
