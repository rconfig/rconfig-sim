package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
)

// syncBuffer is a log sink safe for the server's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// authEnv is a UI with no instances, for login and request-check tests.
func authEnv(t *testing.T, listen string, allowed []string) (*env, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	e := &env{t: t, dir: t.TempDir()}
	e.fleet = fleet.New(fleet.Options{EnvDir: e.dir})
	e.auth = testAuth(t)
	e.srv = New(Options{Fleet: e.fleet, Presets: presets.NewStore(t.TempDir()), Auth: e.auth,
		ListenAddr: listen, AllowedHosts: allowed, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	e.http = httptest.NewServer(e.srv)
	t.Cleanup(func() { e.srv.Close(); e.http.Close() })
	return e, logs
}

func loginForm(user, pass string) url.Values {
	return url.Values{"username": {user}, "password": {pass}}
}

func TestRoutesNeedALogin(t *testing.T) {
	e := newEnv(t, 1)
	for _, p := range []string{"/fleet", "/failures", "/activity", "/build", "/fleet/cards", "/"} {
		resp, _ := e.do(req{method: "GET", path: p, anon: true})
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("GET %s without a session: %d to %q, want 303 to /login", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// HTMX requests get 401 with HX-Redirect, so htmx goes to the login page.
	resp, _ := e.do(req{method: "GET", path: "/fleet/cards", anon: true, htmx: true})
	if resp.StatusCode != 401 || resp.Header.Get("HX-Redirect") != "/login" {
		t.Errorf("HTMX GET: %d, HX-Redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	resp, _ = e.do(req{method: "POST", path: "/fleet/delay", anon: true, htmx: true, form: url.Values{"delay_min": {"0"}, "delay_max": {"1"}}})
	if resp.StatusCode != 401 {
		t.Errorf("HTMX POST: %d", resp.StatusCode)
	}
	// SSE streams get 401.
	for _, p := range []string{"/activity/stream", "/build/jobs/x/stream"} {
		resp, _ = e.do(req{method: "GET", path: p, anon: true, header: map[string]string{"Accept": "text/event-stream"}})
		if resp.StatusCode != 401 {
			t.Errorf("SSE %s: %d", p, resp.StatusCode)
		}
	}
	// A made-up session is no session.
	resp, _ = e.do(req{method: "GET", path: "/fleet", session: "deadbeef"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("forged session: %d", resp.StatusCode)
	}
	// Public routes.
	for _, p := range []string{"/login", "/static/app.css", "/healthz"} {
		if resp, _ := e.do(req{method: "GET", path: p, anon: true}); resp.StatusCode != 200 {
			t.Errorf("GET %s without a session: %d", p, resp.StatusCode)
		}
	}
}

func TestLoginAndLogout(t *testing.T) {
	e, logs := authEnv(t, "127.0.0.1:8080", nil)
	page := e.get("/login")
	for _, want := range []string{`hx-post="/login"`, `name="username"`, `name="password"`, `type="password"`, "sim-logo-white.svg", "brand-hairline"} {
		if !strings.Contains(page, want) {
			t.Errorf("login page missing %q", want)
		}
	}

	resp, body := e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: loginForm("admin", testPassword)})
	if resp.StatusCode != 200 || resp.Header.Get("HX-Redirect") != "/fleet" {
		t.Fatalf("login: %d %q %s", resp.StatusCode, resp.Header.Get("HX-Redirect"), body)
	}
	var c *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == SessionCookie {
			c = ck
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || len(c.Value) != 64 {
		t.Fatalf("session cookie: %+v", c)
	}
	if resp, _ := e.do(req{method: "GET", path: "/fleet", session: c.Value}); resp.StatusCode != 200 {
		t.Fatalf("Fleet with the new session: %d", resp.StatusCode)
	}
	if resp, _ := e.do(req{method: "GET", path: "/login", session: c.Value}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/login when logged in should go to the fleet: %d", resp.StatusCode)
	}
	if _, b := e.do(req{method: "GET", path: "/fleet", session: c.Value}); !strings.Contains(b, `hx-post="/logout"`) || !strings.Contains(b, ">admin<") {
		t.Fatal("header must show the user and a Log out button")
	}

	resp, _ = e.do(req{method: "POST", path: "/logout", session: c.Value, htmx: true})
	if resp.StatusCode != 200 || resp.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("logout: %d %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	if resp, _ := e.do(req{method: "GET", path: "/fleet", session: c.Value}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("session must end at logout: %d", resp.StatusCode)
	}
	if strings.Contains(logs.String(), testPassword) {
		t.Fatal("the password reached the log")
	}
}

func TestFailedLoginIsGenericLoggedAndSlowed(t *testing.T) {
	e, logs := authEnv(t, "127.0.0.1:8080", nil)
	var msgs []string
	for _, f := range []url.Values{loginForm("admin", "wrong"), loginForm("nobody", testPassword)} {
		resp, body := e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: f})
		if resp.StatusCode != 200 || resp.Header.Get("HX-Redirect") != "" || len(resp.Cookies()) != 0 {
			t.Fatalf("a failed login must not log in: %d %v", resp.StatusCode, resp.Cookies())
		}
		msgs = append(msgs, body)
	}
	if msgs[0] != msgs[1] || !strings.Contains(msgs[0], "Wrong username or password.") {
		t.Fatalf("the message must not say which value was wrong: %q vs %q", msgs[0], msgs[1])
	}
	if !strings.Contains(logs.String(), `"msg":"login failed"`) || !strings.Contains(logs.String(), `"remote":"127.0.0.1"`) {
		t.Fatalf("failures must be logged with the source address:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "wrong") || strings.Contains(logs.String(), testPassword) {
		t.Fatal("a password reached the log")
	}

	// Three more failures make five in the window: not slowed yet.
	for i := 0; i < 3; i++ {
		start := time.Now()
		e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: loginForm("admin", "nope")})
		if time.Since(start) > 200*time.Millisecond {
			t.Fatalf("attempt %d slowed too early", i+3)
		}
	}
	// The sixth is slowed (the test delay is 300ms; the policy is 30s).
	start := time.Now()
	e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: loginForm("admin", "nope")})
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Fatalf("sixth failure not slowed: %v", d)
	}
	if !strings.Contains(logs.String(), "login slowed") {
		t.Fatal("slowing must be logged")
	}
	// A correct login is still slowed while the failures are recent, then
	// clears them.
	resp, _ := e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: loginForm("admin", testPassword)})
	if resp.Header.Get("HX-Redirect") != "/fleet" {
		t.Fatal("correct login refused")
	}
	start = time.Now()
	e.do(req{method: "POST", path: "/login", anon: true, htmx: true, form: loginForm("admin", "nope")})
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("a successful login must clear the failures")
	}
}

func TestSessionExpiryIsSliding(t *testing.T) {
	h, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	now := time.Unix(1_000_000, 0)
	a, err := NewAuth(AuthOptions{Username: "u", PasswordHash: string(h), SessionTTL: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	id := a.NewSession()
	now = now.Add(50 * time.Minute)
	if !a.Valid(id) {
		t.Fatal("valid inside the TTL")
	}
	now = now.Add(50 * time.Minute) // 100 minutes in, but used 50 minutes ago
	if !a.Valid(id) {
		t.Fatal("use must extend the session")
	}
	now = now.Add(61 * time.Minute)
	if a.Valid(id) {
		t.Fatal("idle past the TTL must expire")
	}
	if a.Valid("") || a.Valid("nope") {
		t.Fatal("empty and unknown ids are not sessions")
	}
}

func TestCheckCredentials(t *testing.T) {
	good, err := HashPassword("s3cret pass")
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := bcrypt.Cost([]byte(good)); c != HashCost {
		t.Fatalf("hash cost %d, want %d", c, HashCost)
	}
	if bcrypt.CompareHashAndPassword([]byte(good), []byte("s3cret pass")) != nil {
		t.Fatal("hash does not verify")
	}
	for desc, c := range map[string]struct{ user, hash, want string }{
		"no user":   {"", good, "UI_USERNAME"},
		"no hash":   {"admin", "", "UI_PASSWORD_HASH is not set"},
		"bad hash":  {"admin", "plaintext", "not a valid bcrypt"},
		"truncated": {"admin", good[:20], "not a valid bcrypt"},
	} {
		if err := CheckCredentials(c.user, c.hash); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", desc, c.want, err)
		}
	}
	if err := CheckCredentials("admin", good); err != nil {
		t.Fatal(err)
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatal("empty password must be refused")
	}
}

func TestHostHeaderCheck(t *testing.T) {
	e, _ := authEnv(t, "10.1.1.2:8080", []string{"sim-lab.example", " "})
	e.session = e.auth.NewSession()
	for host, want := range map[string]int{
		"10.1.1.2:8080":        200,
		"localhost:8080":       200,
		"127.0.0.1":            200,
		"SIM-LAB.example:8080": 200,
		"evil.example":         403,
		"10.1.1.3:8080":        403,
		"attacker.com:8080":    403,
	} {
		resp, _ := e.do(req{method: "GET", path: "/fleet", header: map[string]string{"Host": host}})
		if resp.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, resp.StatusCode, want)
		}
	}
	// Even the public routes.
	if resp, _ := e.do(req{method: "GET", path: "/login", anon: true, header: map[string]string{"Host": "evil.example"}}); resp.StatusCode != 403 {
		t.Errorf("login page with a foreign Host: %d", resp.StatusCode)
	}

	// A wildcard listen address adds nothing: name it with --ui-allowed-hosts.
	w, _ := authEnv(t, "0.0.0.0:8080", nil)
	if resp, _ := w.do(req{method: "GET", path: "/healthz", anon: true, header: map[string]string{"Host": "0.0.0.0:8080"}}); resp.StatusCode != 403 {
		t.Errorf("Host 0.0.0.0: %d", resp.StatusCode)
	}
}

func TestStateChangesNeedHXRequest(t *testing.T) {
	e := newEnv(t, 1)
	for _, p := range []string{"/fleet/delay", "/failures/apply", "/build/run", "/logout", "/login"} {
		resp, _ := e.do(req{method: "POST", path: p, form: url.Values{}})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without HX-Request: %d", p, resp.StatusCode)
		}
	}
}
