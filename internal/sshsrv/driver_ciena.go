package sshsrv

import (
	"fmt"
	"strings"
)

func init() { registerDriver(cienaTL1{}) }

// cienaTL1 is the Ciena 6500 optical personality: a TL1 management interface
// reached over SSH. After connect the device emits a bare "<" prompt and waits
// for an in-band ACT-USER login; only after a valid login do RTRV-* verbs
// return COMPLD blocks (otherwise DENY). Commands are terminated by ";" and may
// span multiple physical lines.
//
// Self-contained on purpose: adding a vendor should not require touching any
// other file in the hot path. The only shared machinery it borrows is
// (*sessionCtx).applyResponseDelay / emit (fault injection, metrics,
// byte-counting) and the writeAndCount/readLine-style primitives.

type cienaTL1 struct{}

// cienaRNEMarker delimits each remote NE's inventory inside a GNE's config file. This is an
// rcfg-sim file convention, not a Ciena wire convention, so each vendor names its own.
var cienaRNEMarker = []byte(";;RNE ")

func (cienaTL1) Name() string { return "ciena_tl1" }

// RequiresSSHAuth: Ciena TL1 authenticates in-band via ACT-USER, so the SSH
// transport can accept the connection without a password challenge.

func (cienaTL1) RequiresSSHAuth() bool { return false }

func (cienaTL1) Commands() []string {
	return []string{
		CmdTL1Unknown.String(), CmdTL1Deny.String(), CmdTL1ActUser.String(),
		CmdTL1RtrvEqpt.String(), CmdTL1RtrvAlmAll.String(), CmdTL1RtrvCondAll.String(),
		CmdTL1RtrvActiveUser.String(), CmdTL1RtrvSwVer.String(), CmdTL1RtrvSys.String(),
		CmdTL1RtrvNeList.String(),
	}
}

// cienaSession is the per-channel TL1 state. Driver-local — kept off the Cisco
// State struct so vendor concepts stay disjoint.
//
// A device may be a Gateway NE (GNE) fronting Remote NEs (RNEs) reachable only
// through it. The GNE's own EQPT inventory and each RNE's are carved out of the
// mmap'd config as zero-copy sub-slices at session start; commands addressed to
// an RNE TID stream that RNE's slice.

type cienaSession struct {
	loggedIn  bool
	sid       string            // GNE system identifier / TID, shown in local response headers
	serial    string            // GNE shelf serial, substituted into synthesized payloads
	localEQPT []byte            // GNE-own RTRV-EQPT inventory (sub-slice of dev.Data)
	rneEQPT   map[string][]byte // RNE TID (upper-case) -> its RTRV-EQPT inventory (sub-slice)
	rneOrder  []string          // RNE TIDs in file order, for RTRV-NE-LIST
}

func (cienaTL1) Serve(ctx *sessionCtx) {
	s := &cienaSession{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
	if len(ctx.dev.Data) > 0 {
		s.localEQPT, s.rneEQPT, s.rneOrder = indexSections(ctx.dev.Data, cienaRNEMarker)
	}

	// The "<" prompt is both greeting and per-command prompt on a 6500.
	tl1Serve(ctx, "\r\n< ", func(raw string) (Command, Response) {
		return ctx.dispatchCiena(raw, s)
	})
}

// dispatchCiena parses one raw TL1 command and produces a Response. The login
// gate is enforced here: anything other than ACT-USER before a successful login
// returns DENY. Commands carrying an RNE TID are routed to that RNE's data; the
// response header SID becomes the RNE's TID, mirroring a real GNE forwarding the
// reply on the RNE's behalf.

func (ctx *sessionCtx) dispatchCiena(raw string, s *cienaSession) (Command, Response) {
	verb, tid, ctag := parseTL1(raw)
	if verb == "" {
		// Bare ";" or whitespace — silently re-prompt.
		return CmdTL1Unknown, Response{}
	}

	if verb == "ACT-USER" {
		user, pass := parseActUser(raw)
		if ctx.validTL1Login(user, pass) {
			s.loggedIn = true
			return CmdTL1ActUser, Response{Output: tl1Compld(s.sid, ctag, actUserPayload(user))}
		}
		// PLNA = login failure (privileged login not allowed / bad credentials).
		return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "PLNA")}
	}

	if !s.loggedIn {
		return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "PLNA")}
	}

	// Neighbour discovery is always a GNE-local op: list the RNEs reachable through this GNE.
	if verb == "RTRV-NE-LIST" || verb == "RTRV-NODES" {
		return CmdTL1RtrvNeList, Response{Output: tl1Compld(s.sid, ctag, neListPayload(s))}
	}

	// Resolve the target NE. Local (empty/ALL/own-SID) responds as the GNE; an
	// RNE TID routes to that RNE's data and responds AS the RNE.
	respSID := s.sid
	rneEQPT := s.localEQPT
	if !tidIsLocal(tid, s.sid) {
		body, ok := s.rneEQPT[strings.ToUpper(tid)]
		if !ok {
			// IIAC = input, invalid access identifier (unknown/unreachable TID).
			return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "IIAC")}
		}
		respSID, rneEQPT = strings.ToUpper(tid), body
	}

	switch verb {
	case "RTRV-EQPT":
		// Stream the generated inventory zero-copy if present; otherwise
		// synthesize a small canned block so the driver works without a
		// generated payload (unit tests, hand-rolled manifests).
		if len(rneEQPT) > 0 {
			return CmdTL1RtrvEqpt, Response{
				Output:       tl1CompldHeader(respSID, ctag),
				ConfigOutput: rneEQPT,
				Trailer:      []byte(";\r\n"),
			}
		}
		return CmdTL1RtrvEqpt, Response{Output: tl1Compld(respSID, ctag, eqptPayload(s))}
	case "RTRV-ALM-ALL":
		return CmdTL1RtrvAlmAll, Response{Output: tl1Compld(respSID, ctag, almPayload(respSID))}
	case "RTRV-COND-ALL":
		return CmdTL1RtrvCondAll, Response{Output: tl1Compld(respSID, ctag, condPayload(respSID))}
	case "RTRV-ACTIVE-USER":
		return CmdTL1RtrvActiveUser, Response{Output: tl1Compld(respSID, ctag, activeUserPayload(ctx.username))}
	case "RTRV-SW-VER":
		return CmdTL1RtrvSwVer, Response{Output: tl1Compld(respSID, ctag, swVerPayload(s))}
	case "RTRV-SYS":
		return CmdTL1RtrvSys, Response{Output: tl1Compld(respSID, ctag, sysPayload(respSID, s.serial))}
	default:
		// ICNV = input, command not valid.
		return CmdTL1Unknown, Response{Output: tl1Deny(respSID, ctag, "ICNV")}
	}
}

// tidIsLocal reports whether a TID addresses the GNE itself rather than an RNE.
// Empty, "ALL", or the GNE's own SID (case-insensitive) all mean local.

func actUserPayload(user string) string {
	var b strings.Builder
	b.WriteString("   /*AUTHTYPE=LOCAL*/\r\n")
	fmt.Fprintf(&b, "   /*USERID=%s*/\r\n", strings.ToUpper(user))
	return b.String()
}

func eqptPayload(s *cienaSession) string {
	var b strings.Builder
	fmt.Fprintf(&b, "   \"SHELF-1::PROVISIONED,SN=%s,TYPE=6500-7SLOT:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-1:OTR2,%s-01:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-2:WL3N,%s-02:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-7:EDFA,%s-07:IS-NR\"\r\n", s.serial)
	return b.String()
}

func almPayload(sid string) string {
	var b strings.Builder
	b.WriteString("   \"SLOT-2:MN,CONTBUS,SA,,,,:\\\"Intermittent equipment communication\\\"\"\r\n")
	b.WriteString("   \"SLOT-7:MJ,T-LOS,NSA,,,,:\\\"Loss of signal\\\"\"\r\n")
	return b.String()
}

func condPayload(sid string) string {
	var b strings.Builder
	b.WriteString("   \"SLOT-1:T-OPR-OCH,NEND,,,,,:\\\"Optical power received\\\"\"\r\n")
	return b.String()
}

// neListPayload answers RTRV-NE-LIST: the remote NEs reachable through this GNE, one quoted
// record each, in the field order a 6500 emits.
// Empty for a standalone node / legacy single-NE config — a valid empty COMPLD.

func neListPayload(s *cienaSession) string {
	var b strings.Builder
	for i, tid := range s.rneOrder {
		// SID and NENAME carry escaped quotes on the wire, exactly as a 6500 emits them.
		// GNE=NO marks a plain remote NE; the gateway itself is not listed here.
		// COST is the routing metric, which varies per element and is not otherwise
		// meaningful to us - derived from the index so output stays deterministic.
		fmt.Fprintf(&b,
			"   \"SHELF-1::SID=\\\"%s\\\",NENAME=\\\"%s\\\",GNE=NO,GNEIPADDR=,INETADDR=%s,COST=%d,NETYPE=00011600\"\r\n",
			tid, tid, rneAddr(s, i), 30+(i*10)%180)
	}
	return b.String()
}

// rneAddr gives each remote NE a stable management address of its own. A real RTRV-NE-LIST
// reports the element's own INETADDR, not the gateway's, and rConfig now stores it - so the
// simulator has to differ per element or that path is never exercised.
func rneAddr(s *cienaSession, i int) string {
	return fmt.Sprintf("172.28.3.%d", 26+i)
}

func activeUserPayload(user string) string {
	if user == "" {
		user = "ADMIN"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "   \"%s:ADMIN,ACTIVE\"\r\n", strings.ToUpper(user))
	return b.String()
}

func swVerPayload(s *cienaSession) string {
	return "   \"SWVER=12.4,LOAD=12.4-GA,STATUS=ACTIVE\"\r\n"
}

func sysPayload(sid, serial string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "   \"SID=%s,TYPE=6500-7SLOT,SHELFSN=%s\"\r\n", sid, serial)
	return b.String()
}
