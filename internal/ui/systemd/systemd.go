// Package systemd starts, stops and restarts rcfg-sim@<IP>.service units by
// running systemctl. The command is configurable (--systemctl), so a host
// without polkit can use "sudo -n systemctl", and tests can fake it.
package systemd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a command and returns its combined output. Tests replace it.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs the command with os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Ctl controls rcfg-sim instance units.
type Ctl struct {
	cmd  []string // systemctl plus any leading words ("sudo -n systemctl")
	run  Runner
	wait time.Duration // per-command timeout
}

// New returns a Ctl that runs command (split on spaces, e.g. "systemctl" or
// "sudo -n systemctl") through run. A nil run uses ExecRunner.
func New(command string, run Runner) *Ctl {
	if run == nil {
		run = ExecRunner
	}
	cmd := strings.Fields(command)
	if len(cmd) == 0 {
		cmd = []string{"systemctl"}
	}
	return &Ctl{cmd: cmd, run: run, wait: 60 * time.Second}
}

// Unit returns the unit name for an instance, e.g. rcfg-sim@10.50.0.1.service.
// The instance must be an IP address, so nothing else can be named.
func Unit(instance string) (string, error) {
	if net.ParseIP(instance) == nil {
		return "", fmt.Errorf("instance %q is not an IP address", instance)
	}
	return "rcfg-sim@" + instance + ".service", nil
}

// Verbs the controller may run.
const (
	Start   = "start"
	Stop    = "stop"
	Restart = "restart"
)

// Do runs systemctl verb on the instance's unit.
func (c *Ctl) Do(ctx context.Context, verb, instance string) error {
	switch verb {
	case Start, Stop, Restart:
	default:
		return fmt.Errorf("unsupported verb %q", verb)
	}
	unit, err := Unit(instance)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.wait)
	defer cancel()
	out, err := c.run(ctx, c.cmd[0], append(c.cmd[1:], verb, unit)...)
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", verb, unit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsActive returns the unit's state as systemctl is-active prints it
// ("active", "inactive", "failed", "activating", ...). is-active exits
// non-zero for any state but active, so only a missing state is an error.
func (c *Ctl) IsActive(ctx context.Context, instance string) (string, error) {
	unit, err := Unit(instance)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.run(ctx, c.cmd[0], append(c.cmd[1:], "is-active", unit)...)
	state := strings.TrimSpace(string(out))
	if i := strings.IndexByte(state, '\n'); i >= 0 {
		state = state[:i]
	}
	if state == "" {
		if err == nil {
			err = errors.New("empty output")
		}
		return "", fmt.Errorf("systemctl is-active %s: %w", unit, err)
	}
	return state, nil
}

// RollingStep reports one instance's progress through a rolling restart.
type RollingStep struct {
	Instance string
	Phase    string // "restarting", "waiting", "ok", "failed"
	Err      error
}

// RollingRestart restarts the instances one at a time. After each restart it
// calls healthy until it returns nil or wait passes, then pauses before the
// next. It stops at the first instance that fails to restart or come back,
// so a bad config cannot take down the whole fleet. progress, if not nil,
// is called at every step.
func (c *Ctl) RollingRestart(ctx context.Context, instances []string, wait, pause time.Duration,
	healthy func(ctx context.Context, instance string) error, progress func(RollingStep)) error {
	report := func(s RollingStep) {
		if progress != nil {
			progress(s)
		}
	}
	for i, inst := range instances {
		report(RollingStep{Instance: inst, Phase: "restarting"})
		if err := c.Do(ctx, Restart, inst); err != nil {
			report(RollingStep{Instance: inst, Phase: "failed", Err: err})
			return err
		}
		report(RollingStep{Instance: inst, Phase: "waiting"})
		if err := waitHealthy(ctx, inst, wait, healthy); err != nil {
			report(RollingStep{Instance: inst, Phase: "failed", Err: err})
			return err
		}
		report(RollingStep{Instance: inst, Phase: "ok"})
		if i < len(instances)-1 && pause > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	return nil
}

func waitHealthy(ctx context.Context, inst string, wait time.Duration, healthy func(context.Context, string) error) error {
	deadline := time.Now().Add(wait)
	for {
		err := healthy(ctx, inst)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy after %s: %w", inst, wait, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
