package inventory

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeDevice is a minimal SSH device: Cisco-style (a "host>" prompt, show
// version) or TL1 (a "<" prompt, ACT-USER, RTRV-SW-VER). mode "refuse"
// closes every connection at once; "hang" accepts and never answers.
func fakeDevice(t *testing.T, mode string, tl1 bool) string {
	t.Helper()
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "admin" && string(pw) == "secret" {
			return nil, nil
		}
		return nil, errDenied
	}, NoClientAuth: tl1}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			switch mode {
			case "refuse":
				c.Close()
				continue
			case "hang":
				t.Cleanup(func() { c.Close() })
				continue
			}
			go serveFake(c, cfg, tl1)
		}
	}()
	return l.Addr().String()
}

type denied struct{}

func (denied) Error() string { return "denied" }

var errDenied = denied{}

func serveFake(c net.Conn, cfg *ssh.ServerConfig, tl1 bool) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				r.Reply(r.Type == "pty-req" || r.Type == "shell", nil)
			}
		}()
		go func() {
			defer ch.Close()
			rd := bufio.NewReader(ch)
			if tl1 {
				ch.Write([]byte("\r\n<"))
				for {
					line, err := rd.ReadString(';')
					if err != nil {
						return
					}
					switch {
					case strings.Contains(line, `ACT-USER::admin:1::"secret"`):
						ch.Write([]byte("\r\n   SIM 26-11-01 09:00:00\r\nM  1 COMPLD\r\n;\r\n<"))
					case strings.Contains(line, "ACT-USER"):
						ch.Write([]byte("\r\n   SIM 26-11-01 09:00:00\r\nM  1 DENY\r\n   PICC\r\n;\r\n<"))
					case strings.Contains(line, "RTRV-SW-VER"):
						ch.Write([]byte("\r\n   SIM 26-11-01 09:00:00\r\nM  2 COMPLD\r\n   \"REL1100\"\r\n;\r\n<"))
					}
				}
			}
			ch.Write([]byte("\r\nsw-1>"))
			for {
				line, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "show version":
					ch.Write([]byte("show version\r\nCisco IOS Software, C2960X Software\r\nuptime is 3 weeks\r\nsw-1>"))
				case "exit":
					return
				}
			}
		}()
	}
}

func TestTestLogin(t *testing.T) {
	good := Device{Hostname: "sw-1", Template: "cisco_ios", Username: "admin", Password: "secret"}
	ctx := context.Background()

	res := TestLogin(ctx, fakeDevice(t, "ok", false), good, 5*time.Second)
	if !res.OK || res.Stage != "done" || res.LoginTime <= 0 || len(res.Output) < 1 || !strings.Contains(res.Output[0], "Cisco IOS") || res.Command != "show version" {
		t.Fatalf("healthy device: %+v", res)
	}

	bad := good
	bad.Password = "wrong"
	res = TestLogin(ctx, fakeDevice(t, "ok", false), bad, 5*time.Second)
	if res.OK || res.Stage != "ssh login" || res.Err == "" {
		t.Fatalf("auth failure: %+v", res)
	}
	if hint := Explain(res, map[string]float64{"auth_fail": 0.05}, ""); !strings.Contains(hint, "auth_fail fault (rate 5%)") {
		t.Fatalf("auth hint %q", hint)
	}

	res = TestLogin(ctx, fakeDevice(t, "refuse", false), good, 5*time.Second)
	if res.OK || res.Err == "" {
		t.Fatalf("refused: %+v", res)
	}
	if hint := Explain(res, nil, "refuse"); !strings.Contains(hint, "refuse profile") {
		t.Fatalf("refuse hint %q", hint)
	}

	start := time.Now()
	res = TestLogin(ctx, fakeDevice(t, "hang", false), good, 300*time.Millisecond)
	if res.OK || !strings.Contains(res.Err, "no answer within 300ms") || time.Since(start) > 3*time.Second {
		t.Fatalf("timeout: %+v after %s", res, time.Since(start))
	}

	tl1 := Device{Hostname: "ne-1", Template: "ciena_tl1", Username: "admin", Password: "secret"}
	res = TestLogin(ctx, fakeDevice(t, "ok", true), tl1, 5*time.Second)
	if !res.OK || res.Command != "RTRV-SW-VER:::2;" || !strings.Contains(strings.Join(res.Output, "\n"), "REL1100") {
		t.Fatalf("TL1: %+v", res)
	}
	tl1.Password = "wrong"
	res = TestLogin(ctx, fakeDevice(t, "ok", true), tl1, 5*time.Second)
	if res.OK || res.Stage != "TL1 login" || !strings.Contains(res.Err, "ACT-USER refused") {
		t.Fatalf("TL1 bad password: %+v", res)
	}
}
