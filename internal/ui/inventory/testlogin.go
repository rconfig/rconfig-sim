package inventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestTimeout bounds one Test login (Phase 7 item 5).
const TestTimeout = 10 * time.Second

// testCommands is the command each driver runs after login: the model's
// equivalent of "show version", from the commands its driver answers.
var testCommands = map[string]string{
	"cisco_ios":     "show version",
	"junos":         "show configuration | no-more",
	"ciena_tl1":     "RTRV-SW-VER:::2;",
	"infinera_tl1":  "RTRV-SW-VER:::2;",
	"cisco_ons_tl1": "RTRV-SW-VER:::2;",
}

// TestCommand is the command Test login runs on a device of this driver.
func TestCommand(template string) string {
	if c, ok := testCommands[template]; ok {
		return c
	}
	return testCommands["cisco_ios"]
}

func isTL1(template string) bool { return strings.HasSuffix(template, "_tl1") }

// LoginResult is what Test login found.
type LoginResult struct {
	OK        bool
	Stage     string        // where it stopped: connect, ssh login, TL1 login, command, or done
	LoginTime time.Duration // until the device was ready for a command (SSH, and ACT-USER for TL1)
	Total     time.Duration
	Command   string
	Output    []string // first lines of the command's output
	Err       string
}

// maxOutputLines is how much of the command's output the drawer shows.
const maxOutputLines = 12

// TestLogin opens one SSH session to addr as the device's manifest
// credentials, logs in the way its driver expects, runs TestCommand, and
// reports the timings and the first lines of output, or where it failed.
// It is a normal session, so the instance's faults apply.
func TestLogin(ctx context.Context, addr string, d Device, timeout time.Duration) LoginResult {
	if timeout <= 0 {
		timeout = TestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res := LoginResult{Command: TestCommand(d.Template), Stage: "connect"}
	fail := func(err error) LoginResult {
		res.Total = time.Since(start)
		if ctx.Err() != nil {
			err = fmt.Errorf("no answer within %s (%v)", timeout, err)
		}
		res.Err = err.Error()
		return res
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fail(err)
	}
	defer conn.Close()
	// Close the connection when the deadline passes, whatever is blocked.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	res.Stage = "ssh login"
	cfg := &ssh.ClientConfig{
		User: d.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(d.Password),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = d.Password
				}
				return ans, nil
			}),
		},
		// The simulator makes its own host keys; there is nothing to pin.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         timeout,
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return fail(err)
	}
	client := ssh.NewClient(c, chans, reqs)
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return fail(err)
	}
	defer sess.Close()
	_ = sess.RequestPty("vt100", 50, 200, ssh.TerminalModes{ssh.ECHO: 0})
	in, err := sess.StdinPipe()
	if err != nil {
		return fail(err)
	}
	out, err := sess.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	if err := sess.Shell(); err != nil {
		return fail(err)
	}
	rd := &reader{r: out}

	if isTL1(d.Template) {
		res.Stage = "TL1 login"
		if _, err := rd.until(func(b []byte) bool { return bytes.HasSuffix(bytes.TrimRight(b, " \r\n"), []byte("<")) }); err != nil {
			return fail(fmt.Errorf("no TL1 prompt: %w", err))
		}
		if _, err := io.WriteString(in, "ACT-USER::"+d.Username+":1::"+strconv.Quote(d.Password)+";\r\n"); err != nil {
			return fail(err)
		}
		reply, err := rd.until(tl1Done)
		if err != nil {
			return fail(fmt.Errorf("ACT-USER: %w", err))
		}
		if !bytes.Contains(reply, []byte("COMPLD")) {
			return fail(fmt.Errorf("ACT-USER refused: %s", firstLines(reply, 3, res.Command)))
		}
		res.LoginTime = time.Since(start)
		res.Stage = "command"
		if _, err := io.WriteString(in, res.Command+"\r\n"); err != nil {
			return fail(err)
		}
		reply, err = rd.until(tl1Done)
		res.Output = firstLines(reply, maxOutputLines, res.Command)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", res.Command, err))
		}
	} else {
		prompt := func(b []byte) bool {
			t := bytes.TrimRight(b, " ")
			return bytes.HasSuffix(t, []byte(">")) || bytes.HasSuffix(t, []byte("#"))
		}
		if _, err := rd.until(prompt); err != nil {
			return fail(fmt.Errorf("no prompt after login: %w", err))
		}
		res.LoginTime = time.Since(start)
		res.Stage = "command"
		if _, err := io.WriteString(in, res.Command+"\r\n"); err != nil {
			return fail(err)
		}
		reply, err := rd.until(prompt)
		res.Output = firstLines(reply, maxOutputLines, res.Command)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", res.Command, err))
		}
		_, _ = io.WriteString(in, "exit\r\n")
	}
	res.OK, res.Stage, res.Total = true, "done", time.Since(start)
	return res
}

// tl1Done reports a complete TL1 response: COMPLD or DENY, then the ";"
// terminator line.
func tl1Done(b []byte) bool {
	return (bytes.Contains(b, []byte("COMPLD")) || bytes.Contains(b, []byte("DENY"))) &&
		bytes.HasSuffix(bytes.TrimRight(b, " \r\n<"), []byte(";"))
}

// reader accumulates output until a predicate holds on what came since the
// last call.
type reader struct {
	r   io.Reader
	buf []byte
}

// maxRead caps what one wait collects.
const maxRead = 256 << 10

func (r *reader) until(done func([]byte) bool) ([]byte, error) {
	chunk := make([]byte, 4096)
	for !done(r.buf) {
		if len(r.buf) > maxRead {
			// Enough to show; stop reading a very large output.
			break
		}
		n, err := r.r.Read(chunk)
		r.buf = append(r.buf, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("the device closed the session")
			}
			out := r.buf
			r.buf = nil
			return out, err
		}
	}
	out := r.buf
	r.buf = nil
	return out, nil
}

// firstLines is up to n non-empty lines of b, without the echoed command.
func firstLines(b []byte, n int, cmd string) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimRight(l, "\r ")
		if strings.TrimSpace(l) == "" || strings.TrimSpace(l) == cmd {
			continue
		}
		out = append(out, l)
		if len(out) == n {
			break
		}
	}
	return out
}

// Explain says when a failed (or odd) result looks like the simulator
// misbehaving on purpose: an injected fault at the instance's rates, or the
// device's profile.
func Explain(res LoginResult, faults map[string]float64, prof string) string {
	kind, _, _ := strings.Cut(prof, ":")
	pct := func(t string) string { return strconv.FormatFloat(faults[t]*100, 'f', -1, 64) + "%" }
	switch {
	case kind == "refuse" && !res.OK:
		return "This device has the refuse profile: it turns sessions away on purpose."
	case kind == "hang" && !res.OK:
		return "This device has the hang profile: sessions hang on purpose."
	case kind == "disconnect" && !res.OK:
		return "This device has the disconnect profile: it drops sessions on purpose."
	case !res.OK && res.Stage == "ssh login" && faults["auth_fail"] > 0:
		return "Looks like an injected auth_fail fault (rate " + pct("auth_fail") + "). Try again."
	case !res.OK && res.Stage == "command" && faults["disconnect_mid"] > 0:
		return "Looks like an injected disconnect_mid fault (rate " + pct("disconnect_mid") + "). Try again."
	case !res.OK && strings.Contains(res.Err, "no answer within") && faults["slow_response"] > 0:
		return "May be an injected slow_response fault (rate " + pct("slow_response") + ")."
	case res.OK && faults["malformed"] > 0:
		return "Malformed output is injected at " + pct("malformed") + "; odd output may be on purpose."
	case kind == "latency":
		return "This device has the latency profile: slow answers are on purpose."
	}
	return ""
}
