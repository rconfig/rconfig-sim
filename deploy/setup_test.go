// Package deploy_test runs deploy/setup.sh (and through it make install and
// make install-ui) against a temporary root. Every host-changing command is
// a fake that records its calls and keeps its state in the temp root, so a
// test never runs a real one, even as root.
package deploy_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
)

// fakes are the host-changing commands. Each appends "name args" to
// $FAKE_ROOT/calls.log; the state they need lives under $FAKE_ROOT/state.
var fakes = map[string]string{
	"id": `if [ "$1" = -u ] && [ -z "${2:-}" ]; then echo 0; exit 0; fi
grep -qxF "$2" "$FAKE_ROOT/state/users" 2>/dev/null && { echo 990; exit 0; }
exit 1`,
	"getent":   `[ "$1" = group ] && grep -qxF "$2" "$FAKE_ROOT/state/groups" 2>/dev/null`,
	"groupadd": `echo "${@: -1}" >> "$FAKE_ROOT/state/groups"`,
	"useradd":  `echo "${@: -1}" >> "$FAKE_ROOT/state/users"`,
	"systemctl": `s=$FAKE_ROOT/state
case $1 in
  is-enabled) grep -qxF "$2" "$s/enabled" 2>/dev/null && { echo enabled; exit 0; }; echo disabled; exit 1 ;;
  is-active) grep -qxF "$2" "$s/active" 2>/dev/null && { echo active; exit 0; }; echo inactive; exit 3 ;;
  enable) [ "$2" = --now ] && { echo "$3" >> "$s/enabled"; echo "$3" >> "$s/active"; } ;;
esac
exit 0`,
	"chown":   `exit 0`,
	"sysctl":  `exit 0`,
	"iproute": `exit 0`,
}

type rig struct {
	t                 *testing.T
	root, repo, fakes string
	env               []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binaries")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, root: t.TempDir(), repo: repo}
	r.fakes = filepath.Join(r.root, "fakes")
	for _, d := range []string{r.fakes, filepath.Join(r.root, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range fakes {
		script := fmt.Sprintf("#!/usr/bin/env bash\necho %q \"$*\" >> \"$FAKE_ROOT/calls.log\"\n%s\n", name, body)
		if err := os.WriteFile(filepath.Join(r.fakes, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	R := r.root
	r.env = append(os.Environ(),
		"FAKE_ROOT="+R,
		"PREFIX="+R+"/opt/rcfg-sim", "SYSTEMD_DIR="+R+"/etc/systemd/system", "SYSCTL_DIR="+R+"/etc/sysctl.d",
		"LIMITS_DIR="+R+"/etc/security/limits.d", "ETC_DIR="+R+"/etc/rcfg-sim", "UI_ETC_DIR="+R+"/etc/rcfg-sim-ui",
		"POLKIT_DIR="+R+"/etc/polkit-1/rules.d", "BIN_DIR="+R+"/build",
		"SYSTEMCTL="+r.fake("systemctl"), "GROUPADD="+r.fake("groupadd"), "USERADD="+r.fake("useradd"),
		"CHOWN="+r.fake("chown"), "GETENT="+r.fake("getent"), "ID="+r.fake("id"), "SYSCTL="+r.fake("sysctl"),
		"IPROUTE="+r.fake("iproute"),
		"IPS=2", "BASE_IP=10.60.0.1", "DEVICES_PER_IP=3", "PORT_START=20000", "DEVICES=5", "INTERFACE=lo",
		"UI_USER=", "UI_PASSWORD=",
	)
	for _, d := range []string{"etc/systemd/system", "etc/sysctl.d", "etc/security/limits.d", "etc/polkit-1/rules.d"} {
		if err := os.MkdirAll(filepath.Join(R, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *rig) fake(name string) string { return filepath.Join(r.fakes, name) }

// setup runs deploy/setup.sh from the repo with args and extra env.
func (r *rig) setup(extra []string, args ...string) (string, error) {
	r.t.Helper()
	cmd := exec.Command("bash", append([]string{"deploy/setup.sh"}, args...)...)
	cmd.Dir = r.repo
	cmd.Env = append(slices.Clone(r.env), extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *rig) calls() []string {
	b, _ := os.ReadFile(filepath.Join(r.root, "calls.log"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (r *rig) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(r.root, rel))
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// snapshot is every file under the root (but the fakes' log and state)
// with its mode and content hash.
func (r *rig) snapshot() map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(r.root, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(r.root, p)
		if err != nil || rel == "calls.log" || strings.HasPrefix(rel, "state") || strings.HasPrefix(rel, "fakes") || strings.HasPrefix(rel, "build") {
			return nil
		}
		fi, _ := os.Lstat(p)
		v := fi.Mode().String()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			v += " -> " + t
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			v += fmt.Sprintf(" %x", sha256.Sum256(b))
		}
		out[rel] = v
		return nil
	})
	return out
}

func TestSetupDryRun(t *testing.T) {
	r := newRig(t)
	before := r.snapshot()
	out, err := r.setup(nil, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"would run: " + r.fake("useradd") + " -r -s /sbin/nologin",
		"make --no-print-directory install install-ui",
		"would run: " + r.fake("sysctl") + " --quiet --system",
		"would write: " + r.root + "/etc/systemd/system/rcfg-sim-aliases.service",
		"would write: " + r.root + "/etc/rcfg-sim/10.60.0.1.env",
		"would write: " + r.root + "/etc/rcfg-sim/10.60.0.2.env",
		"rcfg-sim-gen --count 5",
		"would set UI_USERNAME=admin",
		"enable --now rcfg-sim@10.60.0.2.service",
		"enable --now rcfg-sim-ui.service",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run does not list %q:\n%s", want, out)
		}
	}
	if after := r.snapshot(); len(after) != len(before) {
		t.Fatalf("dry run changed files: %d -> %d", len(before), len(after))
	}
	for _, c := range r.calls() {
		if c != "" && !strings.HasPrefix(c, "id ") && !strings.HasPrefix(c, "systemctl is-") {
			t.Errorf("dry run made a change: %s", c)
		}
	}
}

func TestSetupRunAndRerun(t *testing.T) {
	r := newRig(t)
	out, err := r.setup(nil)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	pw := regexp.MustCompile(`Password \(shown once, stored only as a hash\): ([A-Za-z0-9]{20})\n`).FindStringSubmatch(out)
	if pw == nil || !strings.Contains(out, "ssh -L 8080:127.0.0.1:8080") {
		t.Fatalf("no password or tunnel command printed:\n%s", out)
	}

	// Users and groups, made only by the fakes.
	if got := r.read("state/users"); got != "rcfgsim\nrcfgsim-ui\n" {
		t.Errorf("users: %q", got)
	}
	if got := r.read("state/groups"); got != "rcfgsim-ui\n" {
		t.Errorf("groups: %q", got)
	}
	// Env files: setup's marker and keys, admin and metrics on loopback.
	for n, ip := range []string{"10.60.0.1", "10.60.0.2"} {
		env := r.read("etc/rcfg-sim/" + ip + ".env")
		if !strings.HasPrefix(env, "# managed by rcfg-sim setup (deploy/setup.sh)") {
			t.Errorf("%s.env has no setup marker", ip)
		}
		for _, want := range []string{"LISTEN_IP=" + ip, "PORT_START=20000", "PORT_COUNT=3",
			"MANIFEST=" + r.root + "/opt/rcfg-sim/data/current/manifest.csv",
			fmt.Sprintf("METRICS_ADDR=127.0.0.1:%d", 9101+n), fmt.Sprintf("ADMIN_ADDR=127.0.0.1:%d", 19101+n), "ALLOW_EMPTY=true"} {
			if !strings.Contains(env, want+"\n") {
				t.Errorf("%s.env missing %q:\n%s", ip, want, env)
			}
		}
	}
	// The first build, in the data/ layout.
	current, err := os.Readlink(filepath.Join(r.root, "opt/rcfg-sim/data/current"))
	if err != nil || !strings.HasPrefix(current, "builds/") {
		t.Fatalf("current -> %q (%v)", current, err)
	}
	man := r.read("opt/rcfg-sim/data/current/manifest.csv")
	if rows := strings.Count(man, "\n") - 1; rows != 5 || !strings.Contains(man, "10.60.0.2,20001") {
		t.Fatalf("manifest has %d rows:\n%s", rows, man)
	}
	if bj := r.read("opt/rcfg-sim/data/current/build.json"); !strings.Contains(bj, `"devices_per_ip": 3`) || !strings.Contains(bj, `"count": 5`) {
		t.Fatalf("build.json:\n%s", bj)
	}
	// The same settings_hash the UI computes, so a testbench reuses the build.
	built, err := jobs.ReadSettings(filepath.Join(r.root, "opt/rcfg-sim/data/current"))
	if err != nil {
		t.Fatal(err)
	}
	wantHash := jobs.HashSettings(map[string]int{"sm": 5}, 42, nil, jobs.Layout{IPBase: "10.60.0.1", IPCount: 2, PortStart: 20000, DevicesPerIP: 3})
	if built.SettingsHash != wantHash || built.LastUsed.IsZero() {
		t.Fatalf("build.json settings_hash %q, want %q (last_used %v)", built.SettingsHash, wantHash, built.LastUsed)
	}
	// The login: a bcrypt hash, never the password.
	ui := r.read("etc/rcfg-sim-ui/rcfg-sim-ui.env")
	if !strings.Contains(ui, "\nUI_USERNAME=admin\n") || !regexp.MustCompile(`\nUI_PASSWORD_HASH=\$2[aby]\$12\$`).MatchString(ui) || strings.Contains(ui, pw[1]) {
		t.Fatalf("ui env:\n%s", ui)
	}
	if !strings.Contains(r.read("etc/systemd/system/rcfg-sim-aliases.service"), "--interface lo --base-ip 10.60.0.1 --count 2") {
		t.Error("aliases unit")
	}
	// Units enabled and started.
	want := []string{"rcfg-sim-aliases.service", "rcfg-sim@10.60.0.1.service", "rcfg-sim@10.60.0.2.service", "rcfg-sim-ui.service"}
	if got := strings.Fields(r.read("state/enabled")); !slices.Equal(got, want) {
		t.Errorf("enabled units %v, want %v", got, want)
	}

	// A second run changes nothing.
	before, calls := r.snapshot(), len(r.calls())
	out, err = r.setup(nil)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	after := r.snapshot()
	for p, v := range after {
		if before[p] != v {
			t.Errorf("second run changed %s: %s -> %s", p, before[p], v)
		}
	}
	if len(after) != len(before) {
		t.Errorf("second run: %d files, was %d", len(after), len(before))
	}
	for _, c := range r.calls()[calls:] {
		for _, verb := range []string{"useradd", "groupadd", "systemctl enable", "systemctl restart", "systemctl daemon-reload"} {
			if strings.HasPrefix(c, verb) && !strings.HasPrefix(c, "systemctl daemon-reload") {
				t.Errorf("second run: %s", c)
			}
		}
	}
	if strings.Contains(out, "Password (shown once") {
		t.Error("second run made a new password")
	}

	// A runtime setting the UI wrote survives a rerun; a changed setup key
	// is put back and its instance restarted.
	envPath := filepath.Join(r.root, "etc/rcfg-sim/10.60.0.1.env")
	env := r.read("etc/rcfg-sim/10.60.0.1.env")
	env = strings.Replace(env, "PORT_COUNT=3", "PORT_COUNT=9", 1) + "FAULT_RATES=auth_fail=0.1\n"
	if err := os.WriteFile(envPath, []byte(env), 0o660); err != nil {
		t.Fatal(err)
	}
	calls = len(r.calls())
	if out, err = r.setup(nil); err != nil {
		t.Fatalf("third run: %v\n%s", err, out)
	}
	env = r.read("etc/rcfg-sim/10.60.0.1.env")
	if !strings.Contains(env, "PORT_COUNT=3\n") || !strings.Contains(env, "FAULT_RATES=auth_fail=0.1\n") {
		t.Fatalf("third run env:\n%s", env)
	}
	if !slices.Contains(r.calls()[calls:], "systemctl restart rcfg-sim@10.60.0.1.service") {
		t.Errorf("instance with a fixed env file not restarted: %v", r.calls()[calls:])
	}
}

func TestSetupMetricsBind(t *testing.T) {
	r := newRig(t)
	out, err := r.setup([]string{"METRICS_BIND=0.0.0.0"}, "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	r.env = append(r.env, "METRICS_BIND=0.0.0.0")
	if out, err = r.setup(nil); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	env := r.read("etc/rcfg-sim/10.60.0.2.env")
	if !strings.Contains(env, "METRICS_ADDR=0.0.0.0:9102\n") || !strings.Contains(env, "ADMIN_ADDR=127.0.0.1:19102\n") {
		t.Fatalf("env with METRICS_BIND=0.0.0.0:\n%s", env)
	}
	if out, err = r.setup([]string{"METRICS_BIND=nope"}, "--dry-run", "--force"); err == nil || !strings.Contains(out, "METRICS_BIND") {
		t.Fatalf("bad METRICS_BIND accepted: %s", out)
	}
}

func TestSetupRefusesForeignEnvFiles(t *testing.T) {
	r := newRig(t)
	etc := filepath.Join(r.root, "etc/rcfg-sim")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "10.60.0.1.env"), []byte("LISTEN_IP=10.60.0.1\nPORT_COUNT=2500\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	out, err := r.setup(nil, "--dry-run")
	if err == nil || !strings.Contains(out, "make adopt-data") || !strings.Contains(out, "--force") {
		t.Fatalf("want a refusal pointing at adopt-data and --force: %v\n%s", err, out)
	}
	if out, err = r.setup(nil, "--dry-run", "--force"); err != nil {
		t.Fatalf("--force: %v\n%s", err, out)
	}
	// Bad settings are refused before anything runs.
	out, err = r.setup([]string{"DEVICES=3"}, "--dry-run", "--force")
	if err == nil || !strings.Contains(out, "DEVICES must be 4 to 6") {
		t.Fatalf("DEVICES=3: %v\n%s", err, out)
	}
	if calls := r.calls(); len(calls) > 0 && calls[0] != "" {
		for _, c := range calls {
			if !strings.HasPrefix(c, "id ") && !strings.HasPrefix(c, "systemctl is-") {
				t.Errorf("a refused setup made a change: %s", c)
			}
		}
	}
}
