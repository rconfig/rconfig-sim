//go:build integration

package sshsrv_test

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// dialErr opens and closes one SSH connection, returning the dial error.
func dialErr(port int) error {
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User: "admin", Auth: []ssh.AuthMethod{ssh.Password("admin")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
	})
	if err == nil {
		_ = c.Close()
	}
	return err
}

// TestRuntime_SwapAffectsNewSessionsOnly swaps in auth_fail and
// disconnect_mid at 1.0 while a session is open. The open session keeps its
// fault-free snapshot and streams the whole config; a new session gets the
// new faults and fails auth.
func TestRuntime_SwapAffectsNewSessionsOnly(t *testing.T) {
	srv, port, manifest := withServer(t, nil, 0, 0, 1)
	host := manifestHostname(t, manifest, port)

	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()
	ec.expect(host+">", 3*time.Second)

	before := srv.Runtime().Load()
	faults, err := fault.ParseRates("auth_fail=1,disconnect_mid=1")
	if err != nil {
		t.Fatal(err)
	}
	next := before.Clone()
	next.Faults = faults
	if got := srv.Runtime().Store(next).Generation; got != before.Generation+1 {
		t.Fatalf("generation: want %d, got %d", before.Generation+1, got)
	}

	// In flight: no disconnect, full config.
	ec.reset()
	ec.send("terminal length 0")
	ec.expect(host+">", 3*time.Second)
	ec.reset()
	ec.send("show running-config")
	got := ec.expect("\nend", 5*time.Second)
	if !strings.Contains(got, "Building configuration") {
		t.Errorf("in-flight session: config not streamed: %q", got)
	}
	if v := testutil.ToFloat64(srv.Metrics().FaultsInjected.WithLabelValues("disconnect_mid")); v != 0 {
		t.Errorf("in-flight session rolled disconnect_mid after the swap: %v", v)
	}

	// New session: auth_fail at 1.0.
	driveExpectFail(t, port, "admin", "admin", "handshake failed")
	if v := testutil.ToFloat64(srv.Metrics().FaultsInjected.WithLabelValues("auth_fail")); v < 1 {
		t.Errorf("new session: faults_injected_total{auth_fail}: want >=1, got %v", v)
	}
}

// TestRuntime_ActiveDevicesRefusesTheRest lowers the active device count and
// checks devices past it drop connections like the refuse profile.
func TestRuntime_ActiveDevicesRefusesTheRest(t *testing.T) {
	srv, port, _ := withServer(t, nil, 0, 0, 3)

	next := srv.Runtime().Load().Clone()
	next.ActiveDevices = 1
	srv.Runtime().Store(next)

	if err := dialErr(port); err != nil {
		t.Fatalf("device 0 (active): %v", err)
	}
	errsBefore := testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("error"))
	for i := 1; i < 3; i++ {
		if err := dialErr(port + i); err == nil {
			t.Fatalf("device %d (inactive): dial succeeded", i)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if v := testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("error")); v < errsBefore+2 {
		t.Errorf("sessions_total{error}: want >=%v, got %v", errsBefore+2, v)
	}

	next = srv.Runtime().Load().Clone()
	next.ActiveDevices = 3
	srv.Runtime().Store(next)
	if err := dialErr(port + 2); err != nil {
		t.Fatalf("device 2 after reactivating: %v", err)
	}
}

// TestRuntime_SessionCapSwap lowers the cap to one, holds a session, and
// checks a second is refused until the cap is raised.
func TestRuntime_SessionCapSwap(t *testing.T) {
	srv, port, manifest := withServer(t, nil, 0, 0, 1)
	host := manifestHostname(t, manifest, port)

	next := srv.Runtime().Load().Clone()
	next.MaxSessions = 1
	srv.Runtime().Store(next)

	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()
	ec.expect(host+">", 3*time.Second)

	if err := dialErr(port); err == nil {
		t.Fatal("second session admitted at cap 1")
	}

	next = srv.Runtime().Load().Clone()
	next.MaxSessions = 2
	srv.Runtime().Store(next)
	if err := dialErr(port); err != nil {
		t.Fatalf("second session at cap 2: %v", err)
	}
}

// TestRuntime_ProfilesSwap rebuilds the profile map with ResolveProfiles and
// swaps it in: a refuse rule now drops new connections.
func TestRuntime_ProfilesSwap(t *testing.T) {
	srv, port, _ := withServer(t, nil, 0, 0, 1)
	if err := dialErr(port); err != nil {
		t.Fatalf("before swap: %v", err)
	}

	rules, err := profile.Parse("refuse@100%")
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := srv.ResolveProfiles(rules, 42)
	if err != nil {
		t.Fatal(err)
	}
	next := srv.Runtime().Load().Clone()
	next.Profiles = profiles
	srv.Runtime().Store(next)

	if err := dialErr(port); err == nil {
		t.Fatal("after swap to refuse@100%: dial succeeded")
	}
}

// TestActiveDevicesConfig checks the --active-devices start value: in range
// it is used, out of range New refuses to start.
func TestActiveDevicesConfig(t *testing.T) {
	tmp := t.TempDir()
	sshPort := freePorts(t, 2)
	manifest := filepath.Join(tmp, "manifest.csv")
	if _, err := configs.Run(configs.Config{
		Count: 2, OutputDir: filepath.Join(tmp, "configs"), ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 2,
		Seed: 42, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	cfg := func(active *int) sshsrv.Config {
		return sshsrv.Config{
			ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 2,
			ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
			Username: "admin", Password: "admin", EnablePassword: "enable123",
			MaxConcurrentSessions: 4, ActiveDevices: active,
		}
	}
	intp := func(n int) *int { return &n }

	for _, bad := range []int{-1, 3} {
		if _, err := sshsrv.New(cfg(intp(bad))); err == nil {
			t.Errorf("ActiveDevices=%d: New succeeded, want range error", bad)
		}
	}
	for _, tc := range []struct {
		active *int
		want   int
	}{{nil, 2}, {intp(0), 0}, {intp(1), 1}} {
		srv, err := sshsrv.New(cfg(tc.active))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		srv.Shutdown(time.Second) // never started; releases the mmaps
		live := srv.Runtime().Load()
		if live.ActiveDevices != tc.want || live.MaxSessions != 4 || live.Generation != 1 {
			t.Errorf("active=%v: got ActiveDevices=%d MaxSessions=%d Generation=%d",
				tc.active, live.ActiveDevices, live.MaxSessions, live.Generation)
		}
	}
}
