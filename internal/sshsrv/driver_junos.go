package sshsrv

import (
	"strings"
	"time"
)

func init() { registerDriver(junos{}) }

// junos is a minimal Juniper Junos operational-mode personality: a
// "<user>@<host>> " prompt, no enable mode, the two CLI setup commands rConfig
// sends, and "show configuration" served from the device's config file the
// same way cisco_ios serves show running-config. Anything else is answered
// from --commands-root or with the Junos "unknown command." error.
type junos struct{}

func (junos) Name() string { return "junos" }

// RequiresSSHAuth: Junos authenticates at the SSH layer.
func (junos) RequiresSSHAuth() bool { return true }

func (junos) Commands() []string {
	return []string{
		CmdUnknown.String(), CmdEmpty.String(), CmdExit.String(),
		CmdJunosSetCli.String(), CmdJunosShowConfiguration.String(),
	}
}

const junosUnknownMsg = "unknown command.\r\n"

func (junos) Serve(ctx *sessionCtx) {
	prompt := junosPrompt(ctx)

	for {
		if _, err := writeAndCount(ctx, []byte(prompt)); err != nil {
			return
		}

		line, err := readLine(ctx.ch, true)
		if err != nil {
			if ctx.outcome != nil {
				ctx.outcome.Set("disconnect")
			}
			return
		}

		if handled, closed := ctx.serveCommandFile(line); handled {
			if closed {
				return
			}
			continue
		}

		cmdStart := time.Now()
		ctx.applyResponseDelay()

		cmd, resp := dispatchJunos(line, ctx.dev.Data, len(prompt))
		if ctx.emit(cmd, cmdStart, resp) {
			return
		}
		if resp.Close {
			return
		}
	}
}

// junosPrompt is "<user>@<hostname>> ", using the name the client logged in
// with and falling back to --username when the SSH layer did not supply one.
func junosPrompt(ctx *sessionCtx) string {
	user := ctx.loginUser
	if user == "" {
		user = ctx.username
	}
	return user + "@" + ctx.dev.Hostname + "> "
}

// dispatchJunos resolves one operational-mode line. promptLen positions the
// caret of the unknown-command error under the first character typed.
func dispatchJunos(line string, config []byte, promptLen int) (Command, Response) {
	fields := strings.Fields(strings.ToLower(line))
	if len(fields) == 0 {
		return CmdEmpty, Response{}
	}

	switch {
	case len(fields) == 4 && fields[0] == "set" && fields[1] == "cli" &&
		(fields[2] == "screen-length" || fields[2] == "screen-width"):
		return CmdJunosSetCli, Response{}

	case isJunosShowConfiguration(fields):
		return CmdJunosShowConfiguration, Response{ConfigOutput: config}

	case len(fields) == 1 && (fields[0] == "exit" || fields[0] == "quit"):
		return CmdExit, Response{Close: true}
	}

	pad := promptLen + len(line) - len(strings.TrimLeft(line, " \t"))
	return CmdUnknown, Response{Output: []byte(strings.Repeat(" ", pad) + "^\r\n" + junosUnknownMsg)}
}

// isJunosShowConfiguration matches "show configuration" optionally followed by
// "| display set" and/or "| no-more" pipes.
func isJunosShowConfiguration(fields []string) bool {
	segs := strings.Split(strings.Join(fields, " "), "|")
	if strings.TrimSpace(segs[0]) != "show configuration" {
		return false
	}
	for _, p := range segs[1:] {
		switch strings.TrimSpace(p) {
		case "display set", "no-more":
		default:
			return false
		}
	}
	return true
}
