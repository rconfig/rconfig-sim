package sshsrv

import (
	"fmt"
	"strings"
)

func init() { registerDriver(ciscoONSTL1{}) }

// ciscoONSTL1 is the Cisco ONS 15454 personality: a TL1 management interface reached over SSH.
//
// Its neighbour map comes from RTRV-MAP-NETWORK, and the records are POSITIONAL rather than
// keyword-shaped: "<IPADDR>,<NODENAME>,<PRODUCT>" with no KEY= anywhere. That is a third
// distinct payload grammar alongside Ciena's and Infinera's, and it is why each vendor parses
// its own neighbour records instead of sharing a field-map helper.
//
// Cisco's vocabulary also differs: a gateway is a GNE, but the nodes behind it are ENEs
// (End NEs), not RNEs.
//
// STATUS: this driver is built from vendor documentation, not from a capture of a real node.
// The command and its output format come from the Cisco ONS SONET TL1 Command Guide R9.1
// section 21.68 and Oracle's Cisco ONS 15454 TL1 reference. Documented is not the same as
// observed: treat the payload text as provisional until someone runs it against hardware.
type ciscoONSTL1 struct{}

// ciscoENEMarker delimits each end NE's inventory inside a gateway's config file.
var ciscoENEMarker = []byte(";;ENE ")

func (ciscoONSTL1) Name() string { return "cisco_ons_tl1" }

// RequiresSSHAuth: TL1 authenticates in-band via ACT-USER.
func (ciscoONSTL1) RequiresSSHAuth() bool { return false }

// Commands reuses the shared CmdTL1* labels for shared verbs, adding only RTRV-MAP-NETWORK.
// See the note on infineraTL1.Commands for why labels are per-verb and not per-vendor.
func (ciscoONSTL1) Commands() []string {
	return []string{
		CmdTL1Unknown.String(), CmdTL1Deny.String(), CmdTL1ActUser.String(),
		CmdTL1RtrvEqpt.String(), CmdTL1RtrvAlmAll.String(), CmdTL1RtrvCondAll.String(),
		CmdTL1RtrvSwVer.String(), CmdTL1RtrvMapNetwork.String(),
	}
}

// ciscoONSSession is the per-channel state for one ONS 15454 node.
type ciscoONSSession struct {
	loggedIn  bool
	sid       string
	serial    string
	localEQPT []byte
	eneEQPT   map[string][]byte // end NE TID (upper-case) -> its inventory
	eneOrder  []string          // end NE TIDs in file order, for RTRV-MAP-NETWORK
}

func (ciscoONSTL1) Serve(ctx *sessionCtx) {
	s := &ciscoONSSession{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
	if len(ctx.dev.Data) > 0 {
		s.localEQPT, s.eneEQPT, s.eneOrder = indexSections(ctx.dev.Data, ciscoENEMarker)
	}

	tl1Serve(ctx, ciscoONSPrompt, func(raw string) (Command, Response) {
		return ctx.dispatchCiscoONS(raw, s)
	})
}

// ciscoONSPrompt is the greeting and per-command prompt.
const ciscoONSPrompt = "\r\n< "

func (ctx *sessionCtx) dispatchCiscoONS(raw string, s *ciscoONSSession) (Command, Response) {
	verb, tid, ctag := parseTL1(raw)
	if verb == "" {
		return CmdTL1Unknown, Response{}
	}

	if verb == "ACT-USER" {
		user, pass := parseActUser(raw)
		if ctx.validTL1Login(user, pass) {
			s.loggedIn = true

			return CmdTL1ActUser, Response{Output: tl1Compld(s.sid, ctag, actUserPayload(user))}
		}

		return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "PLNA")}
	}

	if !s.loggedIn {
		return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "PLNA")}
	}

	// RTRV-MAP-NETWORK lists everything reachable from this gateway, answered locally.
	if verb == "RTRV-MAP-NETWORK" {
		return CmdTL1RtrvMapNetwork, Response{Output: tl1Compld(s.sid, ctag, mapNetworkPayload(s))}
	}

	respSID := s.sid
	eqpt := s.localEQPT
	if !tidIsLocal(tid, s.sid) {
		body, ok := s.eneEQPT[strings.ToUpper(tid)]
		if !ok {
			return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "IIAC")}
		}
		respSID, eqpt = strings.ToUpper(tid), body
	}

	switch verb {
	case "RTRV-EQPT":
		if len(eqpt) > 0 {
			return CmdTL1RtrvEqpt, Response{
				Output:       tl1CompldHeader(respSID, ctag),
				ConfigOutput: eqpt,
				Trailer:      []byte(";\r\n"),
			}
		}

		return CmdTL1RtrvEqpt, Response{Output: tl1Compld(respSID, ctag, ciscoONSEqptPayload(s))}
	case "RTRV-ALM-ALL":
		return CmdTL1RtrvAlmAll, Response{Output: tl1Compld(respSID, ctag, almPayload(respSID))}
	case "RTRV-COND-ALL":
		return CmdTL1RtrvCondAll, Response{Output: tl1Compld(respSID, ctag, condPayload(respSID))}
	case "RTRV-SW-VER":
		return CmdTL1RtrvSwVer, Response{Output: tl1Compld(respSID, ctag, ciscoONSSwVerPayload())}
	}

	return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "ICNV")}
}

// mapNetworkPayload answers RTRV-MAP-NETWORK. Records are positional:
//
//	"<IPADDR>,<NODENAME>,<PRODUCT>"
//
// NODENAME is the node's TID. PRODUCT is the platform type, and the vendor documentation
// notes it comes back as UNKNOWN for nodes running a different software version - so one
// element is emitted that way deliberately, to keep a client honest about handling it.
func mapNetworkPayload(s *ciscoONSSession) string {
	var b strings.Builder

	// The gateway lists itself as well as the nodes behind it.
	fmt.Fprintf(&b, "   \"%s,%s,15454\"\r\n", ciscoONSAddr(0), s.sid)

	for i, tid := range s.eneOrder {
		product := "15454"
		if i%7 == 6 {
			product = "UNKNOWN"
		}
		fmt.Fprintf(&b, "   \"%s,%s,%s\"\r\n", ciscoONSAddr(i+1), tid, product)
	}

	return b.String()
}

// ciscoONSAddr gives each node a stable management address.
func ciscoONSAddr(i int) string {
	return fmt.Sprintf("172.20.222.%d", 225-i)
}

func ciscoONSEqptPayload(s *ciscoONSSession) string {
	return fmt.Sprintf("   \"SLOT-1::PROVISIONED,TYPE=15454-TCC2P,SN=%s,STATE=IS-NR\"\r\n", s.serial)
}

func ciscoONSSwVerPayload() string {
	return "   \"SWVER=09.10-005E-16.28,LOADSTATE=ACTIVE\"\r\n"
}
