package systemd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a fake Runner that records calls and answers from a table.
type recorder struct {
	mu    sync.Mutex
	calls []string
	out   map[string]string // joined args -> output
	fail  map[string]bool
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	var err error
	if r.fail[call] {
		err = errors.New("exit status 3")
	}
	return []byte(r.out[call]), err
}

func TestDo(t *testing.T) {
	r := &recorder{}
	c := New("sudo -n systemctl", r.run)
	if err := c.Do(context.Background(), Restart, "10.50.0.1"); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "sudo -n systemctl restart rcfg-sim@10.50.0.1.service" {
		t.Fatalf("call: %q", r.calls[0])
	}
	for _, bad := range []struct{ verb, inst string }{
		{"enable", "10.50.0.1"}, {Start, "10.50.0.1; rm -rf /"}, {Start, "rtr"}, {Stop, ""},
	} {
		if err := c.Do(context.Background(), bad.verb, bad.inst); err == nil {
			t.Errorf("%s %q: want an error", bad.verb, bad.inst)
		}
	}
	if len(r.calls) != 1 {
		t.Fatalf("refused calls must not run anything: %v", r.calls)
	}
}

func TestDoFailureIncludesOutput(t *testing.T) {
	r := &recorder{
		out:  map[string]string{"systemctl stop rcfg-sim@10.50.0.1.service": "Access denied"},
		fail: map[string]bool{"systemctl stop rcfg-sim@10.50.0.1.service": true},
	}
	err := New("systemctl", r.run).Do(context.Background(), Stop, "10.50.0.1")
	if err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("want the systemctl output in the error, got %v", err)
	}
}

func TestIsActive(t *testing.T) {
	r := &recorder{
		out: map[string]string{
			"systemctl is-active rcfg-sim@10.50.0.1.service": "active\n",
			"systemctl is-active rcfg-sim@10.50.0.2.service": "failed\n",
		},
		fail: map[string]bool{"systemctl is-active rcfg-sim@10.50.0.2.service": true},
	}
	c := New("", r.run)
	if s, err := c.IsActive(context.Background(), "10.50.0.1"); err != nil || s != "active" {
		t.Fatalf("active: %q %v", s, err)
	}
	if s, err := c.IsActive(context.Background(), "10.50.0.2"); err != nil || s != "failed" {
		t.Fatalf("failed unit: non-zero exit is still a state: %q %v", s, err)
	}
	if _, err := c.IsActive(context.Background(), "10.50.0.3"); err == nil {
		t.Fatal("empty output must be an error")
	}
}

func TestRollingRestart(t *testing.T) {
	r := &recorder{}
	c := New("systemctl", r.run)
	healthyAfter := map[string]int{"10.50.0.1": 0, "10.50.0.2": 2}
	var mu sync.Mutex
	checks := map[string]int{}
	healthy := func(_ context.Context, inst string) error {
		mu.Lock()
		defer mu.Unlock()
		checks[inst]++
		if checks[inst] > healthyAfter[inst] {
			return nil
		}
		return errors.New("not yet")
	}
	var steps []string
	err := c.RollingRestart(context.Background(), []string{"10.50.0.1", "10.50.0.2"}, 5*time.Second, time.Millisecond, healthy,
		func(s RollingStep) { steps = append(steps, s.Instance+":"+s.Phase) })
	if err != nil {
		t.Fatal(err)
	}
	want := "10.50.0.1:restarting 10.50.0.1:waiting 10.50.0.1:ok 10.50.0.2:restarting 10.50.0.2:waiting 10.50.0.2:ok"
	if strings.Join(steps, " ") != want {
		t.Fatalf("steps:\n got %v\nwant %s", steps, want)
	}
	if len(r.calls) != 2 || !strings.HasSuffix(r.calls[1], "restart rcfg-sim@10.50.0.2.service") {
		t.Fatalf("calls: %v", r.calls)
	}
}

func TestRollingRestartStopsAtFirstUnhealthy(t *testing.T) {
	r := &recorder{}
	c := New("systemctl", r.run)
	never := func(context.Context, string) error { return errors.New("down") }
	err := c.RollingRestart(context.Background(), []string{"10.50.0.1", "10.50.0.2"}, 300*time.Millisecond, 0, never, nil)
	if err == nil || !strings.Contains(err.Error(), "10.50.0.1 not healthy") {
		t.Fatalf("want the first instance to fail, got %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("must not restart the second instance: %v", r.calls)
	}
}
