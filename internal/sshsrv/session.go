package sshsrv

import (
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/metrics"
)

// sessionCtx is everything the shell loop needs to service one channel.
type sessionCtx struct {
	ch             ssh.Channel
	dev            *configs.Device
	enablePassword string
	delayMinMS     int
	delayMaxMS     int
	logger         *slog.Logger
	rng            *rand.Rand
	metrics        *metrics.Registry
	outcome        *sessionOutcome
	faults         *fault.Set
	rawConn        net.Conn // for hard-close (disconnect_mid) fault
}

// runShell is the per-channel interactive loop. It does line editing with echo
// so the `ssh` CLI client is usable, reads one line at a time, resolves to a
// Command, applies a uniform response delay, and writes back the response.
//
// It returns when the channel closes, the client requests exit, or a read error
// occurs. Channel close is the caller's responsibility. Metrics hooks are
// inline so the hot path has no extra indirection.
func runShell(ctx *sessionCtx) {
	state := &State{
		Hostname:    ctx.dev.Hostname,
		Serial:      ctx.dev.SerialNumber,
		ConfigBytes: ctx.dev.Data,
	}

	writeAndCount(ctx, []byte("\r\n"))
	writeAndCount(ctx, []byte(ctx.dev.Hostname+" line 0 is now available\r\n"))
	writeAndCount(ctx, []byte("\r\n"))

	for {
		prompt := state.Hostname + ">"
		if state.EnableMode {
			prompt = state.Hostname + "#"
		}
		if _, err := writeAndCount(ctx, []byte(prompt)); err != nil {
			return
		}

		line, err := readLine(ctx.ch, true)
		if err != nil {
			// Mid-session read error (EOF / reset) with no explicit exit
			// command ⇒ classify as disconnect. Authoritative exit commands
			// return via resp.Close below and leave outcome="ok".
			if ctx.outcome != nil {
				ctx.outcome.Set("disconnect")
			}
			return
		}
		cmdStart := time.Now()
		cmd, canonical := ResolveCommand(line)

		delayMS := ctx.delayMinMS
		if ctx.delayMaxMS > ctx.delayMinMS {
			delayMS += ctx.rng.Intn(ctx.delayMaxMS - ctx.delayMinMS + 1)
		}
		// slow_response fault: multiply delay by uniform[10,50], cap at 60s.
		// Guarantee at least 10ms of base so the multiplier is observable
		// even when the operator configured --response-delay-ms-max=0.
		if ctx.faults.Roll(ctx.rng, fault.TypeSlowResponse) {
			multiplier := 10 + ctx.rng.Intn(41) // inclusive 10..50
			if delayMS < 10 {
				delayMS = 10
			}
			delayMS *= multiplier
			if delayMS > 60000 {
				delayMS = 60000
			}
			if ctx.metrics != nil {
				ctx.metrics.FaultsInjected.WithLabelValues(fault.TypeSlowResponse.String()).Inc()
			}
		}
		if delayMS > 0 {
			time.Sleep(time.Duration(delayMS) * time.Millisecond)
		}

		resp := Dispatch(cmd, canonical, state)

		if resp.RequestEnablePassword {
			if _, err := writeAndCount(ctx, []byte("Password: ")); err != nil {
				return
			}
			pw, err := readLine(ctx.ch, false)
			if err != nil {
				if ctx.outcome != nil {
					ctx.outcome.Set("disconnect")
				}
				return
			}
			if pw == ctx.enablePassword {
				state.EnableMode = true
			} else {
				writeAndCount(ctx, []byte("% Access denied\r\n"))
			}
			observeCmd(ctx, cmd, cmdStart)
			continue
		}

		if len(resp.Output) > 0 {
			if _, err := writeAndCount(ctx, resp.Output); err != nil {
				return
			}
		}
		if len(resp.ConfigOutput) > 0 {
			// disconnect_mid: write a 20-40% prefix then hard-RST the TCP conn.
			// Happens before any malformed check because the connection is
			// going away anyway. Observe the command duration first so phase 5
			// metrics still reflect work the dispatcher did.
			if ctx.faults.Roll(ctx.rng, fault.TypeDisconnectMid) {
				window := 20 + ctx.rng.Intn(21) // 20..40 inclusive
				n := len(resp.ConfigOutput) * window / 100
				_, _ = writeAndCount(ctx, resp.ConfigOutput[:n])
				if ctx.metrics != nil {
					ctx.metrics.FaultsInjected.WithLabelValues(fault.TypeDisconnectMid.String()).Inc()
				}
				if ctx.outcome != nil {
					ctx.outcome.Set("disconnect")
				}
				observeCmd(ctx, cmd, cmdStart)
				hardCloseTCP(ctx.rawConn)
				return
			}

			// malformed: corrupt the stream in one of three ways. Preserves
			// the zero-copy hot path for before/after segments; only the
			// perturbation itself allocates (bit flip = 1 byte, inject = ~60
			// byte junk marker, truncate = no allocation).
			if ctx.faults.Roll(ctx.rng, fault.TypeMalformed) {
				if err := writeMalformed(ctx, resp.ConfigOutput); err != nil {
					return
				}
				if ctx.metrics != nil {
					ctx.metrics.FaultsInjected.WithLabelValues(fault.TypeMalformed.String()).Inc()
				}
			} else {
				// Hot path: direct write of mmap'd bytes. Zero copy.
				if _, err := writeAndCount(ctx, resp.ConfigOutput); err != nil {
					return
				}
			}
			// Trailing CRLF so the next prompt lands on a fresh line.
			if _, err := writeAndCount(ctx, []byte("\r\n")); err != nil {
				return
			}
		}
		observeCmd(ctx, cmd, cmdStart)

		if resp.ExitEnable {
			state.EnableMode = false
			continue
		}
		if resp.Close {
			return
		}
	}
}

// writeAndCount writes to the channel and increments the bytes_sent counter
// by the number of bytes actually accepted. Metrics may be nil in unit tests.
func writeAndCount(ctx *sessionCtx, p []byte) (int, error) {
	n, err := ctx.ch.Write(p)
	if n > 0 && ctx.metrics != nil {
		ctx.metrics.BytesSent.Add(float64(n))
	}
	return n, err
}

// observeCmd records the command duration with the canonical Cmd* label.
// Nil-safe so dispatch-only tests don't need a full metrics registry.
func observeCmd(ctx *sessionCtx, cmd Command, start time.Time) {
	if ctx.metrics == nil {
		return
	}
	ctx.metrics.CommandDuration.WithLabelValues(cmd.String()).Observe(time.Since(start).Seconds())
}

// hardCloseTCP closes the underlying net.Conn, using SetLinger(0) to force an
// RST on TCPConn so the client sees "connection reset" rather than a graceful
// FIN. Any rcfg-style collector must interpret this as an unexpected
// disconnect, not a clean end-of-stream.
func hardCloseTCP(conn net.Conn) {
	if conn == nil {
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()
}

// malformedMarker is the bytes injected into the middle of the stream in
// malformed-inject mode. Big enough to be grep-able in collector dumps.
var malformedMarker = []byte("\r\n!!! RCFG-SIM-MALFORMED-FAULT-INJECTION-MARKER !!!\r\n")

// writeMalformed applies one of three corruption modes to the running-config
// stream. Chooses uniformly. Writes mmap bytes directly wherever possible —
// only the perturbation itself allocates. Returns a write error if the
// channel is gone.
func writeMalformed(ctx *sessionCtx, body []byte) error {
	if len(body) == 0 {
		return nil
	}
	mode := ctx.rng.Intn(3)
	switch mode {
	case 0:
		// truncate: write 90-99% of the body then stop. The channel stays
		// open and the next prompt appears, so the client believes the
		// command completed with a short body.
		pct := 90 + ctx.rng.Intn(10) // 90..99 inclusive
		n := len(body) * pct / 100
		if n < 1 {
			n = 1
		}
		_, err := writeAndCount(ctx, body[:n])
		return err

	case 1:
		// inject: write the middle-boundary prefix, the marker, then the
		// suffix. Three writes, one small allocation (the marker itself,
		// which is a package-level var so even that doesn't allocate per call).
		mid := len(body) / 2
		if _, err := writeAndCount(ctx, body[:mid]); err != nil {
			return err
		}
		if _, err := writeAndCount(ctx, malformedMarker); err != nil {
			return err
		}
		_, err := writeAndCount(ctx, body[mid:])
		return err

	default:
		// bit flip: pick a byte in the middle 50% of the stream, XOR 0xFF.
		// One-byte stack allocation for the flipped byte; before and after
		// are written directly from the mmap.
		lo := len(body) / 4
		hi := 3 * len(body) / 4
		if hi <= lo {
			_, err := writeAndCount(ctx, body)
			return err
		}
		off := lo + ctx.rng.Intn(hi-lo)
		if _, err := writeAndCount(ctx, body[:off]); err != nil {
			return err
		}
		flipped := [1]byte{body[off] ^ 0xFF}
		if _, err := writeAndCount(ctx, flipped[:]); err != nil {
			return err
		}
		_, err := writeAndCount(ctx, body[off+1:])
		return err
	}
}

var errUserAborted = errors.New("user aborted")

// readLine reads one line from the channel, performing minimal line editing:
//   - echo printable chars back (if echo=true)
//   - handle backspace (0x08 / 0x7f)
//   - terminate on CR or LF (with CRLF echo back so the client sees a fresh line)
//   - Ctrl-C ⇒ errUserAborted
//
// Returns the line contents without the terminator. No allocation beyond the
// small per-line buffer — we do not read the mmap'd config body through here.
func readLine(ch io.ReadWriter, echo bool) (string, error) {
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
				if echo {
					_, _ = ch.Write([]byte("\b \b"))
				}
			}
		case '\r':
			// Swallow the companion LF if the client sends CRLF, but don't block
			// waiting for one — most clients in pty mode send CR only.
			_, _ = ch.Write([]byte("\r\n"))
			return string(buf), nil
		case '\n':
			_, _ = ch.Write([]byte("\r\n"))
			return string(buf), nil
		case 0x03:
			_, _ = ch.Write([]byte("^C\r\n"))
			return "", errUserAborted
		case 0x04:
			// EOF (Ctrl-D): close session if buffer is empty, otherwise ignore.
			if len(buf) == 0 {
				return "", io.EOF
			}
		default:
			if c >= 0x20 && c < 0x7f {
				buf = append(buf, c)
				if echo {
					_, _ = ch.Write([]byte{c})
				}
			}
		}
	}
}

// writeLine writes "line\r\n". Used for short status lines; never for the
// config body (which is zero-copy via ConfigOutput).
func writeLine(w io.Writer, line string) {
	_, _ = w.Write([]byte(line))
	_, _ = w.Write([]byte("\r\n"))
}

// trimLine strips a CR or LF suffix — used where we ingest a line from a raw
// read rather than through readLine (e.g. if we ever accept exec channels).
func trimLine(s string) string {
	return strings.TrimRight(s, "\r\n")
}
