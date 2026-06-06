package sshsrv

import "time"

func init() { registerDriver(ciscoIOS{}) }

// ciscoIOS is the Cisco IOS personality: the original simulator behaviour,
// extracted verbatim from runShell. The greeting, prompts, per-token
// prefix-matched dispatch, and enable-mode password sub-flow are unchanged; the
// shared response-delay and post-dispatch machinery is delegated to
// (*sessionCtx).applyResponseDelay / emit so every vendor injects faults and
// records metrics identically.
type ciscoIOS struct{}

func (ciscoIOS) Name() string { return "cisco_ios" }

// RequiresSSHAuth: Cisco IOS authenticates at the SSH layer.
func (ciscoIOS) RequiresSSHAuth() bool { return true }

func (ciscoIOS) Commands() []string {
	return []string{
		CmdUnknown.String(), CmdEmpty.String(), CmdAmbiguous.String(),
		CmdTerminalLength.String(), CmdTerminalPager.String(), CmdEnable.String(),
		CmdShowVersion.String(), CmdShowRunningConfig.String(),
		CmdShowStartupConfig.String(), CmdShowInventory.String(), CmdExit.String(),
	}
}

// Serve is the per-channel interactive loop. It does line editing with echo so
// the `ssh` CLI client is usable, reads one line at a time, resolves to a
// Command, applies the shared response delay, and emits the response.
//
// It returns when the channel closes, the client requests exit, or a read error
// occurs. Channel close is the caller's responsibility.
func (ciscoIOS) Serve(ctx *sessionCtx) {
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

		ctx.applyResponseDelay()

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

		if ctx.emit(cmd, cmdStart, resp) {
			return
		}

		if resp.ExitEnable {
			state.EnableMode = false
			continue
		}
		if resp.Close {
			return
		}
	}
}
