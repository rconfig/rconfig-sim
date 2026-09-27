package sshsrv

import (
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

func TestJunosPrompt(t *testing.T) {
	ctx := &sessionCtx{dev: &configs.Device{Hostname: "mx-01"}, username: "admin"}
	if got := junosPrompt(ctx); got != "admin@mx-01> " {
		t.Errorf("fallback prompt = %q", got)
	}
	ctx.loginUser = "netops"
	if got := junosPrompt(ctx); got != "netops@mx-01> " {
		t.Errorf("login-user prompt = %q", got)
	}
}

func TestJunosDispatch(t *testing.T) {
	cfg := []byte("system { host-name mx-01; }\n")

	for _, line := range []string{"set cli screen-length 0", "set cli screen-width 0"} {
		cmd, resp := dispatchJunos(line, cfg, 14)
		if cmd != CmdJunosSetCli || len(resp.Output) != 0 || len(resp.ConfigOutput) != 0 {
			t.Errorf("%q: cmd=%v resp=%+v, want silent CmdJunosSetCli", line, cmd, resp)
		}
	}

	for _, line := range []string{"show configuration", "show configuration | display set", "show configuration | display set | no-more"} {
		cmd, resp := dispatchJunos(line, cfg, 14)
		if cmd != CmdJunosShowConfiguration || string(resp.ConfigOutput) != string(cfg) {
			t.Errorf("%q: cmd=%v, want config bytes", line, cmd)
		}
	}

	if cmd, resp := dispatchJunos("exit", cfg, 14); cmd != CmdExit || !resp.Close {
		t.Errorf("exit: cmd=%v close=%v", cmd, resp.Close)
	}
}

func TestJunosUnknownCommand(t *testing.T) {
	// Prompt "admin@mx-01> " is 13 chars; caret sits under the first typed char.
	cmd, resp := dispatchJunos("show bogus", nil, 13)
	want := "             ^\r\nunknown command.\r\n"
	if cmd != CmdUnknown || string(resp.Output) != want {
		t.Errorf("unknown: cmd=%v out=%q, want %q", cmd, resp.Output, want)
	}

	_, resp = dispatchJunos("  foo", nil, 13)
	if want := "               ^\r\nunknown command.\r\n"; string(resp.Output) != want {
		t.Errorf("leading spaces: out=%q, want %q", resp.Output, want)
	}
}
