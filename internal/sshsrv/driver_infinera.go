package sshsrv

import (
	"fmt"
	"strings"
)

func init() { registerDriver(infineraTL1{}) }

// infineraTL1 is the Infinera DTN-X personality: a TL1 management interface reached over SSH.
//
// It shares the TL1 core in tl1.go with the Ciena driver and differs in three ways that
// matter to a client:
//
//   - Neighbours come from RTRV-TIDMAP, not RTRV-NE-LIST, and the records are keyword-shaped
//     with an empty AID ("::TID=...") rather than carrying a shelf AID.
//   - The response to RTRV-TIDMAP is PAGED. Intermediate blocks carry the completion code
//     RTRV and only the last carries COMPLD, with a prompt written between them. A client
//     that stops reading at the first prompt gets a partial answer and leaves the remaining
//     blocks on the wire.
//   - The response header carries the system name, not the addressed TID, so a client cannot
//     verify routing by comparing the header SID to the TID it asked for.
//
// All three are real DTN-X behaviour taken from customer session logs, and all three break a
// client written against Ciena alone. That is the point of simulating them.
type infineraTL1 struct{}

// infineraRNEMarker delimits each remote node's inventory inside a gateway's config file.
var infineraRNEMarker = []byte(";;NODE ")

// infineraTidmapPageSize is how many records fit in one RTRV-TIDMAP block before the node
// pages. Small enough that a modest fleet still produces several blocks, because a
// single-block response would not exercise the paging path at all.
const infineraTidmapPageSize = 10

func (infineraTL1) Name() string { return "infinera_tl1" }

// RequiresSSHAuth: like other TL1 personalities, DTN-X authenticates in-band via ACT-USER.
func (infineraTL1) RequiresSSHAuth() bool { return false }

// Commands reuses the shared CmdTL1* labels rather than minting an Infinera-specific set.
// The metric label answers "which verb", not "which vendor" - the vendor is already a
// property of the device. Per-vendor label sets would multiply cardinality by the number of
// vendors for no analytical gain, and TestMetrics_Cardinality caps it deliberately.
func (infineraTL1) Commands() []string {
	return []string{
		CmdTL1Unknown.String(), CmdTL1Deny.String(), CmdTL1ActUser.String(),
		CmdTL1RtrvEqpt.String(), CmdTL1RtrvAlmAll.String(), CmdTL1RtrvCondAll.String(),
		CmdTL1RtrvSwVer.String(), CmdTL1RtrvSys.String(), CmdTL1RtrvTidmap.String(),
	}
}

// infineraSession is the per-channel state for one DTN-X node.
type infineraSession struct {
	loggedIn  bool
	sid       string            // system name, shown in every response header
	serial    string            // chassis serial, substituted into synthesized payloads
	localEQPT []byte            // this node's own inventory (sub-slice of dev.Data)
	nodeEQPT  map[string][]byte // remote node TID (upper-case) -> its inventory
	nodeOrder []string          // remote TIDs in file order, for RTRV-TIDMAP
}

func (infineraTL1) Serve(ctx *sessionCtx) {
	s := &infineraSession{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
	if len(ctx.dev.Data) > 0 {
		s.localEQPT, s.nodeEQPT, s.nodeOrder = indexSections(ctx.dev.Data, infineraRNEMarker)
	}

	tl1Serve(ctx, infineraPrompt, func(raw string) (Command, Response) {
		return ctx.dispatchInfinera(raw, s)
	})
}

// infineraPrompt is the greeting and per-command prompt. Note it is NOT Ciena's "< ": the
// prompt is vendor behaviour, not part of TL1.
const infineraPrompt = "\r\n> "

// dispatchInfinera parses one raw TL1 command and produces a Response. The login gate matches
// every TL1 node: anything but ACT-USER before a successful login is denied.
func (ctx *sessionCtx) dispatchInfinera(raw string, s *infineraSession) (Command, Response) {
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

	// RTRV-TIDMAP is always answered by the node you are attached to.
	if verb == "RTRV-TIDMAP" {
		return CmdTL1RtrvTidmap, Response{Output: tidmapPages(s, ctag)}
	}

	// Resolve the target node. Empty/ALL/own-SID answers locally; any other TID routes to
	// that node's data.
	respSID := s.sid
	eqpt := s.localEQPT
	if !tidIsLocal(tid, s.sid) {
		body, ok := s.nodeEQPT[strings.ToUpper(tid)]
		if !ok {
			return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "IIAC")}
		}
		eqpt = body

		// Deliberately NOT respSID = tid. A DTN-X answers under its own system name even
		// when relaying for another node, so a client that verifies routing by comparing
		// the header SID to the requested TID will reject every routed collection. This is
		// real behaviour and the simulator has to reproduce it.
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

		return CmdTL1RtrvEqpt, Response{Output: tl1Compld(respSID, ctag, infineraEqptPayload(s))}
	case "RTRV-ALM-ALL":
		return CmdTL1RtrvAlmAll, Response{Output: tl1Compld(respSID, ctag, almPayload(respSID))}
	case "RTRV-COND-ALL":
		return CmdTL1RtrvCondAll, Response{Output: tl1Compld(respSID, ctag, condPayload(respSID))}
	case "RTRV-SW-VER":
		return CmdTL1RtrvSwVer, Response{Output: tl1Compld(respSID, ctag, infineraSwVerPayload())}
	case "RTRV-SYS":
		return CmdTL1RtrvSys, Response{Output: tl1Compld(respSID, ctag, infineraSysPayload(respSID, s.serial))}
	}

	// ICNV = input command not valid.
	return CmdTL1Deny, Response{Output: tl1Deny(s.sid, ctag, "ICNV")}
}

// tidmapPages builds the paged RTRV-TIDMAP response: one block per page, every block but the
// last coded RTRV, with the prompt written between blocks exactly as the node does.
//
// The prompts matter. They are why a client that reads "until the prompt" sees only the first
// page and then finds the remaining pages waiting for it as the reply to its next command.
func tidmapPages(s *infineraSession, ctag string) []byte {
	var out strings.Builder

	if len(s.nodeOrder) == 0 {
		// A node with no neighbours still answers, with a single empty COMPLD block.
		return tl1Block(s.sid, ctag, "COMPLD", "")
	}

	for start := 0; start < len(s.nodeOrder); start += infineraTidmapPageSize {
		end := start + infineraTidmapPageSize
		if end > len(s.nodeOrder) {
			end = len(s.nodeOrder)
		}

		code := "RTRV"
		if end == len(s.nodeOrder) {
			code = "COMPLD"
		}

		var page strings.Builder
		for _, tid := range s.nodeOrder[start:end] {
			fmt.Fprintf(&page, "   \"::TID=%s,NODEID=%s,ROUTERID=%s\"\r\n",
				tid, infineraNodeID(tid), infineraRouterID(tid))
		}

		out.Write(tl1Block(s.sid, ctag, code, page.String()))

		// The prompt the node emits between pages.
		if code == "RTRV" {
			out.WriteString(infineraPrompt)
		}
	}

	return []byte(out.String())
}

// infineraNodeID derives the stable node identifier a DTN-X reports for a TID. Deterministic
// so a given TID always maps to the same id across runs and across gateways.
func infineraNodeID(tid string) string {
	sum := 0
	for _, r := range tid {
		sum = sum*31 + int(r)
	}
	if sum < 0 {
		sum = -sum
	}

	return fmt.Sprintf("MA%010d", sum%10000000000)
}

// infineraRouterID derives the node's router id, reported alongside the TID.
func infineraRouterID(tid string) string {
	sum := 0
	for _, r := range tid {
		sum += int(r)
	}

	return fmt.Sprintf("11.253.152.%d", 33+sum%200)
}

func infineraEqptPayload(s *infineraSession) string {
	return fmt.Sprintf("   \"CHASSIS-1::PROVISIONED,TYPE=DTN-X-XTC10,SN=%s,SWVER=R21.3,STATE=IS-NR\"\r\n", s.serial)
}

func infineraSwVerPayload() string {
	return "   \"SWVER=R21.3,LOADSTATE=ACTIVE\"\r\n"
}

func infineraSysPayload(sid, serial string) string {
	return fmt.Sprintf("   \"%s::TYPE=DTN-X-XTC10,SN=%s,STATE=IS-NR\"\r\n", sid, serial)
}
