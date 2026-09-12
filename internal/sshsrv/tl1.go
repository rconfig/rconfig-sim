package sshsrv

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"time"
)

// Shared TL1 machinery, vendor-neutral.
//
// TL1 (Telcordia GR-831) is one protocol spoken by several optical vendors, and the parts
// below are the parts they agree on: ";"-terminated commands that may span physical lines,
// the VERB:TID:AID:CTAG grammar, the ACT-USER login, and the response block framing with its
// SID/timestamp header and COMPLD or DENY code line.
//
// What differs per vendor is the verb set, the payload text those verbs return, and how
// neighbouring nodes are enumerated. Those live in the driver_<vendor>.go files, which keep
// only their session state, their verb switch, and their payload builders.
//
// These symbols are package-level in sshsrv, so a second TL1 driver cannot redeclare them.
// Extracting them here is what makes more than one TL1 vendor possible at all.

func tidIsLocal(tid, ownSID string) bool {
	t := strings.ToUpper(strings.TrimSpace(tid))
	return t == "" || t == "ALL" || t == strings.ToUpper(ownSID)
}

// indexSections splits an mmap'd Ciena config into the GNE-own EQPT inventory
// and the per-RNE inventories, using lines that begin with ";;RNE <TID>" as
// section separators. Every returned []byte is a zero-copy sub-slice of data.
//
// A file with no ";;RNE " marker is a legacy single-NE config: localEQPT is the
// whole file and there are no RNEs. This keeps the standalone ciena-6500-tl1
// model byte-identical and driver behaviour unchanged for it.

func indexSections(data []byte, marker []byte) (localEQPT []byte, rne map[string][]byte, order []string) {
	// Find section boundaries: the start of each ";;RNE " line (at column 0,
	// i.e. either at offset 0 or immediately after a newline).
	type bound struct {
		tid             string
		bodyStart, line int
	}
	var bounds []bound
	for i := 0; i < len(data); i++ {
		atLineStart := i == 0 || data[i-1] == '\n'
		if !atLineStart || !bytes.HasPrefix(data[i:], marker) {
			continue
		}
		// TID runs to end of this marker line.
		nl := bytes.IndexByte(data[i:], '\n')
		lineEnd := len(data)
		bodyStart := len(data)
		if nl >= 0 {
			lineEnd = i + nl
			bodyStart = lineEnd + 1
		}
		tid := strings.ToUpper(strings.TrimSpace(string(data[i+len(marker) : lineEnd])))
		bounds = append(bounds, bound{tid: tid, bodyStart: bodyStart, line: i})
	}

	if len(bounds) == 0 {
		return data, nil, nil
	}

	localEQPT = data[:bounds[0].line]
	rne = make(map[string][]byte, len(bounds))
	for i, b := range bounds {
		end := len(data)
		if i+1 < len(bounds) {
			end = bounds[i+1].line
		}
		if b.tid == "" {
			continue
		}
		rne[b.tid] = data[b.bodyStart:end]
		order = append(order, b.tid)
	}
	return localEQPT, rne, order
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

// parseTL1 splits a TL1 command into its verb, target TID, and CTAG. Grammar:
//
//	VERB:TID:AID:CTAG[:GENBLK][:payload]
//
// The verb is field 0 and the TID is field 1. CTAG is the 4th field (index 3)
// in the strict form (VERB::AID:CTAG, e.g. "RTRV-EQPT::ALL:100"), but operators
// commonly omit the empty AID placeholder when addressing an NE directly
// (VERB:TID:CTAG, e.g. "RTRV-EQPT:RNE-LIMERICK:3" or "RTRV-NBR:ALL:2"). So CTAG
// is field 3 when the command has 4+ fields, otherwise the last field. (The ";"
// terminator is stripped by readTL1 before this is called, so it is never a field.)

func parseTL1(raw string) (verb, tid, ctag string) {
	fields := strings.Split(strings.TrimSpace(raw), ":")
	if len(fields) > 0 {
		verb = strings.ToUpper(strings.TrimSpace(fields[0]))
	}
	if len(fields) > 1 {
		tid = strings.TrimSpace(fields[1])
	}
	if len(fields) >= 4 {
		ctag = strings.TrimSpace(fields[3])
	} else if len(fields) > 0 {
		ctag = strings.TrimSpace(fields[len(fields)-1])
	}
	return verb, tid, ctag
}

// parseActUser extracts the username and password from an ACT-USER command:
//
//	ACT-USER::<username>:<ctag>::<password>
//
// username is field[2], password is field[5]. Both forms of the password are
// accepted — bare, and the TL1 (GR-831) quoted form "<password>" a real 6500
// requires once the password contains anything beyond plain alphanumerics.
// Being liberal here keeps older rConfig builds, which send the password bare,
// working against a newer simulator.
//
// The password is the last ACT-USER parameter, so fields 5..n are re-joined
// before unquoting: that way a password containing ":" — the TL1 field
// separator, and the whole reason the quotes exist — survives intact instead of
// being truncated at the colon.

func parseActUser(raw string) (user, pass string) {
	fields := strings.Split(strings.TrimSpace(raw), ":")
	if len(fields) > 2 {
		user = unquoteTL1(fields[2])
	}
	if len(fields) > 5 {
		pass = unquoteTL1(strings.Join(fields[5:], ":"))
	}
	return user, pass
}

// unquoteTL1 trims surrounding whitespace and then strips one matching pair of
// enclosing double quotes, leaving anything unquoted untouched. Only the
// outermost pair is removed — trimming every quote character would corrupt a
// password that itself contains one — and unwrapping after the whitespace trim
// keeps significant leading/trailing spaces that the quotes were protecting.

func unquoteTL1(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// readTL1 reads one ";"-terminated TL1 command from the channel. Like the Cisco
// readLine it echoes printable input and handles backspace / Ctrl-C / Ctrl-D,
// but it terminates on ";" rather than newline and tolerates commands spanning
// multiple physical lines (CR/LF between tokens are echoed but not buffered).
//
// The terminator search is quote-aware: inside a TL1 quoted string a ";" is
// ordinary data, so a password such as "pa;ss" is read whole rather than cut
// short at the semicolon.

func readTL1(ch io.ReadWriter) (string, error) {
	var buf []byte
	var inQuote bool
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
				if buf[len(buf)-1] == '"' {
					inQuote = !inQuote
				}
				buf = buf[:len(buf)-1]
				_, _ = ch.Write([]byte("\b \b"))
			}
		case ';':
			if inQuote {
				buf = append(buf, c)
				_, _ = ch.Write([]byte{c})
				continue
			}
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
				if c == '"' {
					inQuote = !inQuote
				}
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

// tl1Serve runs the interactive loop every TL1 driver shares: emit the prompt, read one
// ";"-terminated command, hand it to the vendor's dispatch, emit the response, repeat.
//
// The prompt is a parameter rather than a constant because it is not as standard as it looks:
// Ciena answers on "< ", other vendors do not.
//
// A driver must route every response byte through emit (for fault injection, command_duration
// and byte counting), which is why the loop lives here rather than being copied per vendor.
func tl1Serve(ctx *sessionCtx, prompt string, dispatch func(raw string) (Command, Response)) {
	for {
		if _, err := writeAndCount(ctx, []byte(prompt)); err != nil {
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

		cmd, resp := dispatch(raw)
		if ctx.emit(cmd, cmdStart, resp) {
			return
		}

		if resp.Close {
			return
		}
	}
}
