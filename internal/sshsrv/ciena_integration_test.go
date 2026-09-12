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

// cienaModelServer generates one device of the given model and serves it with
// the standard test password.
func cienaModelServer(t *testing.T, authMode, model string) (port int, hostname string, srv *sshsrv.Server) {
	t.Helper()
	return cienaModelServerAuth(t, authMode, model, "admin")
}

// cienaModelServerAuth is cienaModelServer with an explicit accepted password,
// so a complex one can be driven end to end over the wire.
func cienaModelServerAuth(t *testing.T, authMode, model, password string) (port int, hostname string, srv *sshsrv.Server) {
	t.Helper()
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	sshPort := freePort(t)

	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: configsDir, ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 1,
		Seed: 7, Distribution: model + ":100",
		Username: "admin", Password: password, EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	hostname = manifestHostname(t, manifest, sshPort)

	var err error
	srv, err = sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: password, EnablePassword: "enable123",
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

// A real 6500 rejects a complex password unless it is wrapped in double quotes,
// which is the form rConfig now always sends. Drive the quoted ACT-USER over a
// real SSH channel and confirm the simulator authenticates it — this is the
// contract the rconfig8 TL1 integration suite depends on.
func TestCiena_QuotedPasswordLogin(t *testing.T) {
	// Contains ":" — the TL1 field separator — and ";", the wire terminator.
	const complexPassword = `Tr@ns!p:rt;#2026`

	port, sid, _ := cienaModelServerAuth(t, "", "ciena-6500-tl1", complexPassword)
	ec := dialExpect(t, port, "admin", complexPassword)
	defer ec.close()

	ec.expect("< ", 3*time.Second)

	ec.reset()
	ec.send(`ACT-USER::admin:100::"` + complexPassword + `";`)
	login := ec.expect("M  100 COMPLD", 3*time.Second)
	if !strings.Contains(login, sid) {
		t.Errorf("quoted-password login COMPLD should carry SID %q: %q", sid, login)
	}

	// The session really is logged in: a post-login verb must not DENY.
	ec.reset()
	ec.send("RTRV-EQPT::ALL:101;")
	ec.expect("M  101 COMPLD", 3*time.Second)
}

// A wrong password sent in the quoted form must still be denied — the quotes are
// framing, not an authentication bypass.
func TestCiena_QuotedPasswordWrongIsDenied(t *testing.T) {
	const complexPassword = `Tr@ns!p:rt;#2026`

	port, _, _ := cienaModelServerAuth(t, "none", "ciena-6500-tl1", complexPassword)
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send(`ACT-USER::admin:100::"WRONG:pass";`)
	got := ec.expect("M  100 DENY", 3*time.Second)
	if !strings.Contains(got, "PLNA") {
		t.Errorf("wrong quoted password should DENY with PLNA: %q", got)
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
// cienaDualHomedFleet generates a fleet of GNEs with dual-homing turned all the way up
// and serves them on consecutive ports, so the same RNE is fronted by several gateways.
func cienaDualHomedFleet(t *testing.T, count int) (portStart int) {
	t.Helper()
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	sshPort := freePort(t)

	if _, err := configs.Run(configs.Config{
		Count: count, OutputDir: configsDir, ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: count,
		Seed: 7, Distribution: "ciena-6500-tl1-gne:100",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		RNEDualHomePct: 100,
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}

	srv, err := sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: count,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		SSHAuthMode:        "none",
		ResponseDelayMinMS: 0, ResponseDelayMaxMS: 0,
		MaxConcurrentSessions: 8,
		MetricsAddr:           fmt.Sprintf("127.0.0.1:%d", freePort(t)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })

	return sshPort
}

// neighbourTIDs logs into one GNE and returns the RNE TIDs it reports.
func neighbourTIDs(t *testing.T, port int) []string {
	t.Helper()
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:1::admin;")
	ec.expect("M  1 COMPLD", 3*time.Second)

	ec.reset()
	ec.send("RTRV-NBR:ALL:2;")
	nbr := ec.expect("M  2 COMPLD", 3*time.Second)

	var tids []string
	for _, m := range regexp.MustCompile(`"(RNE-[A-Z0-9]+):`).FindAllStringSubmatch(nbr, -1) {
		tids = append(tids, m[1])
	}

	return tids
}

// rneInventory retrieves one RNE's equipment inventory through a given gateway and
// returns just the payload lines.
//
// The response header carries a live timestamp, so only the inventory body between the
// COMPLD line and the block terminator can be compared between two sessions.
func rneInventory(t *testing.T, port int, tid string) string {
	t.Helper()
	ec := dialExpectNoAuth(t, port)
	defer ec.close()

	ec.expect("< ", 3*time.Second)
	ec.reset()
	ec.send("ACT-USER::admin:1::admin;")
	ec.expect("M  1 COMPLD", 3*time.Second)

	ec.reset()
	ec.send(fmt.Sprintf("RTRV-EQPT:%s:3;", tid))
	// The inventory streams after the header, so read on to the prompt that follows the
	// whole block rather than stopping at the COMPLD line.
	block := ec.expect("< ", 5*time.Second)

	const compldMarker = "M  3 COMPLD"
	start := strings.Index(block, compldMarker)
	if start < 0 {
		t.Fatalf("no COMPLD in RTRV-EQPT response through port %d: %q", port, block)
	}
	start += len(compldMarker)

	body := block[start:]
	if end := strings.Index(body, "\n;"); end >= 0 {
		body = body[:end]
	}

	return strings.TrimSpace(body)
}

// The point of dual-homing: one RNE, reachable through more than one gateway, reporting
// the same equipment either way. If the two paths disagreed, rConfig would be right to
// treat them as two different devices — so this is the contract the dedup depends on.
func TestCiena_DualHomedRNEIsIdenticalThroughEitherGateway(t *testing.T) {
	const fleet = 8
	portStart := cienaDualHomedFleet(t, fleet)

	// Which gateways front which RNEs.
	frontedBy := map[string][]int{}
	for i := 0; i < fleet; i++ {
		port := portStart + i
		for _, tid := range neighbourTIDs(t, port) {
			frontedBy[tid] = append(frontedBy[tid], port)
		}
	}

	shared, ports := "", []int(nil)
	for tid, p := range frontedBy {
		if len(p) >= 2 {
			shared, ports = tid, p

			break
		}
	}

	if shared == "" {
		t.Fatal("no RNE was reported by two gateways: dual-homing did not happen")
	}

	first := rneInventory(t, ports[0], shared)
	if !strings.Contains(first, "TYPE=6500-7SLOT") {
		t.Fatalf("inventory through gateway %d looks wrong: %q", ports[0], first)
	}

	for _, port := range ports[1:] {
		if got := rneInventory(t, port, shared); got != first {
			t.Errorf("RNE %s reports different equipment through gateway %d than through %d", shared, port, ports[0])
		}
	}
}

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
