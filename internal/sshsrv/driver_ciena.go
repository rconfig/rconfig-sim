package sshsrv

import (
	"fmt"
	"io"
	"strings"
	"time"
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

func (cienaTL1) Name() string { return "ciena_tl1" }

// RequiresSSHAuth: Ciena TL1 authenticates in-band via ACT-USER, so the SSH
// transport can accept the connection without a password challenge.
func (cienaTL1) RequiresSSHAuth() bool { return false }

func (cienaTL1) Commands() []string {
	return []string{
		CmdTL1Unknown.String(), CmdTL1Deny.String(), CmdTL1ActUser.String(),
		CmdTL1RtrvEqpt.String(), CmdTL1RtrvAlmAll.String(), CmdTL1RtrvCondAll.String(),
		CmdTL1RtrvActiveUser.String(), CmdTL1RtrvSwVer.String(), CmdTL1RtrvSys.String(),
	}
}

// tl1Session is the per-channel TL1 state. Driver-local — kept off the Cisco
// State struct so vendor concepts stay disjoint.
type tl1Session struct {
	loggedIn bool
	sid      string // system identifier / TID, shown in every response header
	serial   string // shelf serial, substituted into synthesized payloads
}

func (cienaTL1) Serve(ctx *sessionCtx) {
	s := &tl1Session{sid: ctx.dev.Hostname, serial: ctx.dev.SerialNumber}
	for {
		// The "<" prompt is both greeting and per-command prompt in TL1.
		if _, err := writeAndCount(ctx, []byte("\r\n< ")); err != nil {
			return
		}
		raw, err := readTL1(ctx.ch)
		if err != nil {
			if ctx.outcome != nil {
				ctx.outcome.Set("disconnect")
			}
			return
		}
		cmdStart := time.Now()
		ctx.applyResponseDelay()
		cmd, resp := ctx.dispatchTL1(raw, s)
		if ctx.emit(cmd, cmdStart, resp) {
			return
		}
		if resp.Close {
			return
		}
	}
}

// dispatchTL1 parses one raw TL1 command and produces a Response. The login
// gate is enforced here: anything other than ACT-USER before a successful login
// returns DENY.
func (ctx *sessionCtx) dispatchTL1(raw string, s *tl1Session) (Command, Response) {
	verb, ctag := parseTL1(raw)
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

	switch verb {
	case "RTRV-EQPT":
		// Stream the generated inventory zero-copy if present; otherwise
		// synthesize a small canned block so the driver works without a
		// generated payload (unit tests, hand-rolled manifests).
		if len(ctx.dev.Data) > 0 {
			return CmdTL1RtrvEqpt, Response{
				Output:       tl1CompldHeader(s.sid, ctag),
				ConfigOutput: ctx.dev.Data,
				Trailer:      []byte(";\r\n"),
			}
		}
		return CmdTL1RtrvEqpt, Response{Output: tl1Compld(s.sid, ctag, eqptPayload(s))}
	case "RTRV-ALM-ALL":
		return CmdTL1RtrvAlmAll, Response{Output: tl1Compld(s.sid, ctag, almPayload(s))}
	case "RTRV-COND-ALL":
		return CmdTL1RtrvCondAll, Response{Output: tl1Compld(s.sid, ctag, condPayload(s))}
	case "RTRV-ACTIVE-USER":
		return CmdTL1RtrvActiveUser, Response{Output: tl1Compld(s.sid, ctag, activeUserPayload(ctx.username))}
	case "RTRV-SW-VER":
		return CmdTL1RtrvSwVer, Response{Output: tl1Compld(s.sid, ctag, swVerPayload(s))}
	case "RTRV-SYS":
		return CmdTL1RtrvSys, Response{Output: tl1Compld(s.sid, ctag, sysPayload(s))}
	default:
		// ICNV = input, command not valid.
		return CmdTL1Unknown, Response{Output: tl1Deny(s.sid, ctag, "ICNV")}
	}
}

// validTL1Login mirrors the SSH PasswordCallback semantics: empty configured
// password accepts any; a configured username/password must match.
func (ctx *sessionCtx) validTL1Login(user, pass string) bool {
	if ctx.password != "" && pass != ctx.password {
		return false
	}
	if ctx.username != "" && user != ctx.username {
		return false
	}
	return true
}

// parseTL1 splits a TL1 command into its verb and CTAG. Grammar:
//
//	VERB:TID:AID:CTAG[:GENBLK][:payload]
//
// The verb is everything before the first ":"; the CTAG is the 4th field.
func parseTL1(raw string) (verb, ctag string) {
	fields := strings.Split(strings.TrimSpace(raw), ":")
	if len(fields) > 0 {
		verb = strings.ToUpper(strings.TrimSpace(fields[0]))
	}
	if len(fields) > 3 {
		ctag = strings.TrimSpace(fields[3])
	}
	return verb, ctag
}

// parseActUser extracts the username and password from an ACT-USER command:
//
//	ACT-USER::<username>:<ctag>::<password>
//
// username is field[2], password is field[5].
func parseActUser(raw string) (user, pass string) {
	fields := strings.Split(strings.TrimSpace(raw), ":")
	if len(fields) > 2 {
		user = strings.TrimSpace(fields[2])
	}
	if len(fields) > 5 {
		pass = strings.TrimSpace(fields[5])
	}
	return user, pass
}

// readTL1 reads one ";"-terminated TL1 command from the channel. Like the Cisco
// readLine it echoes printable input and handles backspace / Ctrl-C / Ctrl-D,
// but it terminates on ";" rather than newline and tolerates commands spanning
// multiple physical lines (CR/LF between tokens are echoed but not buffered).
func readTL1(ch io.ReadWriter) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for {
		n, err := ch.Read(one)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		c := one[0]
		switch c {
		case 0x7f, 0x08:
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				_, _ = ch.Write([]byte("\b \b"))
			}
		case ';':
			_, _ = ch.Write([]byte(";\r\n"))
			return string(buf), nil
		case '\r', '\n':
			// TL1 commands may span physical lines; echo a newline but keep
			// accumulating until the ";" terminator.
			_, _ = ch.Write([]byte("\r\n"))
		case 0x03:
			_, _ = ch.Write([]byte("^C\r\n"))
			return "", errUserAborted
		case 0x04:
			if len(buf) == 0 {
				return "", io.EOF
			}
		default:
			if c >= 0x20 && c < 0x7f {
				buf = append(buf, c)
				_, _ = ch.Write([]byte{c})
			}
		}
	}
}

// tl1Timestamp renders the response-header time in the YY-MM-DD HH:MM:SS form a
// 6500 uses. Isolated behind one function so output is easy to stub; no test
// hashes TL1 wire bytes.
func tl1Timestamp() string {
	return time.Now().UTC().Format("06-01-02 15:04:05")
}

// tl1CompldHeader renders the COMPLD block header (blank line, SID + timestamp,
// "M  <ctag> COMPLD"). Used standalone when the payload is streamed separately
// (zero-copy RTRV-EQPT) and the ";" terminator comes via Response.Trailer.
func tl1CompldHeader(sid, ctag string) []byte {
	var b strings.Builder
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "   %s %s\r\n", sid, tl1Timestamp())
	fmt.Fprintf(&b, "M  %s COMPLD\r\n", ctag)
	return []byte(b.String())
}

// tl1Compld renders a complete COMPLD block: header, payload (which supplies its
// own trailing CRLFs), and the ";" terminator.
func tl1Compld(sid, ctag, payload string) []byte {
	var b strings.Builder
	b.Write(tl1CompldHeader(sid, ctag))
	b.WriteString(payload)
	b.WriteString(";\r\n")
	return []byte(b.String())
}

// tl1Deny renders a DENY block with a 4-char TL1 error code.
func tl1Deny(sid, ctag, errCode string) []byte {
	var b strings.Builder
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "   %s %s\r\n", sid, tl1Timestamp())
	fmt.Fprintf(&b, "M  %s DENY\r\n", ctag)
	fmt.Fprintf(&b, "   %s\r\n", errCode)
	b.WriteString(";\r\n")
	return []byte(b.String())
}

// --- synthesized payloads (small, identity-substituted, like showVersionFor) ---

func actUserPayload(user string) string {
	var b strings.Builder
	b.WriteString("   /*AUTHTYPE=LOCAL*/\r\n")
	fmt.Fprintf(&b, "   /*USERID=%s*/\r\n", strings.ToUpper(user))
	return b.String()
}

func eqptPayload(s *tl1Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "   \"SHELF-1::PROVISIONED,SN=%s,TYPE=6500-7SLOT:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-1:OTR2,%s-01:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-2:WL3N,%s-02:IS-NR\"\r\n", s.serial)
	fmt.Fprintf(&b, "   \"SLOT-7:EDFA,%s-07:IS-NR\"\r\n", s.serial)
	return b.String()
}

func almPayload(s *tl1Session) string {
	var b strings.Builder
	b.WriteString("   \"SLOT-2:MN,CONTBUS,SA,,,,:\\\"Intermittent equipment communication\\\"\"\r\n")
	b.WriteString("   \"SLOT-7:MJ,T-LOS,NSA,,,,:\\\"Loss of signal\\\"\"\r\n")
	return b.String()
}

func condPayload(s *tl1Session) string {
	var b strings.Builder
	b.WriteString("   \"SLOT-1:T-OPR-OCH,NEND,,,,,:\\\"Optical power received\\\"\"\r\n")
	return b.String()
}

func activeUserPayload(user string) string {
	if user == "" {
		user = "ADMIN"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "   \"%s:ADMIN,ACTIVE\"\r\n", strings.ToUpper(user))
	return b.String()
}

func swVerPayload(s *tl1Session) string {
	return "   \"SWVER=12.4,LOAD=12.4-GA,STATUS=ACTIVE\"\r\n"
}

func sysPayload(s *tl1Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "   \"SID=%s,TYPE=6500-7SLOT,SHELFSN=%s\"\r\n", s.sid, s.serial)
	return b.String()
}
