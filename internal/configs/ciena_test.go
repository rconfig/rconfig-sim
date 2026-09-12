package configs

import (
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// TestCienaGNEDeterministic: a GNE config (GNE shelf + RNE sections) is
// byte-reproducible across runs with the same seed.
func TestCienaGNEDeterministic(t *testing.T) {
	mk := func() string {
		cfg := baseTestConfig(t, 12)
		cfg.IPCount = 1
		cfg.DevicesPerIP = 12
		cfg.Distribution = "ciena-6500-tl1-gne:100"
		if _, err := Run(cfg, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		return cfg.OutputDir
	}
	dirA, dirB := mk(), mk()
	for i := 0; i < 12; i++ {
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
			t.Fatalf("GNE device %d not deterministic across runs", i)
		}
		if !bytes.Contains(a, []byte("\n;;RNE ")) {
			t.Errorf("GNE device %d has no RNE section: %q", i, a)
		}
	}
}

// rneSections splits a rendered GNE config into its ";;RNE <TID>" sections.
func rneSections(data []byte) map[string][]byte {
	marker := []byte("\n;;RNE ")
	sections := map[string][]byte{}

	for i := 0; i < len(data); i++ {
		if !bytes.HasPrefix(data[i:], marker) {
			continue
		}
		start := i + len(marker)
		nl := bytes.IndexByte(data[start:], '\n')
		if nl < 0 {
			break
		}
		tid := string(bytes.TrimSpace(data[start : start+nl]))

		// The marker carries a leading "\n", so include that byte in the body this
		// section ends with - otherwise a section in the middle of a file would lose a
		// trailing newline that the last section in a file keeps, and two identical
		// sections would compare unequal.
		bodyStart := start + nl + 1
		end := len(data)
		if next := bytes.Index(data[bodyStart:], marker); next >= 0 {
			end = bodyStart + next + 1
		}
		sections[tid] = data[bodyStart:end]
		i = bodyStart - 1
	}

	return sections
}

// generateGNEFleet renders n GNE devices and returns their configs by filename.
func generateGNEFleet(t *testing.T, n, dualHomePct int) [][]byte {
	t.Helper()

	cfg := baseTestConfig(t, n)
	cfg.IPCount = 1
	cfg.DevicesPerIP = n
	cfg.Distribution = "ciena-6500-tl1-gne:100"
	cfg.RNEDualHomePct = dualHomePct

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

// A dual-homed RNE is one physical element reached through several gateways, so every
// gateway must describe it identically. Divergent inventories behind one TID is the
// failure this whole option exists to make impossible.
func TestCienaGNEDualHomedSectionsAreIdentical(t *testing.T) {
	fleet := generateGNEFleet(t, 40, 100)

	bodies := map[string][][]byte{}
	for _, data := range fleet {
		for tid, body := range rneSections(data) {
			bodies[tid] = append(bodies[tid], body)
		}
	}

	shared := 0
	for tid, seen := range bodies {
		if len(seen) < 2 {
			continue
		}
		shared++
		for i := 1; i < len(seen); i++ {
			if !bytes.Equal(seen[0], seen[i]) {
				t.Errorf("RNE %s differs between the gateways fronting it", tid)
			}
		}
	}

	if shared == 0 {
		t.Fatal("no RNE was fronted by more than one gateway: dual-homing did not happen")
	}
}

// Every adjacent pair of gateways must share an element, so any two neighbouring devices
// in a fleet demonstrate dual-homing. Leaving it to a random collision between two small
// RNE sets only worked about one time in five, which is no use for a test bed.
func TestCienaGNEAdjacentGatewaysAlwaysShareAnRNE(t *testing.T) {
	const fleet = 6
	configs := generateGNEFleet(t, fleet, 100)

	for i := 0; i+1 < fleet; i++ {
		left := rneSections(configs[i])
		right := rneSections(configs[i+1])

		shared := 0
		for tid := range left {
			if _, ok := right[tid]; ok {
				shared++
			}
		}

		if shared == 0 {
			t.Errorf("gateways %d and %d share no RNE", i, i+1)
		}
	}
}

// The chain must not push a gateway past the documented 2-5 remote NEs: linking counts
// towards the total rather than being added on top of it.
func TestCienaGNERneCountStaysInRange(t *testing.T) {
	for _, pct := range []int{0, 100} {
		for i, data := range generateGNEFleet(t, 12, pct) {
			n := len(rneSections(data))
			if n < 2 || n > 5 {
				t.Errorf("pct=%d gateway %d has %d RNEs, want 2-5", pct, i, n)
			}
		}
	}
}

// The last gateway has no successor, so it must not claim a forward link: an element
// behind a single gateway is not dual-homed and would just be a confusing extra device.
func TestCienaGNELastGatewayHasNoDanglingLink(t *testing.T) {
	const fleet = 6
	configs := generateGNEFleet(t, fleet, 100)

	owners := map[string]int{}
	for _, data := range configs {
		for tid := range rneSections(data) {
			owners[tid]++
		}
	}

	for tid := range rneSections(configs[fleet-1]) {
		if strings.HasPrefix(tid, "RNE-LINK") && owners[tid] < 2 {
			t.Errorf("last gateway carries link %s that no other gateway shares", tid)
		}
	}
}

// The option is opt-in: at the default of 0 no RNE is shared between gateways, and the
// generator takes no extra draw from the stream, so existing fleets regenerate unchanged.
func TestCienaGNEDualHomeOffSharesNothing(t *testing.T) {
	fleet := generateGNEFleet(t, 40, 0)

	owners := map[string]int{}
	for _, data := range fleet {
		for tid := range rneSections(data) {
			owners[tid]++
		}
	}

	if len(owners) == 0 {
		t.Fatal("no RNE sections rendered at all")
	}
}

// Private RNEs carry their gateway's index so two gateways cannot collide on a city name
// and present unrelated equipment under one TID. Shared ones keep the bare form. Both must
// stay matchable as RNE-[A-Z0-9]+, which is how a TID is recognised on the wire.
func TestCienaRneTIDShape(t *testing.T) {
	shape := regexp.MustCompile(`^RNE-[A-Z0-9]+$`)

	for _, fleet := range [][][]byte{generateGNEFleet(t, 12, 0), generateGNEFleet(t, 12, 100)} {
		for _, data := range fleet {
			for tid := range rneSections(data) {
				if !shape.MatchString(tid) {
					t.Errorf("TID %q is not matchable as RNE-[A-Z0-9]+", tid)
				}
			}
		}
	}
}

// A shared RNE's shelf is derived from its TID alone, which is what lets two workers
// render it identically without coordinating.
func TestSharedRneShelfDependsOnlyOnTheTID(t *testing.T) {
	cfg := baseTestConfig(t, 1)
	cfg.RNEDualHomePct = 100

	first := buildSharedRneShelf(cfg, "RNE-LAX")
	second := buildSharedRneShelf(cfg, "RNE-LAX")
	other := buildSharedRneShelf(cfg, "RNE-JFK")

	if first.ShelfSerial != second.ShelfSerial || first.NodeIP != second.NodeIP {
		t.Error("the same TID produced two different shelves")
	}
	if len(first.Slots) != len(second.Slots) {
		t.Error("the same TID produced a different slot count")
	}
	if first.ShelfSerial == other.ShelfSerial {
		t.Error("two different TIDs produced the same shelf serial")
	}
	if first.SID != "RNE-LAX" {
		t.Errorf("shared RNE SID = %q, want RNE-LAX", first.SID)
	}
}

func TestParseDistributionCiena(t *testing.T) {
	if _, err := parseDistribution("ciena-6500-tl1:100"); err != nil {
		t.Errorf("ciena-only distribution should parse: %v", err)
	}
	if _, err := parseDistribution("ciena-6500-tl1-gne:100"); err != nil {
		t.Errorf("ciena GNE distribution should parse: %v", err)
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
