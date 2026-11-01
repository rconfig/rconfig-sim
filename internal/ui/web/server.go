// Package web is rcfg-sim-ui's HTTP side: the Fleet and Failures screens
// (templ + htmx), the login, and the embedded static assets.
package web

import (
	"context"
	"embed"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

//go:embed static
var staticFS embed.FS

// csp allows nothing but this origin: no CDN, no inline script, no eval.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// Options configure the UI server.
type Options struct {
	Fleet   *fleet.Fleet
	Units   *systemd.Ctl
	Presets *presets.Store
	Jobs    *jobs.Runner // build jobs; nil disables the Build screen
	Auth    *Auth        // the login; required
	// ListenAddr and AllowedHosts make the Host header allow-list
	// (section 8): the listen address, localhost, 127.0.0.1 and these.
	ListenAddr   string
	AllowedHosts []string
	Poll         time.Duration
	Logger       *slog.Logger
	// RollingWait bounds how long a rolling restart waits for each instance's
	// /healthz. Zero means 60s.
	RollingWait time.Duration
	// FrameEvery is how often the Activity hub assembles a fleet frame
	// from /admin/v1/devices. Zero means 2s.
	FrameEvery time.Duration
}

// Server serves the UI.
type Server struct {
	opt   Options
	mux   *http.ServeMux
	hub   *Hub
	hosts map[string]bool

	ctx    context.Context // ends background work (rolling restarts) on Close
	cancel context.CancelFunc

	rollMu  sync.Mutex
	rolling views.RollingView
}

// New returns the UI server. opt.Auth is required: there is no mode
// without a login.
func New(opt Options) *Server {
	if opt.Auth == nil {
		panic("web.New: Options.Auth is required")
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opt.Poll <= 0 {
		opt.Poll = 2 * time.Second
	}
	if opt.RollingWait <= 0 {
		opt.RollingWait = 60 * time.Second
	}
	if opt.FrameEvery <= 0 {
		opt.FrameEvery = 2 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opt: opt, mux: http.NewServeMux(), ctx: ctx, cancel: cancel,
		hub:   NewHub(opt.Fleet, opt.Logger, opt.FrameEvery),
		hosts: allowedHosts(opt.ListenAddr, opt.AllowedHosts)}
	s.routes()
	return s
}

// Hub returns the Activity hub.
func (s *Server) Hub() *Hub { return s.hub }

// Close stops background work, including any open Activity streams.
func (s *Server) Close() { s.cancel() }

func (s *Server) routes() {
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(static)))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	s.mux.HandleFunc("GET /login", s.loginPage)
	s.mux.HandleFunc("POST /login", s.login)
	s.mux.HandleFunc("POST /logout", s.logout)
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/fleet", http.StatusSeeOther) })

	s.mux.HandleFunc("GET /fleet", s.fleetPage)
	s.mux.HandleFunc("GET /fleet/cards", s.fleetCards)
	s.mux.HandleFunc("POST /fleet/instances/{name}/reapply", s.reapply)
	s.mux.HandleFunc("POST /fleet/instances/{name}/{verb}", s.unitAction)
	s.mux.HandleFunc("POST /fleet/all/{verb}", s.unitActionAll)
	s.mux.HandleFunc("POST /fleet/rolling-restart", s.rollingRestart)
	s.mux.HandleFunc("POST /fleet/active", s.applyActive)
	s.mux.HandleFunc("POST /fleet/delay", s.applyDelay)
	s.mux.HandleFunc("POST /fleet/caps", s.applyCaps)
	s.mux.HandleFunc("POST /pending/active", s.pendingActive)
	s.mux.HandleFunc("POST /pending/delay", s.pendingDelay)
	s.mux.HandleFunc("POST /pending/caps", s.pendingCaps)

	s.mux.HandleFunc("GET /activity", s.activityPage)
	s.mux.HandleFunc("GET /activity/stream", s.activityStream)
	s.mux.HandleFunc("GET /activity/hosts", s.activityHosts)
	s.mux.HandleFunc("GET /activity/device", s.activityDevice)

	s.mux.HandleFunc("GET /build", s.buildPage)
	s.mux.HandleFunc("POST /build/estimate", s.buildEstimate)
	s.mux.HandleFunc("POST /build/run", s.buildRun)
	s.mux.HandleFunc("POST /build/rollback", s.buildRollback)
	s.mux.HandleFunc("GET /build/jobs/{id}/stream", s.buildJobStream)

	s.mux.HandleFunc("GET /failures", s.failuresPage)
	s.mux.HandleFunc("GET /failures/rule-row", s.ruleRow)
	s.mux.HandleFunc("GET /failures/presets", s.presetList)
	s.mux.HandleFunc("POST /failures/apply", s.applyFailures)
	s.mux.HandleFunc("POST /failures/affected", s.affected)
	s.mux.HandleFunc("POST /pending/failures", s.pendingFailures)
	s.mux.HandleFunc("POST /presets/save", s.savePreset)
	s.mux.HandleFunc("POST /presets/apply", s.applyPreset)
	s.mux.HandleFunc("POST /presets/delete", s.deletePreset)
}

// ServeHTTP applies, in order (section 8): security headers, the Host
// check, the cross-site check, then the login, then the route.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")

	// DNS rebinding: only our own names.
	if !s.hostAllowed(r) {
		http.Error(w, "unknown Host header; add the name with --ui-allowed-hosts", http.StatusForbidden)
		return
	}
	// Cross-site requests: every state change comes from htmx, which sends
	// HX-Request. A cross-site form post cannot set that header.
	if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("HX-Request") != "true" {
		http.Error(w, "state-changing requests must come from the UI (missing HX-Request header)", http.StatusForbidden)
		return
	}
	public := strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/login" || r.URL.Path == "/healthz"
	if !public && !s.opt.Auth.Valid(sessionID(r)) {
		switch {
		case r.Header.Get("HX-Request") == "true" || strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
			// htmx follows HX-Redirect; an EventSource just stops.
			w.Header().Set("HX-Redirect", "/login")
			http.Error(w, "login required", http.StatusUnauthorized)
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		default:
			http.Error(w, "login required", http.StatusUnauthorized)
		}
		return
	}
	s.mux.ServeHTTP(w, r)
}

// loginPage shows the login form, or goes to the fleet when already in.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.Auth.Valid(sessionID(r)) {
		http.Redirect(w, r, "/fleet", http.StatusSeeOther)
		return
	}
	render(w, r, http.StatusOK, views.LoginPage())
}

// login checks the form. After failLimit failures from one address inside
// failWindow, each further attempt from it waits failDelay first. The
// answer never says which value was wrong, and the password is never
// logged.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	a, addr := s.opt.Auth, remoteAddr(r)
	if a.recentFailures(addr) >= failLimit {
		s.opt.Logger.Warn("login slowed after repeated failures", "remote", addr, "delay", a.failDelay.String())
		if wait(r.Context(), a.failDelay) != nil {
			return
		}
	}
	if !a.check(r.FormValue("username"), r.FormValue("password")) {
		n := a.recordFailure(addr)
		s.opt.Logger.Warn("login failed", "remote", addr, "failures_in_window", n)
		render(w, r, http.StatusOK, views.LoginError("Wrong username or password."))
		return
	}
	a.clearFailures(addr)
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookie, Value: a.NewSession(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	s.opt.Logger.Info("login", "remote", addr)
	w.Header().Set("HX-Redirect", "/fleet")
	w.WriteHeader(http.StatusOK)
}

// logout ends the session and returns to the login form.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.opt.Auth.End(sessionID(r))
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("HX-Redirect", "/login")
	w.WriteHeader(http.StatusOK)
}

// staticHandler serves the embedded assets with a short cache lifetime and
// without directory listings.
func staticHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") || strings.HasPrefix(r.URL.Path, "src/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		files.ServeHTTP(w, r)
	})
}

// render writes a component as HTML.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}

// page builds the layout model from the last poll.
func (s *Server) page(title, nav string) views.Page {
	vs := s.opt.Fleet.Views()
	up := 0
	for _, v := range vs {
		if v.Status == fleet.StatusUp {
			up++
		}
	}
	return views.Page{Title: title, Nav: nav, Up: up, Total: len(vs), User: s.opt.Auth.Username()}
}

// flash answers an action with result messages, empties the pending bar and
// tells the page the fleet changed so the cards refresh.
func (s *Server) flash(w http.ResponseWriter, r *http.Request, changed bool, fs ...views.Flash) {
	if changed {
		w.Header().Set("HX-Trigger", "fleet-changed")
	}
	render(w, r, http.StatusOK, templ.Join(views.Flashes(fs), views.ClearPending()))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg string) {
	render(w, r, http.StatusOK, views.Flashes([]views.Flash{{Kind: "error", Text: msg}}))
}
