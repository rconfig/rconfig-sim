package sshsrv

import (
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

func tl1Ctx(user, pass string) *sessionCtx {
	return &sessionCtx{
		dev:      &configs.Device{Hostname: "CIENA-LAB-0001", SerialNumber: "SNTEST123"},
		username: user,
		password: pass,
	}
}

func newTL1Session(ctx *sessionCtx) *cienaSession {
	return &cienaSession{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
}

func TestTL1LoginGate(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)

	// Before login: any RTRV is denied.
	cmd, resp := ctx.dispatchCiena("RTRV-EQPT::ALL:100", s)
	if cmd != CmdTL1Deny {
		t.Fatalf("pre-login RTRV-EQPT: cmd=%v, want CmdTL1Deny", cmd)
	}
	if !strings.Contains(string(resp.Output), "DENY") || !strings.Contains(string(resp.Output), "PLNA") {
		t.Errorf("pre-login DENY block missing DENY/PLNA: %q", resp.Output)
	}
	if s.loggedIn {
		t.Error("session should not be logged in after a denied RTRV")
	}

	// Valid ACT-USER unlocks.
	cmd, resp = ctx.dispatchCiena("ACT-USER::admin:100::admin", s)
	if cmd != CmdTL1ActUser {
		t.Fatalf("ACT-USER: cmd=%v, want CmdTL1ActUser", cmd)
	}
	if !s.loggedIn {
		t.Fatal("session should be logged in after valid ACT-USER")
	}
	if !strings.Contains(string(resp.Output), "COMPLD") {
		t.Errorf("ACT-USER success should be COMPLD: %q", resp.Output)
	}

	// After login: RTRV-EQPT completes.
	cmd, resp = ctx.dispatchCiena("RTRV-EQPT::ALL:101", s)
	if cmd != CmdTL1RtrvEqpt {
		t.Fatalf("post-login RTRV-EQPT: cmd=%v, want CmdTL1RtrvEqpt", cmd)
	}
	if !strings.Contains(string(resp.Output), "COMPLD") {
		t.Errorf("post-login RTRV-EQPT should be COMPLD: %q", resp.Output)
	}
}

func TestTL1Credentials(t *testing.T) {
	// Wrong password is denied, session stays logged out.
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)
	cmd, _ := ctx.dispatchCiena("ACT-USER::admin:100::WRONG", s)
	if cmd != CmdTL1Deny || s.loggedIn {
		t.Errorf("wrong password: cmd=%v loggedIn=%v, want CmdTL1Deny/false", cmd, s.loggedIn)
	}

	// Empty configured password accepts any password (mirrors PasswordCallback).
	ctxAny := tl1Ctx("admin", "")
	sAny := newTL1Session(ctxAny)
	cmd, _ = ctxAny.dispatchCiena("ACT-USER::admin:100::whatever", sAny)
	if cmd != CmdTL1ActUser || !sAny.loggedIn {
		t.Errorf("empty-password accept-any: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, sAny.loggedIn)
	}
}

// A complex password is what a real 6500 requires quoting, and what rConfig now
// always sends quoted. Logging in must work through the full dispatch path, and a
// wrong complex password must still be denied.

func TestTL1ComplexPasswordCredentials(t *testing.T) {
	const complexPassword = "Tr@ns!p:rt#2026"

	ctx := tl1Ctx("admin", complexPassword)
	s := newTL1Session(ctx)
	cmd, _ := ctx.dispatchCiena(`ACT-USER::admin:100::"`+complexPassword+`"`, s)
	if cmd != CmdTL1ActUser || !s.loggedIn {
		t.Errorf("quoted complex password: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, s.loggedIn)
	}

	// The same password sent bare is still accepted: the password is the last
	// ACT-USER parameter, so re-joining the tail recovers it even without quotes.
	// Real hardware needs the quotes; the simulator is deliberately liberal so an
	// older rConfig build keeps working against a newer simulator.
	sBare := newTL1Session(ctx)
	cmd, _ = ctx.dispatchCiena("ACT-USER::admin:100::"+complexPassword, sBare)
	if cmd != CmdTL1ActUser || !sBare.loggedIn {
		t.Errorf("bare complex password: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, sBare.loggedIn)
	}

	// A wrong password is still denied when quoted — the quotes are framing, not a bypass.
	sWrong := newTL1Session(ctx)
	cmd, _ = ctx.dispatchCiena(`ACT-USER::admin:100::"WRONG:pass"`, sWrong)
	if cmd != CmdTL1Deny || sWrong.loggedIn {
		t.Errorf("wrong quoted password: cmd=%v loggedIn=%v, want CmdTL1Deny/false", cmd, sWrong.loggedIn)
	}
}

func TestTL1BlockShape(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)
	_, resp := ctx.dispatchCiena("ACT-USER::admin:CTAG7::admin", s)
	out := string(resp.Output)

	if !strings.HasPrefix(out, "\r\n") {
		t.Errorf("COMPLD block should start with a blank line: %q", out)
	}
	if !strings.Contains(out, "CIENA-LAB-0001") {
		t.Errorf("COMPLD block should contain the SID: %q", out)
	}
	if !strings.Contains(out, "M  CTAG7 COMPLD") {
		t.Errorf("COMPLD block should echo the CTAG in the response code line: %q", out)
	}
	if !strings.HasSuffix(out, ";\r\n") {
		t.Errorf("COMPLD block should be terminated by ';': %q", out)
	}
}

func TestRequireSSHAuth(t *testing.T) {
	cisco := &configs.Device{Driver: "cisco_ios"}
	ciena := &configs.Device{Driver: "ciena_tl1"}
	cases := []struct {
		mode string
		dev  *configs.Device
		want bool
	}{
		{"", cisco, true}, {"", ciena, true}, // empty == password (back-compat)
		{"password", cisco, true}, {"password", ciena, true},
		{"driver", cisco, true}, {"driver", ciena, false}, // per-driver
		{"none", cisco, false}, {"none", ciena, false},
	}
	for _, c := range cases {
		s := &Server{cfg: Config{SSHAuthMode: c.mode}}
		if got := s.requireSSHAuth(c.dev); got != c.want {
			t.Errorf("mode=%q driver=%q: requireSSHAuth=%v, want %v", c.mode, c.dev.Driver, got, c.want)
		}
	}
}

func gneSession(t *testing.T) (*sessionCtx, *cienaSession) {
	t.Helper()
	ctx := tl1Ctx("admin", "admin")
	data := []byte("   \"SHELF-1::GNE\"\n;;RNE RNE-CORK\n   \"SHELF-1::CORK\"\n;;RNE RNE-GALWAY\n   \"SHELF-1::GAL\"\n")
	ctx.dev.Data = data
	s := newTL1Session(ctx)
	s.localEQPT, s.rneEQPT, s.rneOrder = indexSections(data, cienaRNEMarker)
	s.loggedIn = true
	return ctx, s
}

func TestTL1GNERouting(t *testing.T) {
	ctx, s := gneSession(t)

	// Local RTRV-EQPT streams the GNE section; header SID is the GNE.
	cmd, resp := ctx.dispatchCiena("RTRV-EQPT::ALL:100", s)
	if cmd != CmdTL1RtrvEqpt || string(resp.ConfigOutput) != "   \"SHELF-1::GNE\"\n" {
		t.Errorf("local EQPT: cmd=%v body=%q", cmd, resp.ConfigOutput)
	}
	if !strings.Contains(string(resp.Output), "CIENA-LAB-0001") {
		t.Errorf("local EQPT header should carry GNE SID: %q", resp.Output)
	}

	// RTRV-EQPT to an RNE streams that RNE's section; header SID is the RNE TID.
	cmd, resp = ctx.dispatchCiena("RTRV-EQPT:RNE-CORK:3", s)
	if cmd != CmdTL1RtrvEqpt || string(resp.ConfigOutput) != "   \"SHELF-1::CORK\"\n" {
		t.Errorf("RNE EQPT: cmd=%v body=%q", cmd, resp.ConfigOutput)
	}
	if !strings.Contains(string(resp.Output), "M  3 COMPLD") || !strings.Contains(string(resp.Output), "RNE-CORK") {
		t.Errorf("RNE EQPT header should carry RNE TID + ctag: %q", resp.Output)
	}

	// Unknown TID -> DENY IIAC.
	cmd, resp = ctx.dispatchCiena("RTRV-EQPT:RNE-NOPE:9", s)
	if cmd != CmdTL1Deny || !strings.Contains(string(resp.Output), "IIAC") {
		t.Errorf("unknown TID: cmd=%v out=%q, want DENY/IIAC", cmd, resp.Output)
	}

	// RNE-targeted alarm: header SID is the RNE.
	cmd, resp = ctx.dispatchCiena("RTRV-ALM-ALL:RNE-GALWAY:4", s)
	if cmd != CmdTL1RtrvAlmAll || !strings.Contains(string(resp.Output), "RNE-GALWAY") {
		t.Errorf("RNE alarm: cmd=%v out=%q", cmd, resp.Output)
	}
}

func TestTL1RtrvNbr(t *testing.T) {
	ctx, s := gneSession(t)
	cmd, resp := ctx.dispatchCiena("RTRV-NE-LIST:ALL:2", s)
	if cmd != CmdTL1RtrvNeList {
		t.Fatalf("RTRV-NE-LIST: cmd=%v, want CmdTL1RtrvNeList", cmd)
	}
	out := string(resp.Output)
	for _, tid := range []string{"RNE-CORK", "RNE-GALWAY"} {
		if !strings.Contains(out, tid) {
			t.Errorf("RTRV-NE-LIST list missing %q: %q", tid, out)
		}
	}

	// A standalone (no-RNE) session answers RTRV-NE-LIST with an empty COMPLD.
	ctxS := tl1Ctx("admin", "admin")
	sStandalone := newTL1Session(ctxS)
	sStandalone.loggedIn = true
	cmd, resp = ctxS.dispatchCiena("RTRV-NE-LIST:ALL:2", sStandalone)
	if cmd != CmdTL1RtrvNeList || !strings.Contains(string(resp.Output), "M  2 COMPLD") {
		t.Errorf("standalone RTRV-NE-LIST: cmd=%v out=%q", cmd, resp.Output)
	}
}

func TestTL1UnknownVerb(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)
	ctx.dispatchCiena("ACT-USER::admin:1::admin", s) // log in first
	cmd, resp := ctx.dispatchCiena("ENT-CRS-OCH::FOO:9", s)
	if cmd != CmdTL1Unknown {
		t.Errorf("unknown verb: cmd=%v, want CmdTL1Unknown", cmd)
	}
	if !strings.Contains(string(resp.Output), "ICNV") {
		t.Errorf("unknown verb DENY should carry ICNV: %q", resp.Output)
	}
}

// scriptedRW feeds readTL1 a fixed byte script one byte at a time (as a real
// channel does) and discards the echo.
