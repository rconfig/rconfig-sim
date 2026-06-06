//go:build integration

package sshsrv_test

// Characterization tests that pin the exact observable Cisco-IOS session
// behaviour BEFORE the driver refactor: greeting bytes, the enable-mode
// password sub-flow (happy + sad), and clean session close on `exit`. These
// must keep passing byte-for-byte after the ciscoIOS driver extraction — they
// are the regression gate for that move.

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// charServer brings up a one-device Cisco server on loopback with the default
// (password) SSH auth mode and returns the SSH port, hostname, and server.
func charServer(t *testing.T) (port int, hostname string, srv *sshsrv.Server) {
	return charServerMode(t, "")
}

// charServerMode is charServer with an explicit --ssh-auth mode.
func charServerMode(t *testing.T, authMode string) (port int, hostname string, srv *sshsrv.Server) {
	t.Helper()
	tmp := t.TempDir()
	manifest := filepath.Join(tmp, "manifest.csv")
	configsDir := filepath.Join(tmp, "configs")
	sshPort := freePort(t)

	if _, err := configs.Run(configs.Config{
		Count: 1, OutputDir: configsDir, ManifestPath: manifest,
		IPBase: "127.0.0.1", IPCount: 1, PortStart: sshPort, DevicesPerIP: 1,
		Seed: 17, Distribution: "sm:100,md:0,lg:0,xl:0",
		Username: "admin", Password: "admin", EnablePassword: "enable123",
	}, io.Discard); err != nil {
		t.Fatalf("generator: %v", err)
	}
	hostname = manifestHostname(t, manifest, sshPort)

	var err error
	srv, err = sshsrv.New(sshsrv.Config{
		ListenIP: "127.0.0.1", PortStart: sshPort, PortCount: 1,
		ManifestPath: manifest, HostKeyPath: filepath.Join(tmp, "host"),
		Username: "admin", Password: "admin", EnablePassword: "enable123",
		SSHAuthMode:        authMode,
		ResponseDelayMinMS: 0, ResponseDelayMaxMS: 0,
		MaxConcurrentSessions: 4,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Shutdown(5 * time.Second) })
	return sshPort, hostname, srv
}

func manifestHostname(t *testing.T, manifest string, port int) string {
	t.Helper()
	f, err := os.Open(manifest)
	if err != nil {
		t.Fatalf("open manifest: %v", err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for i, row := range rows {
		if i == 0 {
			continue
		}
		if p, _ := strconv.Atoi(row[2]); p == port {
			return row[0]
		}
	}
	t.Fatalf("no manifest row for port %d", port)
	return ""
}

// expectConn is an interactive SSH session that buffers everything the server
// sends so a test can wait for a sentinel and assert on the bytes seen so far.
type expectConn struct {
	t      *testing.T
	client *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	mu     sync.Mutex
	buf    bytes.Buffer
	waitMu sync.Mutex
}

func dialExpect(t *testing.T, port int, user, pass string) *expectConn {
	t.Helper()
	return dialExpectCfg(t, port, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
}

// dialExpectNoAuth connects offering no auth methods — the client succeeds only
// if the server accepts the "none" method (NoClientAuth), modelling a TL1-only
// device where the SSH transport does not challenge for a password.
func dialExpectNoAuth(t *testing.T, port int) *expectConn {
	t.Helper()
	return dialExpectCfg(t, port, &ssh.ClientConfig{
		User:            "anyone",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
}

func dialExpectCfg(t *testing.T, port int, clientCfg *ssh.ClientConfig) *expectConn {
	t.Helper()
	c, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), clientCfg)
	if err != nil {
		t.Fatalf("ssh dial port %d: %v", port, err)
	}
	sess, err := c.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	// ECHO:0 so the client terminal layer does not echo; the server's own
	// readLine echo is what we observe.
	_ = sess.RequestPty("xterm", 80, 24, ssh.TerminalModes{ssh.ECHO: 0})
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	ec := &expectConn{t: t, client: c, sess: sess, stdin: stdin}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := stdout.Read(b)
			if n > 0 {
				ec.mu.Lock()
				ec.buf.Write(b[:n])
				ec.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return ec
}

// expect polls the buffer until it contains sub, then returns everything
// accumulated so far. Fails the test on timeout.
func (ec *expectConn) expect(sub string, timeout time.Duration) string {
	ec.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ec.mu.Lock()
		s := ec.buf.String()
		ec.mu.Unlock()
		if bytes.Contains([]byte(s), []byte(sub)) {
			return s
		}
		if time.Now().After(deadline) {
			ec.t.Fatalf("expect %q timed out; buffer so far: %q", sub, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (ec *expectConn) snapshot() string {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return ec.buf.String()
}

func (ec *expectConn) reset() {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.buf.Reset()
}

func (ec *expectConn) send(line string) {
	fmt.Fprintf(ec.stdin, "%s\n", line)
}

func (ec *expectConn) close() {
	_ = ec.stdin.Close()
	_ = ec.sess.Close()
	_ = ec.client.Close()
}

// TestChar_GreetingBytes pins the exact greeting + first prompt the server
// emits before any input. No echo noise is possible here because nothing has
// been sent yet.
func TestChar_GreetingBytes(t *testing.T) {
	port, host, _ := charServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	want := "\r\n" + host + " line 0 is now available\r\n\r\n" + host + ">"
	ec.expect(host+">", 3*time.Second)
	if got := ec.snapshot(); got != want {
		t.Errorf("greeting bytes:\n got %q\nwant %q", got, want)
	}
}

// TestChar_EnableHappyPath pins the enable-mode sub-flow: `enable` -> the
// "Password: " prompt -> correct password -> prompt flips '>' to '#'.
func TestChar_EnableHappyPath(t *testing.T) {
	port, host, _ := charServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	ec.expect(host+">", 3*time.Second)
	ec.reset()
	ec.send("enable")
	ec.expect("Password: ", 3*time.Second)
	ec.reset()
	ec.send("enable123")
	got := ec.expect(host+"#", 3*time.Second)
	if bytes.Contains([]byte(got), []byte("% Access denied")) {
		t.Errorf("enable happy path unexpectedly denied: %q", got)
	}
}

// TestChar_EnableSadPath pins rejection: wrong enable password yields the
// "% Access denied" line and the prompt stays at user-exec '>'.
func TestChar_EnableSadPath(t *testing.T) {
	port, host, _ := charServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	ec.expect(host+">", 3*time.Second)
	ec.reset()
	ec.send("enable")
	ec.expect("Password: ", 3*time.Second)
	ec.reset()
	ec.send("wrongpw")
	got := ec.expect("% Access denied", 3*time.Second)
	if bytes.Contains([]byte(got), []byte(host+"#")) {
		t.Errorf("enable sad path unexpectedly entered enable mode: %q", got)
	}
	// Prompt returns to user-exec.
	ec.expect(host+">", 3*time.Second)
}

// TestChar_SessionCloseOnExit pins that `exit` at user-exec closes the channel
// cleanly (sess.Wait returns) and the session is counted as result=ok.
func TestChar_SessionCloseOnExit(t *testing.T) {
	port, host, srv := charServer(t)
	ec := dialExpect(t, port, "admin", "admin")
	defer ec.close()

	ec.expect(host+">", 3*time.Second)

	before := testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("ok"))
	ec.send("exit")

	done := make(chan error, 1)
	go func() { done <- ec.sess.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not close after exit")
	}

	// sessions_total is recorded when the connection tears down (handleConn's
	// defer), not on channel close — close the client so that fires, then poll.
	_ = ec.client.Close()
	after := before
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		after = testutil.ToFloat64(srv.Metrics().SessionsTotal.WithLabelValues("ok"))
		if after > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after <= before {
		t.Errorf("rcfgsim_sessions_total{result=ok}: want increment, before=%v after=%v", before, after)
	}
}
