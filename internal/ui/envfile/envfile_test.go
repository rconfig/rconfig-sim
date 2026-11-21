package envfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `# Sample env file
LISTEN_IP=10.50.0.1

; semicolon comment
  PORT_START=10000
PASSWORD="pa ss\"word"
FAULT_RATE=0.1
FAULT_TYPES=auth_fail,malformed
DEVICE_PROFILES=hang:3600s@3%
UNKNOWN_KEY=keep me
not an assignment
LOG_LEVEL=info`

func write(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "10.50.0.1.env")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask-proof
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	f := Parse("x", []byte(sample))
	for k, want := range map[string]string{
		"LISTEN_IP": "10.50.0.1", "PORT_START": "10000", "PASSWORD": `pa ss"word`,
		"FAULT_TYPES": "auth_fail,malformed", "DEVICE_PROFILES": "hang:3600s@3%", "UNKNOWN_KEY": "keep me",
	} {
		if got, ok := f.Get(k); !ok || got != want {
			t.Errorf("%s: want %q, got %q (%v)", k, want, got, ok)
		}
	}
	if _, ok := f.Get("not an assignment"); ok {
		t.Error("garbage line parsed as a key")
	}
	if string(f.Bytes()) != sample+"\n" {
		t.Errorf("round trip changed the file:\n%s", f.Bytes())
	}
}

func TestUpdatePreservesEverythingElse(t *testing.T) {
	p := write(t, sample+"\n", 0o660)
	err := Update(p, map[string]string{
		FaultRates:     "auth_fail=0.02,disconnect_mid=0.005",
		DeviceProfiles: "latency:200-800ms@10%",
		ActiveDevices:  "1200",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := strings.NewReplacer(
		"FAULT_RATE=0.1", "FAULT_RATE=0",
		"FAULT_TYPES=auth_fail,malformed", "FAULT_TYPES=",
		"DEVICE_PROFILES=hang:3600s@3%", "DEVICE_PROFILES=latency:200-800ms@10%",
	).Replace(sample) + "\nACTIVE_DEVICES=1200\nFAULT_RATES=auth_fail=0.02,disconnect_mid=0.005\n"
	if string(got) != want {
		t.Fatalf("updated file:\n%s\nwant:\n%s", got, want)
	}
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o660 {
		t.Errorf("mode not kept: %v", info.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".*tmp*")); len(m) != 0 {
		t.Errorf("temp files left behind: %v", m)
	}
}

func TestUpdateFaultRatesWithoutLegacyKeys(t *testing.T) {
	p := write(t, "LISTEN_IP=10.50.0.2\n", 0o640)
	if err := Update(p, map[string]string{FaultRates: "malformed=1"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "LISTEN_IP=10.50.0.2\nFAULT_RATES=malformed=1\n" {
		t.Fatalf("legacy keys must only be cleared when present:\n%s", got)
	}
}

func TestUpdateIndentedAndDuplicateKeys(t *testing.T) {
	p := write(t, "  PROFILE_SEED=1\nPROFILE_SEED=2\n", 0o600)
	if err := Update(p, map[string]string{ProfileSeed: "7"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "  PROFILE_SEED=7\nPROFILE_SEED=7\n" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestUpdateRefusesUnownedKeys(t *testing.T) {
	p := write(t, sample, 0o660)
	before, _ := os.ReadFile(p)
	for _, k := range []string{"USERNAME", "MANIFEST", "ADMIN_ADDR", "LISTEN_IP", FaultRate} {
		if err := Update(p, map[string]string{k: "x"}); !errors.Is(err, ErrNotOwned) {
			t.Errorf("%s: want ErrNotOwned, got %v", k, err)
		}
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("a refused update changed the file")
	}
}

// TestRestore: a saved value is set back, a key saved as unset is removed
// (every assignment of it), other lines stay, and unowned keys are refused.
func TestRestore(t *testing.T) {
	p := write(t, "# keep\nPASSWORD=\nCOMMANDS_ROOT=/x\nLISTEN_IP=1.2.3.4\nCOMMANDS_ROOT=/y\n", 0o660)
	pw := "hand set"
	if err := Restore(p, map[string]*string{Password: &pw, CommandsRoot: nil, EnablePassword: nil}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "# keep\nPASSWORD=\"hand set\"\nLISTEN_IP=1.2.3.4\n" {
		t.Fatalf("got:\n%s", got)
	}
	if err := Restore(p, map[string]*string{"LISTEN_IP": nil}); !errors.Is(err, ErrNotOwned) {
		t.Fatalf("unowned key: %v", err)
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, v := range []string{"", "plain", "has space", `q"uote`, `back\slash`, "hash#", "a=b,c=d", "hang:3600s@3%"} {
		f := Parse("x", nil)
		f.Set("K", v)
		back := Parse("x", f.Bytes())
		if got, _ := back.Get("K"); got != v {
			t.Errorf("%q: round trip gave %q (line %q)", v, got, f.Bytes())
		}
	}
}

func TestWriteAtomicMissingFile(t *testing.T) {
	if err := WriteAtomic(filepath.Join(t.TempDir(), "nope.env"), []byte("x")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}
