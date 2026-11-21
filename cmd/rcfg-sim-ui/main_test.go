package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
)

func TestCheckRemovedFlags(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--ui-token=x"}, "--ui-token was removed"},
		{[]string{"-ui-token", "x"}, "--ui-token was removed"},
		{[]string{"--poll", "2s", "--admin-token", "y"}, "--admin-token was removed"},
	} {
		if err := checkRemovedFlags(c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: want %q, got %v", c.args, c.want, err)
		}
	}
	for _, args := range [][]string{nil, {"--ui-addr", "127.0.0.1:8080"}, {"--", "--ui-token"}} {
		if err := checkRemovedFlags(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestHashPasswordFromPipe(t *testing.T) {
	in, err := os.CreateTemp(t.TempDir(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	in.WriteString("s3cret pass\n")
	in.Seek(0, 0)
	var out, prompt bytes.Buffer
	if err := hashPasswordCmd(in, &out, &prompt); err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(out.String())
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret pass")) != nil {
		t.Fatalf("hash %q does not verify", hash)
	}
	if strings.Contains(out.String()+prompt.String(), "s3cret") {
		t.Fatal("the password was echoed")
	}
}

// TestRefusesToStartWithoutCredentials runs the real binary.
func TestRefusesToStartWithoutCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := filepath.Join(t.TempDir(), "rcfg-sim-ui")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	good, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	envDir := t.TempDir()
	for desc, c := range map[string]struct {
		env  []string
		args []string
		want string
	}{
		"no credentials": {nil, nil, "UI_USERNAME is not set"},
		"no hash":        {[]string{"UI_USERNAME=admin"}, nil, "UI_PASSWORD_HASH is not set"},
		"bad hash":       {[]string{"UI_USERNAME=admin", "UI_PASSWORD_HASH=hunter2"}, nil, "not a valid bcrypt hash"},
		"removed flag":   {[]string{"UI_USERNAME=admin", "UI_PASSWORD_HASH=" + string(good)}, []string{"--ui-token=x"}, "--ui-token was removed"},
	} {
		cmd := exec.Command(bin, append([]string{"--env-dir", envDir, "--ui-addr", "127.0.0.1:0"}, c.args...)...)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, c.env...)
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: want exit with %q, got %v\n%s", desc, c.want, err, out)
		}
	}
}

func TestSettingsHashCmd(t *testing.T) {
	gen := filepath.Join(t.TempDir(), "rcfg-sim-gen")
	uitest.WriteFakeGen(t, gen)
	layout := []string{"--gen-bin", gen, "--ip-base", "10.0.0.1", "--ip-count", "2", "--port-start", "10000", "--devices-per-ip", "3", "--seed", "1"}
	want := jobs.HashSettings(map[string]int{"sm": 4, "md": 2}, 1, map[string]string{},
		jobs.Layout{IPBase: "10.0.0.1", IPCount: 2, PortStart: 10000, DevicesPerIP: 3})
	// A distribution becomes the generator's counts (6 x 60/40 = 4 + 2),
	// so it hashes like the same counts asked for directly.
	for _, extra := range [][]string{{"--count", "6", "--distribution", "md:40, sm:60"}, {"--counts", "sm:4,md:2"}} {
		got, err := settingsHashCmd(append(append([]string{}, layout...), extra...))
		if err != nil || got != want {
			t.Fatalf("%v: %s %v, want %s", extra, got, err, want)
		}
	}
	if _, err := settingsHashCmd([]string{"--count", "6"}); err == nil {
		t.Fatal("missing layout: want an error")
	}
	if _, err := settingsHashCmd(append(layout, "--counts", "sm:1", "--distribution", "sm:100")); err == nil {
		t.Fatal("both forms: want an error")
	}
}
