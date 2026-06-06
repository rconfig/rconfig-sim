package sshsrv

import (
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

// Driver renders one vendor/model's interactive SSH session. A driver owns its
// entire read/parse/respond loop via Serve. It MUST route every response byte
// back through (*sessionCtx).emit (for fault injection, command_duration, and
// byte-counting) or writeAndCount (for greeting/prompt text), so the observable
// fault and metric behaviour is uniform across vendors.
//
// Adding a new vendor is one self-contained file: implement Driver, and
// registerDriver it from an init(). Nothing else in the hot path needs to know
// the vendor exists.
type Driver interface {
	// Name is the manifest `template` value this driver is registered under
	// (e.g. "cisco_ios", "ciena_tl1"). Stable and user-facing.
	Name() string

	// Commands returns the Command label-string values this driver can emit on
	// the rcfgsim_command_duration_seconds `command` label. The server unions
	// these across all registered drivers and pre-registers them, so the
	// cardinality set is assembled from drivers rather than hardcoded.
	Commands() []string

	// RequiresSSHAuth reports whether this device authenticates at the SSH
	// transport layer. Cisco IOS does (SSH password); Ciena TL1 does not — it
	// authenticates in-band via ACT-USER. Consulted only under --ssh-auth=driver.
	RequiresSSHAuth() bool

	// Serve runs the full interactive loop for one channel. It returns when the
	// client exits, the channel closes, or a read error occurs. Closing the
	// channel afterwards remains the caller's responsibility.
	Serve(ctx *sessionCtx)
}

// driverRegistry maps a manifest template name to its Driver. Populated by the
// init() in each driver_*.go file and read-only after package initialisation,
// so no locking is required.
var driverRegistry = map[string]Driver{}

func registerDriver(d Driver) { driverRegistry[d.Name()] = d }

// driverFor resolves a template name to a Driver, defaulting to cisco_ios for
// empty or unknown values. This keeps any pre-existing manifest (whose template
// column was always "cisco_ios", or absent) behaving exactly as before.
func driverFor(template string) Driver {
	if d, ok := driverRegistry[template]; ok {
		return d
	}
	return driverRegistry["cisco_ios"]
}

// registeredCommands returns the sorted-by-insertion union of every registered
// driver's Commands(), de-duplicated. The server uses it to pre-register the
// command_duration label values at zero so they appear in scrapes immediately.
func registeredCommands() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, d := range driverRegistry {
		for _, c := range d.Commands() {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// applyResponseDelay computes the base per-command jitter and, if the
// slow_response fault fires, multiplies it by uniform[10,50] capped at 60s,
// then sleeps. Drivers call this once between resolving a command and
// dispatching it (the slow_response label is attributed to the resolved
// command via the fault counter here).
//
// This is the exact delay logic previously inline in runShell; it is the only
// place a response sleep happens, so neither driver may sleep elsewhere.
func (ctx *sessionCtx) applyResponseDelay() {
	delayMS := ctx.delayMinMS
	if ctx.delayMaxMS > ctx.delayMinMS {
		delayMS += ctx.rng.Intn(ctx.delayMaxMS - ctx.delayMinMS + 1)
	}
	// slow_response fault: multiply delay by uniform[10,50], cap at 60s.
	// Guarantee at least 10ms of base so the multiplier is observable even when
	// the operator configured --response-delay-ms-max=0.
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
}

// emit applies the shared response machinery to a Response: the verbatim
// Output, then the zero-copy ConfigOutput (mmap'd bytes) with disconnect_mid /
// malformed fault injection, an optional Trailer, and a trailing CRLF, and
// finally observes the command_duration metric. cmdStart is captured by the
// caller just before dispatch so the duration covers the same span as before.
//
// It returns closed=true wherever the session must terminate — a write error,
// or a disconnect_mid hard close — so the caller's loop can return. This is the
// exact post-dispatch logic previously inline in runShell.
func (ctx *sessionCtx) emit(cmd Command, cmdStart time.Time, resp Response) (closed bool) {
	if len(resp.Output) > 0 {
		if _, err := writeAndCount(ctx, resp.Output); err != nil {
			return true
		}
	}
	if len(resp.ConfigOutput) > 0 {
		// disconnect_mid: write a 20-40% prefix then hard-RST the TCP conn.
		// Happens before any malformed check because the connection is going
		// away anyway. Observe the command duration first so phase-5 metrics
		// still reflect work the dispatcher did.
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
			return true
		}

		// malformed: corrupt the stream in one of three ways. Preserves the
		// zero-copy hot path for before/after segments.
		if ctx.faults.Roll(ctx.rng, fault.TypeMalformed) {
			if err := writeMalformed(ctx, resp.ConfigOutput); err != nil {
				return true
			}
			if ctx.metrics != nil {
				ctx.metrics.FaultsInjected.WithLabelValues(fault.TypeMalformed.String()).Inc()
			}
		} else {
			// Hot path: direct write of mmap'd bytes. Zero copy.
			if _, err := writeAndCount(ctx, resp.ConfigOutput); err != nil {
				return true
			}
		}
		// Optional driver-supplied trailer (e.g. the TL1 closing ";"), then a
		// trailing CRLF so the next prompt lands on a fresh line.
		if len(resp.Trailer) > 0 {
			if _, err := writeAndCount(ctx, resp.Trailer); err != nil {
				return true
			}
		}
		if _, err := writeAndCount(ctx, []byte("\r\n")); err != nil {
			return true
		}
	}
	observeCmd(ctx, cmd, cmdStart)
	return false
}
