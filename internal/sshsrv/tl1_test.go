package sshsrv

import (
	"io"
	"testing"
)

// Tests for the vendor-neutral TL1 machinery in tl1.go: command framing, the
// VERB:TID:AID:CTAG grammar, ACT-USER parsing, quoted strings and TID locality.
//
// Vendor behaviour (verb sets, payload text, neighbour enumeration) is tested in the
// driver_<vendor>_test.go files.

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
	local, rne, order := indexSections(plain, cienaRNEMarker)
	if string(local) != string(plain) || len(rne) != 0 || len(order) != 0 {
		t.Errorf("no-marker: local=%q rne=%v order=%v", local, rne, order)
	}

	// GNE + two RNEs.
	data := []byte("GNE-A\nGNE-B\n;;RNE RNE-CORK\nCORK-1\n;;RNE RNE-GALWAY\nGAL-1\nGAL-2\n")
	local, rne, order = indexSections(data, cienaRNEMarker)
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
