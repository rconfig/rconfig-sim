package sshsrv

import (
	"io"
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
	cases := []struct {
		name     string
		raw      string
		wantUser string
		wantPass string
	}{
		// The bare form older rConfig builds send — must keep working.
		{"bare password", "ACT-USER::admin:100::s3cret", "admin", "s3cret"},
		// The TL1 (GR-831) quoted form a real 6500 requires for a complex password.
		{"quoted password", `ACT-USER::admin:100::"s3cret"`, "admin", "s3cret"},
		// ":" is the TL1 field separator; the quotes are what keep it in the password.
		{"quoted password containing a colon", `ACT-USER::admin:100::"Tr@ns!p:rt#2026"`, "admin", "Tr@ns!p:rt#2026"},
		// Only the outermost quote pair is stripped, so an embedded quote survives.
		{"quoted password containing a quote", `ACT-USER::admin:100::"pa"ss"`, "admin", `pa"ss`},
		// Quotes protect significant spaces that the whitespace trim would eat.
		{"quoted password with a trailing space", `ACT-USER::admin:100::"pass "`, "admin", "pass "},
		// A quoted username is accepted too, though rConfig does not send one.
		{"quoted username", `ACT-USER::"admin":100::s3cret`, "admin", "s3cret"},
		// A lone quote is not a matching pair and must be left alone.
		{"unbalanced quote", `ACT-USER::admin:100::"oops`, "admin", `"oops`},
		{"empty password", "ACT-USER::admin:100::", "admin", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, pass := parseActUser(c.raw)
			if user != c.wantUser || pass != c.wantPass {
				t.Errorf("parseActUser(%q) = (%q,%q), want (%q,%q)",
					c.raw, user, pass, c.wantUser, c.wantPass)
			}
		})
	}
}

func TestUnquoteTL1(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"quoted"`, "quoted"},
		{"bare", "bare"},
		{`  "padded"  `, "padded"},
		{`"with spaces inside "`, "with spaces inside "},
		{`"`, `"`},       // a single quote is not a pair
		{`""`, ""},       // an empty quoted string
		{`"a"b"`, `a"b`}, // only the outermost pair is stripped
		{`no"quotes"here`, `no"quotes"here`},
		{"", ""},
	}
	for _, c := range cases {
		if got := unquoteTL1(c.in); got != c.want {
			t.Errorf("unquoteTL1(%q) = %q, want %q", c.in, got, c.want)
		}
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

// A complex password is what a real 6500 requires quoting, and what rConfig now
// always sends quoted. Logging in must work through the full dispatch path, and a
// wrong complex password must still be denied.
func TestTL1ComplexPasswordCredentials(t *testing.T) {
	const complexPassword = "Tr@ns!p:rt#2026"

	ctx := tl1Ctx("admin", complexPassword)
	s := newTL1Session(ctx)
	cmd, _ := ctx.dispatchTL1(`ACT-USER::admin:100::"`+complexPassword+`"`, s)
	if cmd != CmdTL1ActUser || !s.loggedIn {
		t.Errorf("quoted complex password: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, s.loggedIn)
	}

	// The same password sent bare is still accepted: the password is the last
	// ACT-USER parameter, so re-joining the tail recovers it even without quotes.
	// Real hardware needs the quotes; the simulator is deliberately liberal so an
	// older rConfig build keeps working against a newer simulator.
	sBare := newTL1Session(ctx)
	cmd, _ = ctx.dispatchTL1("ACT-USER::admin:100::"+complexPassword, sBare)
	if cmd != CmdTL1ActUser || !sBare.loggedIn {
		t.Errorf("bare complex password: cmd=%v loggedIn=%v, want CmdTL1ActUser/true", cmd, sBare.loggedIn)
	}

	// A wrong password is still denied when quoted — the quotes are framing, not a bypass.
	sWrong := newTL1Session(ctx)
	cmd, _ = ctx.dispatchTL1(`ACT-USER::admin:100::"WRONG:pass"`, sWrong)
	if cmd != CmdTL1Deny || sWrong.loggedIn {
		t.Errorf("wrong quoted password: cmd=%v loggedIn=%v, want CmdTL1Deny/false", cmd, sWrong.loggedIn)
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

// scriptedRW feeds readTL1 a fixed byte script one byte at a time (as a real
// channel does) and discards the echo.
type scriptedRW struct {
	in  []byte
	pos int
}

func (r *scriptedRW) Read(p []byte) (int, error) {
	if r.pos >= len(r.in) {
		return 0, io.EOF
	}
	p[0] = r.in[r.pos]
	r.pos++
	return 1, nil
}

func (r *scriptedRW) Write(p []byte) (int, error) { return len(p), nil }

// readTL1 terminates on ";", but inside a TL1 quoted string a ";" is ordinary
// data — so a quoted password containing one must be read whole.
func TestReadTL1QuoteAwareTerminator(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain command", "RTRV-EQPT::ALL:100;", "RTRV-EQPT::ALL:100"},
		{"quoted password", `ACT-USER::admin:100::"s3cret";`, `ACT-USER::admin:100::"s3cret"`},
		{
			"semicolon inside quotes",
			`ACT-USER::admin:100::"pa;ss";`,
			`ACT-USER::admin:100::"pa;ss"`,
		},
		{
			"multiple semicolons inside quotes",
			`ACT-USER::admin:100::"a;b;c";`,
			`ACT-USER::admin:100::"a;b;c"`,
		},
		// Once the quoted string closes, the next ";" terminates as normal.
		{
			"terminator after a closed quote",
			`ACT-USER::admin:100::"pw";RTRV-EQPT`,
			`ACT-USER::admin:100::"pw"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := readTL1(&scriptedRW{in: []byte(c.in)})
			if err != nil {
				t.Fatalf("readTL1(%q) returned error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("readTL1(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Backspacing over a quote character must restore the previous quote state,
// otherwise the terminator search stays stuck inside a string that was erased.
func TestReadTL1BackspaceOverQuote(t *testing.T) {
	// Type a quote, erase it with DEL, then send a normal command. Without the
	// state restore the erased quote would leave inQuote set and the trailing
	// ";" would never terminate.
	script := "\"\x7fRTRV-EQPT::ALL:100;"
	got, err := readTL1(&scriptedRW{in: []byte(script)})
	if err != nil {
		t.Fatalf("readTL1 returned error: %v", err)
	}
	if want := "RTRV-EQPT::ALL:100"; got != want {
		t.Errorf("readTL1 = %q, want %q", got, want)
	}
}
