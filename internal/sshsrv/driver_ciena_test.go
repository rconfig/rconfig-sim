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
		wantCtag string
	}{
		{"RTRV-EQPT::ALL:100", "RTRV-EQPT", "100"},
		{"RTRV-ALM-ALL::ALL:101", "RTRV-ALM-ALL", "101"},
		{"RTRV-SW-VER:::100", "RTRV-SW-VER", "100"},
		{"RTRV-SYS:::101", "RTRV-SYS", "101"},
		{"rtrv-eqpt::all:7", "RTRV-EQPT", "7"}, // case-insensitive verb
		{"ACT-USER::admin:CTAG1::secret", "ACT-USER", "CTAG1"},
	}
	for _, c := range cases {
		verb, ctag := parseTL1(c.raw)
		if verb != c.wantVerb || ctag != c.wantCtag {
			t.Errorf("parseTL1(%q) = (%q,%q), want (%q,%q)", c.raw, verb, ctag, c.wantVerb, c.wantCtag)
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
