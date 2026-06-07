//go:build integration

package sshsrv_test

// Over-the-wire tests for the Ciena 6500 TL1 driver: the in-band ACT-USER login
// gate, zero-copy RTRV-EQPT streaming of the generated inventory, pre-login
// DENY, and a ";"-terminated command split across physical lines.

import (
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// cienaServer generates a single Ciena 6500 device and serves it on loopback
// with the default (password) SSH auth mode.
func cienaServer(t *testing.T) (port int, hostname string, srv *sshsrv.Server) {
	return cienaModelServer(t, "", "ciena-6500-tl1")
}

// cienaServerMode is cienaServer with an explicit --ssh-auth mode.
func cienaServerMode(t *testing.T, authMode string) (port int, hostname string, srv *sshsrv.Server) {
	return cienaModelServer(t, authMode, "ciena-6500-tl1")
}

// cienaModelServer generates one device of the given model and serves it.
func cienaModelServer(t *testing.T, authMode, model string) (port int, hostname string, srv *sshsrv.Server) {
	t.Helper()
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	sshPort := freePort(t)

	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: configsDir, ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 1,
		Seed: 7, Distribution: model + ":100",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	hostname = manifestHostname(t, manifest, sshPort)

	var err error
	srv, err = sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		SSHAuthMode:        authMode,
		ResponseDelayMinMS: 0, ResponseDelayMaxMS: 0,
		MaxConcurrentSessions: 4,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	return sshPort, hostname, srv
}

func TestCiena_LoginAndRtrvEqpt(t *testing.T) {
	port, sid, srv := cienaServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	// Bare "<" greeting/prompt.
	ec.expect("< ", 3*time.Second)

	// In-band login.
	ec.reset()
	ec.send("ACT-USER::admin:100::admin;")
	login := ec.expect("M  100 COMPLD", 3*time.Second)
	if !strings.Contains(login, sid) {
		t.Errorf("login COMPLD should carry SID %q: %q", sid, login)
	}

	// RTRV-EQPT streams the generated inventory (zero-copy) wrapped in COMPLD.
	ec.reset()
	ec.send("RTRV-EQPT::ALL:101;")
	eqpt := ec.expect("M  101 COMPLD", 3*time.Second)
	ec.expect("TYPE=6500-7SLOT", 3*time.Second) // streamed payload body
	if !strings.Contains(eqpt, sid) {
		t.Errorf("RTRV-EQPT COMPLD should carry SID: %q", eqpt)
	}

	// command_duration must have a sample under the TL1 label.
	if n := histogramLabelSampleCount(t, srv.Metrics().Gatherer(),
		"rcfgsim_command_duration_seconds", "command", "CmdTL1RtrvEqpt"); n < 1 {
		t.Errorf("rcfgsim_command_duration_seconds{command=CmdTL1RtrvEqpt}: want >=1 sample, got %d", n)
	}
}

func TestCiena_PreLoginDeny(t *testing.T) {
	port, _, _ := cienaServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("RTRV-ALM-ALL::ALL:200;")
	got := ec.expect("M  200 DENY", 3*time.Second)
	if !strings.Contains(got, "PLNA") {
		t.Errorf("pre-login DENY should carry PLNA: %q", got)
	}
}

// TestCiena_NoSSHAuth (scenario A: TL1-only) — with --ssh-auth=none the client
// connects offering no auth methods, lands on the "<" prompt unchallenged, and
// authenticates purely in-band via ACT-USER.
func TestCiena_NoSSHAuth(t *testing.T) {
	port, sid, _ := cienaServerMode(t, "none")
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:100::admin;")
	got := ec.expect("M  100 COMPLD", 3*time.Second)
	if !strings.Contains(got, sid) {
		t.Errorf("TL1 login COMPLD should carry SID %q: %q", sid, got)
	}
}

// TestCiena_DriverModeNoAuth (scenario A via per-driver default) — with
// --ssh-auth=driver, a Ciena device (RequiresSSHAuth=false) accepts a no-auth
// client, while Cisco devices on the same mode would still require a password.
func TestCiena_DriverModeNoAuth(t *testing.T) {
	port, _, _ := cienaServerMode(t, "driver")
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:1::admin;")
	ec.expect("M  1 COMPLD", 3*time.Second)
}

// TestCiena_GNE_RNERouting drives the full GNE/RNE example session over the wire:
// log in to the GNE, list RNEs via RTRV-NBR, address an RNE by TID (EQPT streamed
// with the RNE's SID in the header), confirm GNE-local commands still work, and
// that an unknown TID is denied with IIAC.
func TestCiena_GNE_RNERouting(t *testing.T) {
	port, gneSID, srv := cienaModelServer(t, "none", "ciena-6500-tl1-gne")
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:1::admin;")
	ec.expect("M  1 COMPLD", 3*time.Second)

	// RTRV-NBR lists the RNEs behind this GNE; pull one TID out of the response.
	ec.reset()
	ec.send("RTRV-NBR:ALL:2;")
	nbr := ec.expect("M  2 COMPLD", 3*time.Second)
	m := regexp.MustCompile(`"(RNE-[A-Z0-9]+):`).FindStringSubmatch(nbr)
	if m == nil {
		t.Fatalf("RTRV-NBR returned no RNE TID: %q", nbr)
	}
	rne := m[1]

	// RTRV-EQPT to that RNE: COMPLD, header SID is the RNE TID (3-space SID line,
	// distinct from the command echo), inventory streamed.
	ec.reset()
	ec.send(fmt.Sprintf("RTRV-EQPT:%s:3;", rne))
	eqpt := ec.expect("M  3 COMPLD", 3*time.Second)
	if !strings.Contains(eqpt, "   "+rne+" ") {
		t.Errorf("RNE EQPT header should carry RNE TID %q as SID: %q", rne, eqpt)
	}
	ec.expect("TYPE=6500-7SLOT", 3*time.Second)

	// GNE-local EQPT still works; header SID is the GNE.
	ec.reset()
	ec.send("RTRV-EQPT::ALL:100;")
	local := ec.expect("M  100 COMPLD", 3*time.Second)
	if !strings.Contains(local, "   "+gneSID+" ") {
		t.Errorf("local EQPT header should carry GNE SID %q: %q", gneSID, local)
	}

	// RNE-targeted alarm completes.
	ec.reset()
	ec.send(fmt.Sprintf("RTRV-ALM-ALL:%s:4;", rne))
	ec.expect("M  4 COMPLD", 3*time.Second)

	// Unknown / unreachable TID -> DENY IIAC.
	ec.reset()
	ec.send("RTRV-EQPT:RNE-NOPE:9;")
	deny := ec.expect("M  9 DENY", 3*time.Second)
	if !strings.Contains(deny, "IIAC") {
		t.Errorf("unknown TID should DENY/IIAC: %q", deny)
	}

	if n := histogramLabelSampleCount(t, srv.Metrics().Gatherer(),
		"rcfgsim_command_duration_seconds", "command", "CmdTL1RtrvNbr"); n < 1 {
		t.Errorf("rcfgsim_command_duration_seconds{command=CmdTL1RtrvNbr}: want >=1 sample, got %d", n)
	}
}

// TestDriverMode_CiscoRejectsNoAuth is the negative of TestCiena_DriverModeNoAuth:
// under --ssh-auth=driver a Cisco device (RequiresSSHAuth=true) still requires SSH
// password auth, so a client offering no auth methods must fail the handshake.
func TestDriverMode_CiscoRejectsNoAuth(t *testing.T) {
	port, _, _ := charServerMode(t, "driver")
	_, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &ssh.ClientConfig{
		User:            "anyone",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err == nil {
		t.Fatal("Cisco device under --ssh-auth=driver should reject a no-auth client")
	}
	if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("expected an authentication failure, got: %v", err)
	}
}

// TestCiena_MultiLineCommand sends a ";"-terminated command split across two
// physical lines and asserts it still parses (CTAG 102 echoed in COMPLD).
func TestCiena_MultiLineCommand(t *testing.T) {
	port, _, _ := cienaServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:1::admin;")
	ec.expect("M  1 COMPLD", 3*time.Second)

	// "RTRV-SYS:::" then a newline, then "102;" — one TL1 command, two lines.
	ec.reset()
	ec.send("RTRV-SYS:::")
	ec.send("102;")
	got := ec.expect("M  102 COMPLD", 3*time.Second)
	if !strings.Contains(got, "TYPE=6500-7SLOT") {
		t.Errorf("RTRV-SYS payload missing system line: %q", got)
	}
}
