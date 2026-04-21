//go:build integration

package sshsrv_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// TestMetrics_EndToEnd stands up a real SSH listener, drives a few commands
// through a real SSH client, and asserts the exposed /metrics output contains
// the expected movements. Build-tagged `integration` so it doesn't run under
// the default `go test ./...`.
func TestMetrics_EndToEnd(t *testing.T) {
	tmp := t.TempDir()

	// Generate a small device set on loopback.
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	genCfg := configs.Config{
		Count:          3,
		OutputDir:      configsDir,
		ManifestPath:   manifest,
		IPBase:         "127.0.0.1",
		IPCount:        1,
		PortStart:      0, // replaced below
		DevicesPerIP:   3,
		Seed:           17,
		Distribution:   "small:100,medium:0,large:0,huge:0",
		Username:       "admin",
		Password:       "admin",
		EnablePassword: "enable123",
	}
	sshPort := freePort(t)
	metricsPort := freePort(t)
	genCfg.PortStart = sshPort

	if _, err := configs.Run(genCfg, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}

	// Bring up the server.
	hostKey := filepath.Join(tmp, "host")
	logFile, _ := os.CreateTemp(tmp, "server.log.*")
	defer logFile.Close()

	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP:              "127.0.0.1",
		PortStart:             sshPort,
		PortCount:             3,
		ManifestPath:          manifest,
		HostKeyPath:           hostKey,
		Username:              "admin",
		Password:              "admin",
		EnablePassword:        "enable123",
		ResponseDelayMinMS:    0,
		ResponseDelayMaxMS:    0,
		MaxConcurrentSessions: 16,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })

	// A well-authed session that runs three commands.
	drive(t, sshPort, "admin", "admin", []string{
		"terminal length 0", "show version", "show inventory", "exit",
	})

	// A session with a bad password — exercises the auth_fail path.
	driveExpectFail(t, sshPort, "admin", "BAD", "handshake failed")

	// Give handlers a moment to finish recording (runShell runs in its own
	// goroutine and may observe after the SSH Wait returned).
	time.Sleep(200 * time.Millisecond)

	// Direct counter checks via testutil (no HTTP).
	reg := srv.Metrics()
	gotOK := testutil.ToFloat64(reg.SessionsTotal.WithLabelValues("ok"))
	if gotOK < 1 {
		t.Errorf("rcfgsim_sessions_total{result=ok}: want >=1, got %v", gotOK)
	}
	gotAuthOK := testutil.ToFloat64(reg.AuthAttempts.WithLabelValues("ok"))
	if gotAuthOK < 1 {
		t.Errorf("rcfgsim_auth_attempts_total{result=ok}: want >=1, got %v", gotAuthOK)
	}
	gotAuthFail := testutil.ToFloat64(reg.AuthAttempts.WithLabelValues("fail"))
	if gotAuthFail < 1 {
		t.Errorf("rcfgsim_auth_attempts_total{result=fail}: want >=1, got %v", gotAuthFail)
	}

	if sampled := histogramSampleCount(t, reg.Gatherer(), "rcfgsim_handshake_duration_seconds"); sampled < 2 {
		t.Errorf("rcfgsim_handshake_duration_seconds: want >=2 samples (1 ok + 1 fail), got %d", sampled)
	}
	if sampled := histogramSampleCount(t, reg.Gatherer(), "rcfgsim_session_duration_seconds"); sampled < 1 {
		t.Errorf("rcfgsim_session_duration_seconds: want >=1 sample, got %d", sampled)
	}

	// Per-command: each dispatched command should have contributed at least
	// one sample to its own labelled histogram.
	for _, want := range []string{"CmdTerminalLength", "CmdShowVersion", "CmdShowInventory", "CmdExit"} {
		if n := histogramLabelSampleCount(t, reg.Gatherer(), "rcfgsim_command_duration_seconds", "command", want); n < 1 {
			t.Errorf("rcfgsim_command_duration_seconds{command=%s}: want >=1 sample, got %d", want, n)
		}
	}

	if b := testutil.ToFloat64(reg.BytesSent); b < 1000 {
		t.Errorf("rcfgsim_bytes_sent_total: want >=1000 (show version is ~1.5KB), got %v", b)
	}

	// HTTP exposition: scrape and assert the output is well-formed.
	body := scrape(t, metricsPort)
	for _, want := range []string{
		`rcfgsim_sessions_total{result="ok"}`,
		`rcfgsim_auth_attempts_total{result="ok"}`,
		`rcfgsim_auth_attempts_total{result="fail"}`,
		`rcfgsim_command_duration_seconds_count{command="CmdShowVersion"}`,
		`rcfgsim_faults_injected_total{type="auth_fail"}`, // pre-registered at 0
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing line for %q", want)
		}
	}
}

// TestMetrics_Cardinality scrapes /metrics after the end-to-end test has run
// and asserts no rcfgsim_* metric family has an unreasonable number of unique
// label combinations. Tightly bounded labels are the whole point of the
// "never log hostnames or session IDs" rule — this test enforces it.
func TestMetrics_Cardinality(t *testing.T) {
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	sshPort := freePort(t)
	metricsPort := freePort(t)

	if _, err := configs.Run(configs.Config{
		Count: 3, OutputDir: configsDir, ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 3,
		Seed: 17, Distribution: "small:100,medium:0,large:0,huge:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}

	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 3,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		MaxConcurrentSessions: 4,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })

	// Run several sessions, mix ok and bad auth.
	for i := 0; i < 3; i++ {
		drive(t, sshPort, "admin", "admin", []string{"show version", "show inventory", "sh run", "exit"})
	}
	driveExpectFail(t, sshPort, "admin", "BAD", "handshake failed")

	time.Sleep(200 * time.Millisecond)

	mfs, err := srv.Metrics().Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	unique := 0
	perMetric := map[string]int{}
	for _, mf := range mfs {
		name := mf.GetName()
		if !strings.HasPrefix(name, "rcfgsim_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			key := name + "|"
			for _, lp := range m.GetLabel() {
				key += lp.GetName() + "=" + lp.GetValue() + ","
			}
			unique++
			perMetric[name]++
			_ = key
		}
	}

	// 11 commands + 4 session_results + 2 auth_results + 4 fault_types
	// + 1 each for active_sessions, session_duration, handshake_duration,
	//   bytes_sent  = 11+4+2+4+4 = 25 unique rcfgsim_* combos.
	// Cap at 50 so future additions have headroom but a typo that adds
	// "hostname" or "port" as a label would immediately blow the limit.
	const maxUnique = 50
	if unique > maxUnique {
		t.Errorf("rcfgsim_* unique label combinations = %d, want <= %d; per-metric: %v",
			unique, maxUnique, perMetric)
	}
	t.Logf("rcfgsim_* unique label combinations: %d (cap %d)", unique, maxUnique)
}

func drive(t *testing.T, port int, user, pass string, cmds []string) {
	t.Helper()
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial port %d: %v", port, err)
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	_ = sess.RequestPty("xterm", 80, 24, ssh.TerminalModes{ssh.ECHO: 0})
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, stdout)
	}()
	time.Sleep(150 * time.Millisecond)
	for _, cmd := range cmds {
		fmt.Fprintln(stdin, cmd)
		time.Sleep(120 * time.Millisecond)
	}
	_ = stdin.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	_ = sess.Wait()
}

func driveExpectFail(t *testing.T, port int, user, pass, wantErrSubstring string) {
	t.Helper()
	_, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	if err == nil {
		t.Fatalf("dial %d: expected auth failure, got success", port)
	}
	if !strings.Contains(err.Error(), wantErrSubstring) {
		t.Fatalf("dial %d: err %q does not contain %q", port, err, wantErrSubstring)
	}
}

func scrape(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape: want 200, got %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("scrape read: %v", err)
	}
	return string(b)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func histogramSampleCount(t *testing.T, g interface {
	Gather() ([]*dto.MetricFamily, error)
}, name string) uint64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var total uint64
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h != nil {
				total += h.GetSampleCount()
			}
		}
		return total
	}
	return 0
}

func histogramLabelSampleCount(t *testing.T, g interface {
	Gather() ([]*dto.MetricFamily, error)
}, name, labelKey, labelVal string) uint64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := false
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelVal {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			h := m.GetHistogram()
			if h != nil {
				return h.GetSampleCount()
			}
		}
	}
	return 0
}
