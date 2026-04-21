package sshsrv

import (
	"regexp"
	"testing"
)

func TestResolveCommand(t *testing.T) {
	cases := []struct {
		in   string
		want Command
	}{
		{"", CmdEmpty},
		{"   ", CmdEmpty},

		// Canonical / case / abbreviation for each full command.
		{"show version", CmdShowVersion},
		{"SHOW VERSION", CmdShowVersion},
		{"sh ver", CmdShowVersion},
		{"sh v", CmdShowVersion},
		{"show running-config", CmdShowRunningConfig},
		{"sh run", CmdShowRunningConfig},
		{"show startup-config", CmdShowStartupConfig},
		{"sh start", CmdShowStartupConfig},
		{"sh s", CmdShowStartupConfig},
		{"show inventory", CmdShowInventory},
		{"sh inv", CmdShowInventory},
		{"sh i", CmdShowInventory},
		{"terminal length 0", CmdTerminalLength},
		{"term len 0", CmdTerminalLength},
		{"terminal pager 0", CmdTerminalPager},
		{"term pager 0", CmdTerminalPager},
		{"ter pa 0", CmdTerminalPager},
		{"enable", CmdEnable},
		{"enabl", CmdEnable},
		{"ena", CmdEnable},

		// Exit family — all four canonicals must resolve via exact match.
		{"exit", CmdExit},
		{"quit", CmdExit},
		{"logout", CmdExit},
		{"end", CmdExit},
		{"EXIT", CmdExit},
		{"Quit", CmdExit},

		// The "en" prefix is ambiguous (enable, end) and must be flagged so
		// — there is no heuristic that silently picks enable over end.
		{"en", CmdAmbiguous},

		// Unknown.
		{"configure terminal", CmdUnknown},
		{"write mem", CmdUnknown},

		// "show" alone now has four candidates (version, running-config,
		// startup-config, inventory). They map to four different Commands → ambiguous.
		{"show", CmdAmbiguous},
		{"sh", CmdAmbiguous},

		// Extra junk tokens beyond canonical length.
		{"show version extra", CmdUnknown},
		{"exit now", CmdUnknown},
	}
	for _, tc := range cases {
		got, _ := ResolveCommand(tc.in)
		if got != tc.want {
			t.Errorf("ResolveCommand(%q): want %v, got %v", tc.in, tc.want, got)
		}
	}
}

func TestDispatchUnknown(t *testing.T) {
	resp := Dispatch(CmdUnknown, "", &State{})
	if string(resp.Output) != unknownCmdMsg {
		t.Errorf("unknown: unexpected output %q", resp.Output)
	}
}

func TestDispatchAmbiguous(t *testing.T) {
	resp := Dispatch(CmdAmbiguous, "", &State{})
	if string(resp.Output) != ambiguousCmdMsg {
		t.Errorf("ambiguous: unexpected output %q", resp.Output)
	}
}

func TestDispatchTerminalCommandsSilent(t *testing.T) {
	for _, c := range []Command{CmdTerminalLength, CmdTerminalPager} {
		resp := Dispatch(c, "", &State{})
		if resp.Output != nil || resp.ConfigOutput != nil || resp.Close {
			t.Errorf("terminal command %v should produce empty response, got %+v", c, resp)
		}
	}
}

func TestDispatchShowRunningReturnsConfigBytes(t *testing.T) {
	cfg := []byte("hostname r1\n!\nend\n")
	resp := Dispatch(CmdShowRunningConfig, "show running-config", &State{ConfigBytes: cfg})
	if &resp.ConfigOutput[0] != &cfg[0] {
		t.Errorf("show running-config must return the same backing []byte (zero-copy requirement)")
	}
}

func TestDispatchShowStartupIsAliasForRunning(t *testing.T) {
	cfg := []byte("hostname r1\n!\nend\n")
	r1 := Dispatch(CmdShowRunningConfig, "", &State{ConfigBytes: cfg})
	r2 := Dispatch(CmdShowStartupConfig, "", &State{ConfigBytes: cfg})
	if &r1.ConfigOutput[0] != &r2.ConfigOutput[0] {
		t.Errorf("startup-config must return the same mmap backing slice as running-config")
	}
}

func TestDispatchExitFamilyBehavior(t *testing.T) {
	// At user-exec (`>`): exit/quit/logout/end all close the session.
	for _, canonical := range []string{"exit", "quit", "logout", "end"} {
		cmd, _ := ResolveCommand(canonical)
		resp := Dispatch(cmd, canonical, &State{EnableMode: false})
		if !resp.Close {
			t.Errorf("%q at user-exec should close session, got %+v", canonical, resp)
		}
		if resp.ExitEnable {
			t.Errorf("%q at user-exec must not set ExitEnable", canonical)
		}
	}

	// In enable mode (`#`): exit/quit/logout/end all drop to user-exec.
	for _, canonical := range []string{"exit", "quit", "logout", "end"} {
		cmd, _ := ResolveCommand(canonical)
		resp := Dispatch(cmd, canonical, &State{EnableMode: true})
		if !resp.ExitEnable {
			t.Errorf("%q in enable mode should set ExitEnable, got %+v", canonical, resp)
		}
		if resp.Close {
			t.Errorf("%q in enable mode must not close the session", canonical)
		}
	}
}

func TestDispatchEnableRequestsPassword(t *testing.T) {
	resp := Dispatch(CmdEnable, "enable", &State{EnableMode: false})
	if !resp.RequestEnablePassword {
		t.Error("enable should request password when not in enable mode")
	}

	resp = Dispatch(CmdEnable, "enable", &State{EnableMode: true})
	if resp.RequestEnablePassword {
		t.Error("enable should not request password when already in enable mode")
	}
}

func TestShowVersionHostnameSubstitution(t *testing.T) {
	out := showVersionFor("rtr-lax-01", "FOCABCDEFGH")
	if !containsAll(out, "rtr-lax-01", "FOCABCDEFGH", "Cisco IOS Software", "Configuration register") {
		t.Error("show version missing expected substrings")
	}
}

func TestShowInventoryContainsSerial(t *testing.T) {
	out := showInventoryFor("rtr-lax-01", "FOCABCDEFGH")
	if !containsAll(out, "rtr-lax-01", "FOCABCDEFGH", "CISCO2901/K9", "Power Supply Module 0") {
		t.Error("show inventory missing expected substrings")
	}
}

// TestShowVersionAndInventoryShareSerial is the load-bearing invariant: any
// operator cross-checking these two commands for a single device must see
// the same serial in both. Regressions here would manifest as phantom
// "inventory mismatch" alerts in rConfig-like collectors.
func TestShowVersionAndInventoryShareSerial(t *testing.T) {
	const hostname = "rtr-lax-edge-1234"
	const serial = "FOCDEADBEEF"
	st := &State{Hostname: hostname, Serial: serial}

	vResp := Dispatch(CmdShowVersion, "show version", st)
	iResp := Dispatch(CmdShowInventory, "show inventory", st)

	vSerial := extractSerial(regexp.MustCompile(`Processor board ID\s+(\S+)`), string(vResp.Output))
	iSerial := extractSerial(regexp.MustCompile(`PID: CISCO2901/K9\s+, VID: V07 , SN:\s*(\S+)`), string(iResp.Output))

	if vSerial == "" {
		t.Fatal("could not extract serial from show version output")
	}
	if iSerial == "" {
		t.Fatal("could not extract serial from show inventory output")
	}
	if vSerial != iSerial {
		t.Errorf("serial mismatch: show version=%q show inventory=%q", vSerial, iSerial)
	}
	if vSerial != serial {
		t.Errorf("show version serial %q != supplied %q", vSerial, serial)
	}
}

func extractSerial(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !stringContains(haystack, n) {
			return false
		}
	}
	return true
}

func stringContains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
