//go:build integration

package sshsrv_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// withProfiledServer stands up a server like withServer, with --device-profiles
// set to spec and, when manifestProfiles is given, a `profile` column holding
// manifestProfiles[i] for device i. Returns the server and its first SSH port.
func withProfiledServer(t *testing.T, spec string, devices int, manifestProfiles []string) (*sshsrv.Server, int) {
	t.Helper()
	return withProfiledServerCap(t, spec, devices, manifestProfiles, 0)
}

// withProfiledServerCap is withProfiledServer with --max-session set to maxSession
// (zero keeps the default).
func withProfiledServerCap(t *testing.T, spec string, devices int, manifestProfiles []string, maxSession time.Duration) (*sshsrv.Server, int) {
	t.Helper()
	tmp := t.TempDir()
	sshPort := freePorts(t, devices)
	metricsPort := freePort(t)

	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: devices, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: devices,
		Seed: 42, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	if manifestProfiles != nil {
		addProfileColumn(t, manifest, manifestProfiles)
	}

	rules, err := profile.Parse(spec)
	if err != nil {
		t.Fatalf("profile.Parse(%q): %v", spec, err)
	}
	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: devices,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		MaxConcurrentSessions: 32,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", metricsPort),
		DeviceProfiles:        rules,
		ProfileSeed:           42,
		MaxSessionDuration:    maxSession,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	return srv, sshPort
}

func addProfileColumn(t *testing.T, manifest string, profiles []string) {
	t.Helper()
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	lines[0] += ",profile"
	for i := 1; i < len(lines) && i <= len(profiles); i++ {
		lines[i] += "," + profiles[i-1]
	}
	if err := os.WriteFile(manifest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

var fetchConfig = []string{"terminal length 0", "show running-config"}

func TestProfile_Hang_StallsThenAnswersQueuedCommands(t *testing.T) {
	_, port := withProfiledServer(t, "hang:2s@100%", 1, nil)

	start := time.Now()
	got, err := driveAndCollect(port, "admin", "admin", fetchConfig, 8*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if elapsed < 2*time.Second {
		t.Fatalf("session took %v, want at least the 2s hang", elapsed)
	}
	if !bytes.Contains(got, []byte("Building configuration")) {
		t.Fatalf("commands sent during the hang were not answered after it; got %d bytes", len(got))
	}
}

func TestProfile_Hang_DoesNotHoldUpShutdown(t *testing.T) {
	srv, port := withProfiledServer(t, "hang:1h@100%", 1, nil)

	go func() { _, _ = driveAndCollect(port, "admin", "admin", fetchConfig, 10*time.Second) }()
	time.Sleep(700 * time.Millisecond)

	start := time.Now()
	srv.Shutdown(5 * time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %v with a hung session; the hang must end on shutdown", elapsed)
	}
}

func TestProfile_Refuse_DropsBeforeHandshake(t *testing.T) {
	srv, port := withProfiledServer(t, "refuse@100%", 1, nil)

	_, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: "admin", Auth: []ssh.AuthMethod{ssh.Password("admin")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	if err == nil {
		t.Fatal("dial succeeded against a refuse device")
	}

	time.Sleep(100 * time.Millisecond)
	if v := testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("error")); v < 1 {
		t.Errorf("sessions_total{error}: want >=1, got %v", v)
	}
}

func TestProfile_Latency_DelaysEveryCommand(t *testing.T) {
	_, port := withProfiledServer(t, "latency:300ms@100%", 1, nil)

	cmds := []string{"terminal length 0", "show version", "show running-config"}
	start := time.Now()
	got, err := driveAndCollect(port, "admin", "admin", cmds, 8*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("3 commands took %v, want at least 3 x 300ms", elapsed)
	}
	if !bytes.Contains(got, []byte("Building configuration")) {
		t.Fatal("latency device did not return its config")
	}
}

func TestProfile_Disconnect_CutsEveryConfig(t *testing.T) {
	srv, port := withProfiledServer(t, "disconnect@100%", 1, nil)

	for i := 0; i < 3; i++ {
		got, _ := driveAndCollect(port, "admin", "admin", fetchConfig, 3*time.Second)
		if bytes.Contains(got, []byte("\nend\n")) || bytes.Contains(got, []byte("\nend\r\n")) {
			t.Fatalf("attempt %d: config was not cut", i+1)
		}
	}

	time.Sleep(200 * time.Millisecond)
	if v := testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("disconnect")); v < 3 {
		t.Errorf("sessions_total{disconnect}: want >=3, got %v", v)
	}
}

func TestProfile_ManifestColumnPinsOneDevice(t *testing.T) {
	_, port := withProfiledServer(t, "", 2, []string{"refuse", ""})

	if _, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: "admin", Auth: []ssh.AuthMethod{ssh.Password("admin")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	}); err == nil {
		t.Fatal("device pinned to refuse in the manifest accepted a session")
	}

	got, err := driveAndCollect(port+1, "admin", "admin", fetchConfig, 3*time.Second)
	if err != nil || !bytes.Contains(got, []byte("Building configuration")) {
		t.Fatalf("device with an empty profile column should behave normally; err=%v bytes=%d", err, len(got))
	}
}

func TestProfile_NoProfilesLeavesDevicesNormal(t *testing.T) {
	_, port := withProfiledServer(t, "", 1, nil)

	got, err := driveAndCollect(port, "admin", "admin", fetchConfig, 3*time.Second)
	if err != nil || !bytes.Contains(got, []byte("Building configuration")) {
		t.Fatalf("normal device: err=%v bytes=%d", err, len(got))
	}
}

func TestProfile_BadManifestProfileRefusesToStart(t *testing.T) {
	tmp := t.TempDir()
	sshPort := freePorts(t, 1)
	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 1,
		Seed: 42, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	addProfileColumn(t, manifest, []string{"hang:forever"})

	_, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		MaxConcurrentSessions: 4,
	})
	if err == nil || !strings.Contains(err.Error(), "hang:forever") {
		t.Fatalf("want a startup error naming the bad profile, got %v", err)
	}
}

// openSession logs in and starts a shell, returning a channel that closes when
// the simulator ends the connection.
func openSession(t *testing.T, port int) <-chan struct{} {
	t.Helper()
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: "admin", Auth: []ssh.AuthMethod{ssh.Password("admin")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	_ = sess.RequestPty("xterm", 200, 50, ssh.TerminalModes{})
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	closed := make(chan struct{})
	go func() { _ = c.Wait(); close(closed) }()
	return closed
}

func TestMaxSession_ClosesSessionAtTheCap(t *testing.T) {
	_, port := withProfiledServerCap(t, "", 1, nil, time.Second)

	start := time.Now()
	select {
	case <-openSession(t, port):
		if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
			t.Fatalf("session closed after %v, before the 1s cap", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("session still open 4s after a 1s cap")
	}
}

func TestMaxSession_LongerCapKeepsSlowSessionOpen(t *testing.T) {
	_, port := withProfiledServerCap(t, "latency:300ms@100%", 1, nil, 3*time.Second)

	select {
	case <-openSession(t, port):
		t.Fatal("session closed before its 3s cap")
	case <-time.After(1500 * time.Millisecond):
	}
}
