package sshsrv

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

func infineraCtx(user, pass string) *sessionCtx {
	return &sessionCtx{
		dev:      &configs.Device{Hostname: "LXTNKYXAO4Z", SerialNumber: "SNINF0001"},
		username: user,
		password: pass,
	}
}

func ciscoONSCtx(user, pass string) *sessionCtx {
	return &sessionCtx{
		dev:      &configs.Device{Hostname: "TID-000", SerialNumber: "SNONS0001"},
		username: user,
		password: pass,
	}
}

func infineraTestTID(i int) string { return fmt.Sprintf("NODE%02dTNAB1Y", i) }

func ciscoTestTID(i int) string { return fmt.Sprintf("TID-1%02d", i) }

// A DTN-X pages RTRV-TIDMAP: every block but the last is coded RTRV, with the prompt written
// between them. A client that stops reading at the first prompt sees a partial answer and
// leaves the rest on the wire, so the simulator has to reproduce the paging faithfully or that
// failure can never be tested.
func TestInfineraTidmapPages(t *testing.T) {
	ctx := infineraCtx("admin", "admin")
	s := &infineraSession{sid: ctx.dev.Hostname, loggedIn: true}
	for i := 0; i < 23; i++ {
		s.nodeOrder = append(s.nodeOrder, infineraTestTID(i))
	}

	cmd, resp := ctx.dispatchInfinera("RTRV-TIDMAP:::ctag", s)
	if cmd != CmdTL1RtrvTidmap {
		t.Fatalf("cmd = %v, want CmdTL1RtrvTidmap", cmd)
	}

	out := string(resp.Output)

	// 23 records at 10 per page: two continuation blocks then a terminal one.
	if got := strings.Count(out, "ctag RTRV"); got != 2 {
		t.Errorf("continuation blocks = %d, want 2", got)
	}
	if got := strings.Count(out, "ctag COMPLD"); got != 1 {
		t.Errorf("terminal blocks = %d, want 1", got)
	}
	if got := strings.Count(out, infineraPrompt); got != 2 {
		t.Errorf("inter-block prompts = %d, want 2 (one after each continuation)", got)
	}

	// The terminal block must be last, or a client reading to the terminal code would stop
	// early and still miss records.
	if strings.Index(out, "ctag COMPLD") < strings.LastIndex(out, "ctag RTRV") {
		t.Error("COMPLD must be the final block")
	}

	// Every record present exactly once.
	for i := 0; i < 23; i++ {
		if strings.Count(out, "TID="+infineraTestTID(i)+",") != 1 {
			t.Errorf("record %d missing or duplicated", i)
		}
	}
}

// A node with no neighbours still answers, in one terminal block.
func TestInfineraTidmapEmpty(t *testing.T) {
	ctx := infineraCtx("admin", "admin")
	s := &infineraSession{sid: ctx.dev.Hostname, loggedIn: true}

	_, resp := ctx.dispatchInfinera("RTRV-TIDMAP:::ctag", s)
	out := string(resp.Output)

	if strings.Contains(out, "ctag RTRV") {
		t.Error("an empty map should not page")
	}
	if !strings.Contains(out, "ctag COMPLD") {
		t.Errorf("want a terminal COMPLD block, got %q", out)
	}
}

// The record shape is keyword-based with an empty AID, matching the customer's capture.
func TestInfineraTidmapRecordShape(t *testing.T) {
	ctx := infineraCtx("admin", "admin")
	s := &infineraSession{sid: ctx.dev.Hostname, loggedIn: true, nodeOrder: []string{"CSVLTNFCO1Y"}}

	_, resp := ctx.dispatchInfinera("RTRV-TIDMAP:::ctag", s)
	out := string(resp.Output)

	if !strings.Contains(out, `"::TID=CSVLTNFCO1Y,NODEID=`) {
		t.Errorf("record should start with an empty AID then TID=, got %q", out)
	}
	if !strings.Contains(out, ",ROUTERID=") {
		t.Errorf("record should carry ROUTERID, got %q", out)
	}
}

// A DTN-X answers under its own system name even when relaying for another node, so a client
// cannot verify routing by comparing the header SID to the TID it addressed. Pinning this
// stops a well-meaning change from "fixing" it into Ciena's behaviour.
func TestInfineraRoutedResponseKeepsSystemName(t *testing.T) {
	ctx := infineraCtx("admin", "admin")
	s := &infineraSession{
		sid:      ctx.dev.Hostname,
		loggedIn: true,
		nodeEQPT: map[string][]byte{"REMOTE1TNAB1Y": []byte("   \"SLOT-1::OTR4\"\n")},
	}

	_, resp := ctx.dispatchInfinera("RTRV-EQPT:REMOTE1TNAB1Y::ctag", s)
	out := string(resp.Output)

	if !strings.Contains(out, "LXTNKYXAO4Z") {
		t.Errorf("header should carry the system name, got %q", out)
	}
	if strings.Contains(out, "   REMOTE1TNAB1Y ") {
		t.Error("header must not carry the addressed TID: a DTN-X does not echo it")
	}
}

func TestInfineraLoginGate(t *testing.T) {
	ctx := infineraCtx("admin", "admin")
	s := &infineraSession{sid: ctx.dev.Hostname}

	cmd, resp := ctx.dispatchInfinera("RTRV-TIDMAP:::1", s)
	if cmd != CmdTL1Deny || !strings.Contains(string(resp.Output), "PLNA") {
		t.Errorf("pre-login should DENY/PLNA, got %v %q", cmd, resp.Output)
	}

	cmd, _ = ctx.dispatchInfinera("ACT-USER::admin:2::\"admin\"", s)
	if cmd != CmdTL1ActUser || !s.loggedIn {
		t.Errorf("login: cmd=%v loggedIn=%v", cmd, s.loggedIn)
	}
}

// Cisco's records are POSITIONAL, not keyword-based. This is the third distinct payload
// grammar and the reason each vendor parses its own neighbour records.
func TestCiscoONSMapNetworkRecordShape(t *testing.T) {
	ctx := ciscoONSCtx("admin", "admin")
	s := &ciscoONSSession{sid: ctx.dev.Hostname, loggedIn: true, eneOrder: []string{"TID-001", "TID-002"}}

	cmd, resp := ctx.dispatchCiscoONS("RTRV-MAP-NETWORK:::ctag", s)
	if cmd != CmdTL1RtrvMapNetwork {
		t.Fatalf("cmd = %v, want CmdTL1RtrvMapNetwork", cmd)
	}

	out := string(resp.Output)

	if strings.Contains(out, "TID=") {
		t.Error("Cisco records are positional; they must carry no KEY= fields")
	}
	// "<IPADDR>,<NODENAME>,<PRODUCT>" - the gateway lists itself plus its end NEs.
	if !strings.Contains(out, `"172.20.222.225,TID-000,15454"`) {
		t.Errorf("gateway should list itself first, got %q", out)
	}
	for _, tid := range []string{"TID-001", "TID-002"} {
		if !strings.Contains(out, ","+tid+",") {
			t.Errorf("missing end NE %s in %q", tid, out)
		}
	}
}

// The vendor documentation notes PRODUCT comes back as UNKNOWN for nodes running a different
// software version. A client must not store that as a model, so the simulator emits it.
func TestCiscoONSMapNetworkEmitsUnknownProduct(t *testing.T) {
	ctx := ciscoONSCtx("admin", "admin")
	s := &ciscoONSSession{sid: ctx.dev.Hostname, loggedIn: true}
	for i := 0; i < 8; i++ {
		s.eneOrder = append(s.eneOrder, ciscoTestTID(i))
	}

	_, resp := ctx.dispatchCiscoONS("RTRV-MAP-NETWORK:::ctag", s)
	if !strings.Contains(string(resp.Output), ",UNKNOWN\"") {
		t.Errorf("want at least one UNKNOWN product, got %q", resp.Output)
	}
}

// Both new drivers must be reachable by their manifest id, and an unknown id must be reported
// rather than silently resolving to another vendor.
func TestOpticalDriversRegistered(t *testing.T) {
	for _, id := range []string{"infinera_tl1", "cisco_ons_tl1"} {
		if driverFor(id).Name() != id {
			t.Errorf("driverFor(%q) resolved to %q", id, driverFor(id).Name())
		}
	}

	bad := unknownDrivers([]*configs.Device{
		{Driver: "infinera_tl1"},
		{Driver: "typo_tl1"},
		{Driver: ""},
	})
	if len(bad) != 1 || bad[0] != "typo_tl1" {
		t.Errorf("unknownDrivers = %v, want [typo_tl1]", bad)
	}
}
