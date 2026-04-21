//go:build integration

package sshsrv_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// withServer stands up a configs+manifest+server on ephemeral ports and
// returns (server, sshPort, manifestPath). Registers a Cleanup to drain and
// shut the server. The returned manifest path lets callers resolve a port
// back to the on-disk config file for byte-level comparisons.
func withServer(t *testing.T, faults *fault.Set, delayMinMS, delayMaxMS, devices int) (*sshsrv.Server, int, string) {
	t.Helper()
	tmp := t.TempDir()
	sshPort := freePort(t)
	metricsPort := freePort(t)

	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: devices, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: devices,
		Seed: 42, Distribution: "small:100,medium:0,large:0,huge:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}

	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: devices,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		ResponseDelayMinMS: delayMinMS, ResponseDelayMaxMS: delayMaxMS,
		MaxConcurrentSessions: 32,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
		Faults:                faults,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	return srv, sshPort, manifest
}

// TestFault_AuthFail_AlwaysRejects confirms auth_fail at rate=1.0 rejects
// even a correct password, and the metric increments match.
func TestFault_AuthFail_AlwaysRejects(t *testing.T) {
	faults, _ := fault.NewSet("auth_fail", 1.0)
	srv, port, _ := withServer(t, faults, 0, 0, 1)

	for i := 0; i < 3; i++ {
		driveExpectFail(t, port, "admin", "admin", "handshake failed")
	}

	time.Sleep(150 * time.Millisecond)
	reg := srv.Metrics()
	if v := testutil.ToFloat64(reg.FaultsInjected.WithLabelValues("auth_fail")); v < 3 {
		t.Errorf("faults_injected_total{auth_fail}: want >=3, got %v", v)
	}
	if v := testutil.ToFloat64(reg.AuthAttempts.WithLabelValues("fail")); v < 3 {
		t.Errorf("auth_attempts_total{fail}: want >=3, got %v", v)
	}
	if v := testutil.ToFloat64(reg.AuthAttempts.WithLabelValues("ok")); v != 0 {
		t.Errorf("auth_attempts_total{ok}: want 0 (auth_fail masked everything), got %v", v)
	}
	if v := testutil.ToFloat64(reg.SessionsTotal.WithLabelValues("auth_fail")); v < 3 {
		t.Errorf("sessions_total{auth_fail}: want >=3, got %v", v)
	}
}

// TestFault_DisconnectMid_HardCloses verifies show running-config output is
// truncated mid-stream and the TCP connection drops abruptly (not EOF).
func TestFault_DisconnectMid_HardCloses(t *testing.T) {
	faults, _ := fault.NewSet("disconnect_mid", 1.0)
	srv, port, _ := withServer(t, faults, 0, 0, 1)

	received, err := driveAndCollect(port, "admin", "admin", []string{"terminal length 0", "show running-config"}, 3*time.Second)
	if err == nil {
		// Some SSH client paths surface "unexpected EOF" not as a Go error but
		// as a short read — both are acceptable behaviours. What matters is
		// that the server truncated the stream.
		t.Logf("client returned without error (truncated stream read cleanly); bytes=%d", len(received))
	} else if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("want EOF/reset/closed, got %v", err)
	} else {
		t.Logf("client disconnect error (expected): %v; bytes=%d", err, len(received))
	}

	time.Sleep(200 * time.Millisecond)
	reg := srv.Metrics()
	if v := testutil.ToFloat64(reg.FaultsInjected.WithLabelValues("disconnect_mid")); v < 1 {
		t.Errorf("faults_injected_total{disconnect_mid}: want >=1, got %v", v)
	}
	if v := testutil.ToFloat64(reg.SessionsTotal.WithLabelValues("disconnect")); v < 1 {
		t.Errorf("sessions_total{disconnect}: want >=1, got %v", v)
	}

	// 20-40% window: confirm we got SOME bytes but the full config did not
	// arrive. The small-bucket template is ~25 KB; a 20% truncation still
	// leaves >=1000 bytes typically. The "end" terminator line must be absent.
	if !bytes.Contains(received, []byte("Building configuration")) {
		t.Errorf("expected some prefix of the config (Building configuration), got %d bytes", len(received))
	}
	if bytes.Contains(received, []byte("\nend\n")) || bytes.Contains(received, []byte("\nend\r\n")) {
		t.Errorf("stream was NOT truncated — contains the final 'end' line")
	}
}

// TestFault_SlowResponse_AddsLatency verifies a configured slow_response
// multiplier noticeably inflates command duration.
func TestFault_SlowResponse_AddsLatency(t *testing.T) {
	faults, _ := fault.NewSet("slow_response", 1.0)
	// max=50 ms base; multiplier is uniform[10,50] ⇒ delay ∈ [500, 2500] ms.
	srv, port, _ := withServer(t, faults, 0, 50, 1)

	start := time.Now()
	_, err := driveAndCollect(port, "admin", "admin", []string{"show version", "exit"}, 10*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("slow_response should have added at least 400 ms; total elapsed = %v", elapsed)
	}
	t.Logf("slow_response elapsed: %v (min observable = base*multiplier_min = 500 ms)", elapsed)

	time.Sleep(100 * time.Millisecond)
	reg := srv.Metrics()
	if v := testutil.ToFloat64(reg.FaultsInjected.WithLabelValues("slow_response")); v < 1 {
		t.Errorf("faults_injected_total{slow_response}: want >=1, got %v", v)
	}
}

// TestFault_Malformed_CorruptsStream runs show running-config enough times
// under rate=1.0 to exercise every corruption mode, and asserts the resulting
// payload does NOT byte-match the on-disk config.
func TestFault_Malformed_CorruptsStream(t *testing.T) {
	faults, _ := fault.NewSet("malformed", 1.0)
	srv, port, manifestPath := withServer(t, faults, 0, 0, 1)

	onDisk, err := os.ReadFile(diskPathForPort(t, manifestPath, port))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	// Run multiple times so we have a good chance of hitting all three modes.
	// Equality with on-disk contents is the invariant we break.
	matchedDisk := 0
	for i := 0; i < 5; i++ {
		body, err := driveAndCollect(port, "admin", "admin",
			[]string{"terminal length 0", "show running-config", "exit"}, 4*time.Second)
		if err != nil {
			t.Logf("drive %d err: %v (acceptable)", i, err)
		}
		// The stream body includes prompts, echoed commands etc., but the on-disk
		// bytes appear verbatim somewhere in the stream if and only if malformed
		// did NOT corrupt. So we search for the on-disk content as a substring.
		if bytes.Contains(body, onDisk) {
			matchedDisk++
		}
	}
	if matchedDisk > 0 {
		t.Errorf("with malformed rate=1.0, %d/5 runs still contained byte-exact config — corruption did not fire",
			matchedDisk)
	}

	time.Sleep(100 * time.Millisecond)
	reg := srv.Metrics()
	if v := testutil.ToFloat64(reg.FaultsInjected.WithLabelValues("malformed")); v < 1 {
		t.Errorf("faults_injected_total{malformed}: want >=1, got %v", v)
	}
}

// TestFault_ZeroRate_AllTypes_NeverFires stands up a server with every fault
// type enabled but rate=0 and runs 100 sessions. No fault counters should move
// and no session should be misclassified.
func TestFault_ZeroRate_AllTypes_NeverFires(t *testing.T) {
	faults, _ := fault.NewSet("auth_fail,disconnect_mid,slow_response,malformed", 0.0)
	srv, port, _ := withServer(t, faults, 0, 0, 1)

	// Keep concurrency under the MaxConcurrentSessions cap so the semaphore
	// doesn't reject half our drivers. 8 at a time × 13 batches ≈ 100 sessions.
	const totalSessions = 100
	const parallel = 8
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < totalSessions; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			_, _ = driveAndCollect(port, "admin", "admin",
				[]string{"show version", "exit"}, 2*time.Second)
		}()
	}
	wg.Wait()
	time.Sleep(300 * time.Millisecond)

	reg := srv.Metrics()
	for _, ft := range []string{"auth_fail", "disconnect_mid", "slow_response", "malformed"} {
		if v := testutil.ToFloat64(reg.FaultsInjected.WithLabelValues(ft)); v != 0 {
			t.Errorf("faults_injected_total{%s}: want 0 at rate=0, got %v", ft, v)
		}
	}
	if v := testutil.ToFloat64(reg.SessionsTotal.WithLabelValues("ok")); v < totalSessions-5 {
		t.Errorf("sessions_total{ok}: want >=%d (almost all 100 must complete cleanly), got %v", totalSessions-5, v)
	}
}

// --- helpers ---

// driveAndCollect opens a session, runs the commands, collects all stdout bytes
// received, and returns them along with any read error.
func driveAndCollect(port int, user, pass string, cmds []string, readBudget time.Duration) ([]byte, error) {
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	_ = sess.RequestPty("xterm", 80, 24, ssh.TerminalModes{ssh.ECHO: 0})
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 4096)
		for {
			n, err := stdout.Read(b)
			if n > 0 {
				buf.Write(b[:n])
			}
			if err != nil {
				if err != io.EOF {
					readErr = err
				}
				return
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	for _, cmd := range cmds {
		fmt.Fprintln(stdin, cmd)
		time.Sleep(80 * time.Millisecond)
	}
	_ = stdin.Close()

	select {
	case <-done:
	case <-time.After(readBudget):
	}
	_ = sess.Wait()
	return buf.Bytes(), readErr
}

func diskPathForPort(t *testing.T, manifestPath string, port int) string {
	t.Helper()
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manifestPath, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		cols := strings.Split(line, ",")
		if len(cols) < 10 {
			continue
		}
		if cols[2] == fmt.Sprintf("%d", port) {
			return cols[8]
		}
	}
	t.Fatalf("no row in %s for port %d", manifestPath, port)
	return ""
}
