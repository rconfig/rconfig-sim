package sshsrv

import (
	"fmt"
	"strings"
)

// Command identifies a resolved Cisco-style command. The input is matched
// to one of these via per-token unique-prefix matching.
//
// Commands have a stable String() form suitable for use as a metrics label
// value. The string matches the Go identifier ("CmdShowRunningConfig") so
// the same set works for both Prometheus labels and log fields. Never label
// a metric with raw user input — that would blow cardinality on the first
// typo.
type Command int

const (
	CmdUnknown Command = iota
	CmdEmpty
	CmdAmbiguous
	CmdTerminalLength
	CmdTerminalPager
	CmdEnable
	CmdShowVersion
	CmdShowRunningConfig
	CmdShowStartupConfig
	CmdShowInventory
	CmdExit
)

// String returns the Go identifier form of the command. Used as a bounded
// Prometheus label value and in structured logs.
func (c Command) String() string {
	switch c {
	case CmdUnknown:
		return "CmdUnknown"
	case CmdEmpty:
		return "CmdEmpty"
	case CmdAmbiguous:
		return "CmdAmbiguous"
	case CmdTerminalLength:
		return "CmdTerminalLength"
	case CmdTerminalPager:
		return "CmdTerminalPager"
	case CmdEnable:
		return "CmdEnable"
	case CmdShowVersion:
		return "CmdShowVersion"
	case CmdShowRunningConfig:
		return "CmdShowRunningConfig"
	case CmdShowStartupConfig:
		return "CmdShowStartupConfig"
	case CmdShowInventory:
		return "CmdShowInventory"
	case CmdExit:
		return "CmdExit"
	default:
		return "CmdUnknown"
	}
}

// cmdSpec is one row of the dispatch table.
type cmdSpec struct {
	canonical string
	tokens    []string
	cmd       Command
}

var cmdTable = func() []cmdSpec {
	entries := []struct {
		canonical string
		cmd       Command
	}{
		{"terminal length 0", CmdTerminalLength},
		{"terminal pager 0", CmdTerminalPager},
		{"enable", CmdEnable},
		{"show version", CmdShowVersion},
		{"show running-config", CmdShowRunningConfig},
		{"show startup-config", CmdShowStartupConfig},
		{"show inventory", CmdShowInventory},
		{"exit", CmdExit},
		{"quit", CmdExit},
		{"logout", CmdExit},
		{"end", CmdExit},
	}
	out := make([]cmdSpec, 0, len(entries))
	for _, e := range entries {
		out = append(out, cmdSpec{
			canonical: e.canonical,
			tokens:    strings.Fields(e.canonical),
			cmd:       e.cmd,
		})
	}
	return out
}()

// ResolveCommand turns a user-typed line into a Command. It handles
// case-insensitive input and Cisco-style per-token unique-prefix matching
// ("sh ver" → "show version"). Exact-canonical matches always win outright —
// this is what lets "end" resolve unambiguously as an exit alias even though
// it shares its first two characters with "enable".
//
// For ambiguous prefixes (e.g. "en" — prefix of both enable and end),
// the resolver returns CmdAmbiguous rather than guessing. Users must type
// the distinguishing third character ("ena…" / "end").
func ResolveCommand(input string) (Command, string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return CmdEmpty, ""
	}
	normalized := strings.ToLower(trimmed)

	// (1) Exact canonical match wins. This is the escape hatch for short
	// single-token commands like "end" that would otherwise collide with
	// abbreviations of longer commands ("en" ⊂ "enable").
	for _, spec := range cmdTable {
		if spec.canonical == normalized {
			return spec.cmd, spec.canonical
		}
	}

	// (2) Per-token unique-prefix match.
	userTokens := strings.Fields(normalized)
	var matches []cmdSpec
	for _, spec := range cmdTable {
		if len(userTokens) > len(spec.tokens) {
			continue
		}
		ok := true
		for i, ut := range userTokens {
			if !strings.HasPrefix(spec.tokens[i], ut) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, spec)
		}
	}

	switch len(matches) {
	case 0:
		return CmdUnknown, ""
	case 1:
		if len(userTokens) < len(matches[0].tokens) {
			// Partial match: user typed fewer tokens than the canonical command.
			// Treat as unknown rather than "did you mean X".
			return CmdUnknown, ""
		}
		return matches[0].cmd, matches[0].canonical
	default:
		// Collapse duplicates that map to the same Command (exit/quit/logout/end
		// all resolve to CmdExit). A single resulting Command is not ambiguous
		// from the dispatcher's point of view, even if the canonical strings differ.
		seen := map[Command]struct{}{}
		for _, m := range matches {
			seen[m.cmd] = struct{}{}
		}
		if len(seen) == 1 {
			return matches[0].cmd, matches[0].canonical
		}
		return CmdAmbiguous, ""
	}
}

// Response describes what a session should emit after a command. The session
// writes Output verbatim, then handles ConfigOutput (mmap'd config bytes) if
// set, then acts on RequestEnablePassword / Close.
type Response struct {
	Output                []byte
	ConfigOutput          []byte // zero-copy mmap slice; nil if not applicable
	RequestEnablePassword bool
	Close                 bool
	ExitEnable            bool
}

const unknownCmdMsg = "% Invalid input detected at '^' marker.\r\n\r\n"
const ambiguousCmdMsg = "% Ambiguous command:  \"\"\r\n"

// Dispatch turns a resolved Command + session state into a Response. Keeps
// no state of its own — all mutation lives on the session.
func Dispatch(cmd Command, canonical string, s *State) Response {
	switch cmd {
	case CmdEmpty:
		return Response{}

	case CmdUnknown:
		return Response{Output: []byte(unknownCmdMsg)}

	case CmdAmbiguous:
		return Response{Output: []byte(ambiguousCmdMsg)}

	case CmdTerminalLength, CmdTerminalPager:
		// Silent success — just next prompt.
		return Response{}

	case CmdEnable:
		if s.EnableMode {
			// Already in enable mode; no-op.
			return Response{}
		}
		return Response{RequestEnablePassword: true}

	case CmdShowVersion:
		return Response{Output: []byte(showVersionFor(s.Hostname, s.Serial))}

	case CmdShowRunningConfig, CmdShowStartupConfig:
		return Response{ConfigOutput: s.ConfigBytes}

	case CmdShowInventory:
		return Response{Output: []byte(showInventoryFor(s.Hostname, s.Serial))}

	case CmdExit:
		if s.EnableMode {
			return Response{ExitEnable: true}
		}
		return Response{Close: true}

	default:
		return Response{Output: []byte(unknownCmdMsg)}
	}
}

// State is the minimum session context the dispatcher needs to build a response.
// It is populated by the session goroutine before each Dispatch call.
type State struct {
	Hostname    string
	Serial      string
	EnableMode  bool
	ConfigBytes []byte // mmap view owned by the loader; not a copy
}

// showVersionFor renders a canned Cisco IOS "show version" response with the
// device's hostname and serial number substituted. Output format matches the
// IOS 15.x style rConfig's stock template expects.
func showVersionFor(hostname, serial string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cisco IOS Software, C2900 Software (C2900-UNIVERSALK9-M), Version 15.5(3)M7, RELEASE SOFTWARE (fc2)\r\n")
	b.WriteString("Technical Support: http://www.cisco.com/techsupport\r\n")
	b.WriteString("Copyright (c) 1986-2018 by Cisco Systems, Inc.\r\n")
	b.WriteString("Compiled Wed 14-Mar-18 16:23 by prod_rel_team\r\n")
	b.WriteString("\r\n")
	b.WriteString("ROM: System Bootstrap, Version 15.0(1r)M16, RELEASE SOFTWARE (fc1)\r\n")
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "%s uptime is 12 weeks, 3 days, 11 hours, 29 minutes\r\n", hostname)
	b.WriteString("System returned to ROM by power-on\r\n")
	b.WriteString("System restarted at 11:42:13 UTC Mon Mar 10 2025\r\n")
	b.WriteString("System image file is \"flash0:c2900-universalk9-mz.SPA.155-3.M7.bin\"\r\n")
	b.WriteString("Last reload type: Normal Reload\r\n")
	b.WriteString("Last reload reason: power-on\r\n")
	b.WriteString("\r\n")
	b.WriteString("cisco CISCO2901/K9 (revision 1.0) with 491520K/32768K bytes of memory.\r\n")
	fmt.Fprintf(&b, "Processor board ID %s\r\n", serial)
	b.WriteString("2 Gigabit Ethernet interfaces\r\n")
	b.WriteString("DRAM configuration is 64 bits wide with parity enabled.\r\n")
	b.WriteString("255K bytes of non-volatile configuration memory.\r\n")
	b.WriteString("250880K bytes of ATA System CompactFlash 0 (Read/Write)\r\n")
	b.WriteString("\r\n")
	b.WriteString("License Info:\r\n")
	b.WriteString("License UDI:\r\n")
	b.WriteString("-------------------------------------------------\r\n")
	b.WriteString("Device#   PID                   SN\r\n")
	fmt.Fprintf(&b, "*0        CISCO2901/K9          %s\r\n", serial)
	b.WriteString("\r\n")
	b.WriteString("Technology Package License Information for Module:'c2900'\r\n")
	b.WriteString("\r\n")
	b.WriteString("-----------------------------------------------------------------\r\n")
	b.WriteString("Technology    Technology-package           Technology-package\r\n")
	b.WriteString("              Current       Type           Next reboot\r\n")
	b.WriteString("------------------------------------------------------------------\r\n")
	b.WriteString("ipbase        ipbasek9      Permanent      ipbasek9\r\n")
	b.WriteString("security      None          None           None\r\n")
	b.WriteString("uc            None          None           None\r\n")
	b.WriteString("data          None          None           None\r\n")
	b.WriteString("\r\n")
	b.WriteString("Configuration register is 0x2102\r\n")
	b.WriteString("\r\n")
	return b.String()
}

// showInventoryFor renders a canned Cisco IOS "show inventory" response.
// The chassis SN field MUST be byte-equal to the "Processor board ID" value
// in showVersionFor so rConfig and any correlation tooling see a consistent
// serial across the two views. All other SNs are derived from the same root
// but suffixed so subcomponents don't duplicate the chassis SN verbatim.
func showInventoryFor(hostname, serial string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "NAME: \"CISCO2901/K9\", DESCR: \"Cisco 2901 Integrated Services Router, %s\"\r\n", hostname)
	fmt.Fprintf(&b, "PID: CISCO2901/K9      , VID: V07 , SN: %s\r\n", serial)
	b.WriteString("\r\n")
	b.WriteString("NAME: \"C2901 Mother board 2FE, integrated VPN and 4W\", DESCR: \"C2901 Motherboard with 2 GE and integrated VPN\"\r\n")
	fmt.Fprintf(&b, "PID: C2901-MB          , VID: V06 , SN: %s-MB\r\n", serial)
	b.WriteString("\r\n")
	b.WriteString("NAME: \"Power Supply Module 0\", DESCR: \"Cisco 2901 AC Power Supply\"\r\n")
	fmt.Fprintf(&b, "PID: PWR-2901-AC       , VID: V01 , SN: %s-PS\r\n", serial)
	b.WriteString("\r\n")
	b.WriteString("NAME: \"Gi0/0/0\", DESCR: \"Integrated Gigabit Ethernet Interface\"\r\n")
	b.WriteString("PID: CISCO2901/K9      , VID: V01 , SN:\r\n")
	b.WriteString("\r\n")
	b.WriteString("NAME: \"Gi0/0/1\", DESCR: \"Integrated Gigabit Ethernet Interface\"\r\n")
	b.WriteString("PID: CISCO2901/K9      , VID: V01 , SN:\r\n")
	b.WriteString("\r\n")
	return b.String()
}
