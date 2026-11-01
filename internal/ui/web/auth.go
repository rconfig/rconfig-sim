package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// SessionCookie holds the session id after a successful login.
const SessionCookie = "rcfg_sim_ui_session"

// Login policy (section 8).
const (
	sessionTTL = 12 * time.Hour // sliding: every request extends it
	failWindow = 5 * time.Minute
	failLimit  = 5                // failures in the window before slowing
	failDelay  = 30 * time.Second // per attempt once slowed
)

// HashCost is the bcrypt cost rcfg-sim-ui hash-password uses.
const HashCost = 12

// AuthOptions configure the login.
type AuthOptions struct {
	Username     string
	PasswordHash string // bcrypt
	// Tests only: zero values mean the section 8 policy and the real clock.
	SessionTTL time.Duration
	FailDelay  time.Duration
	Now        func() time.Time
}

// Auth is the UI's one-user login: credentials from the env file and
// in-memory sessions, so restarting the UI logs everyone out.
type Auth struct {
	user      []byte
	hash      []byte
	ttl       time.Duration
	failDelay time.Duration
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time   // id -> expiry
	fails    map[string][]time.Time // source address -> recent failures
}

// CheckCredentials validates the UI_USERNAME and UI_PASSWORD_HASH values.
// The UI refuses to start unless both pass.
func CheckCredentials(username, hash string) error {
	if strings.TrimSpace(username) == "" {
		return errors.New("UI_USERNAME is not set")
	}
	if hash == "" {
		return errors.New("UI_PASSWORD_HASH is not set: generate one with rcfg-sim-ui hash-password")
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("UI_PASSWORD_HASH is not a valid bcrypt hash (%v): generate one with rcfg-sim-ui hash-password", err)
	}
	return nil
}

// HashPassword returns the bcrypt hash to put in UI_PASSWORD_HASH.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("empty password")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), HashCost)
	return string(b), err
}

// NewAuth returns the login for these credentials.
func NewAuth(o AuthOptions) (*Auth, error) {
	if err := CheckCredentials(o.Username, o.PasswordHash); err != nil {
		return nil, err
	}
	a := &Auth{
		user: []byte(o.Username), hash: []byte(o.PasswordHash),
		ttl: o.SessionTTL, failDelay: o.FailDelay, now: o.Now,
		sessions: map[string]time.Time{}, fails: map[string][]time.Time{},
	}
	if a.ttl <= 0 {
		a.ttl = sessionTTL
	}
	if a.failDelay <= 0 {
		a.failDelay = failDelay
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a, nil
}

// Username is the configured user, for the header.
func (a *Auth) Username() string { return string(a.user) }

// check compares both values, doing the bcrypt work whatever the username,
// so the time taken does not say which one was wrong.
func (a *Auth) check(username, password string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(username), a.user) == 1
	passOK := bcrypt.CompareHashAndPassword(a.hash, []byte(password)) == nil
	return userOK && passOK
}

// NewSession starts a session and returns its id: 32 random bytes, hex.
func (a *Auth) NewSession() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	id := hex.EncodeToString(b[:])
	a.mu.Lock()
	a.sessions[id] = a.now().Add(a.ttl)
	a.mu.Unlock()
	return id
}

// Valid reports whether id is a live session, and extends it.
func (a *Auth) Valid(id string) bool {
	if id == "" {
		return false
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[id]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(a.sessions, id)
		return false
	}
	a.sessions[id] = now.Add(a.ttl)
	return true
}

// End ends a session.
func (a *Auth) End(id string) {
	a.mu.Lock()
	delete(a.sessions, id)
	a.mu.Unlock()
}

// recentFailures prunes and counts an address's failures in the window.
func (a *Auth) recentFailures(addr string) int {
	cut := a.now().Add(-failWindow)
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.fails[addr][:0]
	for _, t := range a.fails[addr] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(a.fails, addr)
	} else {
		a.fails[addr] = kept
	}
	return len(kept)
}

func (a *Auth) recordFailure(addr string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fails[addr] = append(a.fails[addr], a.now())
	return len(a.fails[addr])
}

func (a *Auth) clearFailures(addr string) {
	a.mu.Lock()
	delete(a.fails, addr)
	a.mu.Unlock()
}

// sessionID returns the request's session cookie value.
func sessionID(r *http.Request) string {
	if c, err := r.Cookie(SessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// remoteAddr is the request's source address, without the port.
func remoteAddr(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// wait sleeps for d unless ctx ends first.
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// allowedHosts builds the Host header allow-list (section 8): the address
// the UI listens on, localhost, 127.0.0.1, and any extra names.
func allowedHosts(listen string, extra []string) map[string]bool {
	ok := map[string]bool{"localhost": true, "127.0.0.1": true}
	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" {
		if ip := net.ParseIP(host); ip == nil || !ip.IsUnspecified() {
			ok[strings.ToLower(host)] = true
		}
	}
	for _, h := range extra {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			ok[h] = true
		}
	}
	return ok
}

// hostAllowed checks the request's Host header against the allow-list,
// stopping DNS rebinding.
func (s *Server) hostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	return s.hosts[host]
}
