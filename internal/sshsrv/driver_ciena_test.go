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

func newTL1Session(ctx *sessionCtx) *tl1Session {
	return &tl1Session{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
}

func TestParseTL1(t *testing.T) {
	cases := []struct {
		raw      string
		wantVerb string
		wantTID  string
		wantCtag string
	}{
		// Strict form (VERB::AID:CTAG) — CTAG is field 3.
		{"RTRV-EQPT::ALL:100", "RTRV-EQPT", "", "100"},
		{"RTRV-ALM-ALL::ALL:101", "RTRV-ALM-ALL", "", "101"},
		{"RTRV-SW-VER:::100", "RTRV-SW-VER", "", "100"},
		{"RTRV-SYS:::101", "RTRV-SYS", "", "101"},
		{"rtrv-eqpt::all:7", "RTRV-EQPT", "", "7"}, // case-insensitive verb
		{"ACT-USER::admin:CTAG1::secret", "ACT-USER", "", "CTAG1"},
		// Short form (VERB:TID:CTAG) — CTAG is the last field; TID addresses an RNE.
		{"RTRV-EQPT:RNE-LIMERICK:3", "RTRV-EQPT", "RNE-LIMERICK", "3"},
		{"RTRV-ALM-ALL:RNE-LIMERICK:4", "RTRV-ALM-ALL", "RNE-LIMERICK", "4"},
		{"RTRV-NBR:ALL:2", "RTRV-NBR", "ALL", "2"},
	}
	for _, c := range cases {
		verb, tid, ctag := parseTL1(c.raw)
		if verb != c.wantVerb || tid != c.wantTID || ctag != c.wantCtag {
			t.Errorf("parseTL1(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.raw, verb, tid, ctag, c.wantVerb, c.wantTID, c.wantCtag)
		}
	}
}

func TestParseActUser(t *testing.T) {
	user, pass := parseActUser("ACT-USER::admin:100::s3cret")
	if user != "admin" || pass != "s3cret" {
		t.Errorf("parseActUser = (%q,%q), want (admin,s3cret)", user, pass)
	}
}

func TestTL1LoginGate(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)

	// Before login: any RTRV is denied.
	cmd, resp := ctx.dispatchTL1("RTRV-EQPT::ALL:100", s)
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
	cmd, resp = ctx.dispatchTL1("ACT-USER::admin:100::admin", s)
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
	cmd, resp = ctx.dispatchTL1("RTRV-EQPT::ALL:101", s)
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
	cmd, _ := ctx.dispatchTL1("ACT-USER::admin:100::WRONG", s)
	if cmd != CmdTL1Deny || s.loggedIn {
		t.Errorf("wrong password: cmd=%v loggedIn=%v, want CmdTL1Deny/false", cmd, s.loggedIn)
	}

	// Empty configured password accepts any password (mirrors PasswordCallback).
	ctxAny := tl1Ctx("admin", "")
	sAny := newTL1Session(ctxAny)
	cmd, _ = ctxAny.dispatchTL1("ACT-USER::admin:100::whatever", sAny)
	if cmd != CmdTL1ActUser || !sAny.loggedIn {
		t.Errorf("empty-password accept-any: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, sAny.loggedIn)
	}
}

func TestTL1BlockShape(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)
	_, resp := ctx.dispatchTL1("ACT-USER::admin:CTAG7::admin", s)
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

func TestTidIsLocal(t *testing.T) {
	own := "CIENA-LAB-0001"
	local := []string{"", "ALL", "all", "CIENA-LAB-0001", "ciena-lab-0001"}
	rne := []string{"RNE-CORK", "RNE-LIMERICK", "SOMETHING"}
	for _, tid := range local {
		if !tidIsLocal(tid, own) {
			t.Errorf("tidIsLocal(%q) = false, want true", tid)
		}
	}
	for _, tid := range rne {
		if tidIsLocal(tid, own) {
			t.Errorf("tidIsLocal(%q) = true, want false", tid)
		}
	}
}

func TestIndexSections(t *testing.T) {
	// No marker -> legacy single-NE: whole data is local, no RNEs.
	plain := []byte("   \"SHELF-1::X\"\n   \"SLOT-1:Y\"\n")
	local, rne, order := indexSections(plain)
	if string(local) != string(plain) || len(rne) != 0 || len(order) != 0 {
		t.Errorf("no-marker: local=%q rne=%v order=%v", local, rne, order)
	}

	// GNE + two RNEs.
	data := []byte("GNE-A\nGNE-B\n;;RNE RNE-CORK\nCORK-1\n;;RNE RNE-GALWAY\nGAL-1\nGAL-2\n")
	local, rne, order = indexSections(data)
	if string(local) != "GNE-A\nGNE-B\n" {
		t.Errorf("local section = %q", local)
	}
	if got := []string{"RNE-CORK", "RNE-GALWAY"}; order[0] != got[0] || order[1] != got[1] {
		t.Errorf("order = %v, want %v", order, got)
	}
	if string(rne["RNE-CORK"]) != "CORK-1\n" {
		t.Errorf("RNE-CORK = %q", rne["RNE-CORK"])
	}
	if string(rne["RNE-GALWAY"]) != "GAL-1\nGAL-2\n" {
		t.Errorf("RNE-GALWAY = %q", rne["RNE-GALWAY"])
	}
	// Sections must be zero-copy sub-slices of the input (same backing array).
	if &rne["RNE-CORK"][0] != &data[len("GNE-A\nGNE-B\n;;RNE RNE-CORK\n")] {
		t.Error("RNE-CORK section is not a sub-slice of the input (copy detected)")
	}
}

// gneSession builds a logged-in GNE session over a sectioned config blob.
func gneSession(t *testing.T) (*sessionCtx, *tl1Session) {
	t.Helper()
	ctx := tl1Ctx("admin", "admin")
	data := []byte("   \"SHELF-1::GNE\"\n;;RNE RNE-CORK\n   \"SHELF-1::CORK\"\n;;RNE RNE-GALWAY\n   \"SHELF-1::GAL\"\n")
	ctx.dev.Data = data
	s := newTL1Session(ctx)
	s.localEQPT, s.rneEQPT, s.rneOrder = indexSections(data)
	s.loggedIn = true
	return ctx, s
}

func TestTL1GNERouting(t *testing.T) {
	ctx, s := gneSession(t)

	// Local RTRV-EQPT streams the GNE section; header SID is the GNE.
	cmd, resp := ctx.dispatchTL1("RTRV-EQPT::ALL:100", s)
	if cmd != CmdTL1RtrvEqpt || string(resp.ConfigOutput) != "   \"SHELF-1::GNE\"\n" {
		t.Errorf("local EQPT: cmd=%v body=%q", cmd, resp.ConfigOutput)
	}
	if !strings.Contains(string(resp.Output), "CIENA-LAB-0001") {
		t.Errorf("local EQPT header should carry GNE SID: %q", resp.Output)
	}

	// RTRV-EQPT to an RNE streams that RNE's section; header SID is the RNE TID.
	cmd, resp = ctx.dispatchTL1("RTRV-EQPT:RNE-CORK:3", s)
	if cmd != CmdTL1RtrvEqpt || string(resp.ConfigOutput) != "   \"SHELF-1::CORK\"\n" {
		t.Errorf("RNE EQPT: cmd=%v body=%q", cmd, resp.ConfigOutput)
	}
	if !strings.Contains(string(resp.Output), "M  3 COMPLD") || !strings.Contains(string(resp.Output), "RNE-CORK") {
		t.Errorf("RNE EQPT header should carry RNE TID + ctag: %q", resp.Output)
	}

	// Unknown TID -> DENY IIAC.
	cmd, resp = ctx.dispatchTL1("RTRV-EQPT:RNE-NOPE:9", s)
	if cmd != CmdTL1Deny || !strings.Contains(string(resp.Output), "IIAC") {
		t.Errorf("unknown TID: cmd=%v out=%q, want DENY/IIAC", cmd, resp.Output)
	}

	// RNE-targeted alarm: header SID is the RNE.
	cmd, resp = ctx.dispatchTL1("RTRV-ALM-ALL:RNE-GALWAY:4", s)
	if cmd != CmdTL1RtrvAlmAll || !strings.Contains(string(resp.Output), "RNE-GALWAY") {
		t.Errorf("RNE alarm: cmd=%v out=%q", cmd, resp.Output)
	}
}

func TestTL1RtrvNbr(t *testing.T) {
	ctx, s := gneSession(t)
	cmd, resp := ctx.dispatchTL1("RTRV-NBR:ALL:2", s)
	if cmd != CmdTL1RtrvNbr {
		t.Fatalf("RTRV-NBR: cmd=%v, want CmdTL1RtrvNbr", cmd)
	}
	out := string(resp.Output)
	for _, tid := range []string{"RNE-CORK", "RNE-GALWAY"} {
		if !strings.Contains(out, tid) {
			t.Errorf("RTRV-NBR list missing %q: %q", tid, out)
		}
	}

	// A standalone (no-RNE) session answers RTRV-NBR with an empty COMPLD.
	ctxS := tl1Ctx("admin", "admin")
	sStandalone := newTL1Session(ctxS)
	sStandalone.loggedIn = true
	cmd, resp = ctxS.dispatchTL1("RTRV-NBR:ALL:2", sStandalone)
	if cmd != CmdTL1RtrvNbr || !strings.Contains(string(resp.Output), "M  2 COMPLD") {
		t.Errorf("standalone RTRV-NBR: cmd=%v out=%q", cmd, resp.Output)
	}
}

func TestTL1UnknownVerb(t *testing.T) {
	ctx := tl1Ctx("admin", "admin")
	s := newTL1Session(ctx)
	ctx.dispatchTL1("ACT-USER::admin:1::admin", s) // log in first
	cmd, resp := ctx.dispatchTL1("ENT-CRS-OCH::FOO:9", s)
	if cmd != CmdTL1Unknown {
		t.Errorf("unknown verb: cmd=%v, want CmdTL1Unknown", cmd)
	}
	if !strings.Contains(string(resp.Output), "ICNV") {
		t.Errorf("unknown verb DENY should carry ICNV: %q", resp.Output)
	}
}
