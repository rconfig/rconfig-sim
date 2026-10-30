package sshsrv

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	mrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/metrics"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

// Config is everything the Server needs to run. Only fields supplied by the CLI.
// Faults, DeviceProfiles, ProfileSeed, the response delays,
// MaxConcurrentSessions and ActiveDevices are only the starting values: the
// server copies them into a runtime.Live, which may be swapped later.
type Config struct {
	ListenIP       string
	PortStart      int
	PortCount      int
	ManifestPath   string
	HostKeyPath    string
	Username       string
	Password       string
	EnablePassword string
	// SSHAuthMode controls whether the SSH transport authenticates the client
	// before the interactive session starts:
	//   "password" (default) — every device requires SSH password auth.
	//   "driver"             — the device's driver decides (Cisco IOS yes, every TL1 driver no).
	//   "none"               — no SSH auth for any device; in-band auth only.
	// Empty is treated as "password" for backward compatibility.
	SSHAuthMode           string
	ResponseDelayMinMS    int
	ResponseDelayMaxMS    int
	MaxConcurrentSessions int
	MetricsAddr           string
	// CommandsRoot, when set, serves <root>/<hostname>/<slug>.txt as the
	// output of the matching command. Empty disables the feature.
	CommandsRoot string
	Faults       *fault.Set
	// DeviceProfiles assigns behaviour profiles to a share of the devices
	// (--device-profiles); ProfileSeed decides which devices (--profile-seed).
	// A device's manifest `profile` column overrides both.
	DeviceProfiles []profile.Rule
	ProfileSeed    int64
	// DeviceProfileRules is DeviceProfiles as given, one --device-profiles
	// item each (e.g. "hang:3600s@3%"). Reported by the admin API only.
	DeviceProfileRules []string
	// MaxSessionDuration caps a session's life (--max-session). Zero means
	// the 5-minute default. A hang profile extends it by the hang.
	MaxSessionDuration time.Duration
	// ActiveDevices is how many devices, in port order from PortStart, accept
	// sessions (--active-devices). The rest drop connections like the refuse
	// profile. Nil means every device.
	ActiveDevices *int
	Logger        *slog.Logger
}

// Server owns the TCP listeners, SSH handshake machinery, loaded devices,
// and the live runtime config. One instance per IP alias.
type Server struct {
	cfg       Config
	signer    ssh.Signer
	devices   map[int]*configs.Device
	allDevs   []*configs.Device
	logger    *slog.Logger
	listeners []net.Listener
	acceptWG  sync.WaitGroup
	sessionWG sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	totalMap  int64

	metrics     *metrics.Registry
	metricsHTTP *http.Server
	cmdFiles    *cmdFiles

	// runtime holds the swappable session config. Each session loads it
	// once and keeps that snapshot.
	runtime *runtime.Holder
	// sessions counts admitted sessions, checked against Live.MaxSessions.
	sessions atomic.Int64

	// Activity for the admin API: per-device state and the event stream.
	devTable  *activity.DeviceTable
	bus       *activity.Bus
	startedAt time.Time
	// mounts are extra handlers for the metrics mux, added by Mount.
	mounts []mount
}

type mount struct {
	pattern string
	handler http.Handler
}

// New loads host key + device configs and prepares the Server, but does NOT
// bind any ports yet. Call Start to begin listening.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	if cfg.MaxConcurrentSessions <= 0 {
		return nil, errors.New("max-concurrent-sessions must be > 0")
	}
	if cfg.PortCount <= 0 {
		return nil, errors.New("port-count must be > 0")
	}
	activeDevices := cfg.PortCount
	if cfg.ActiveDevices != nil {
		activeDevices = *cfg.ActiveDevices
		if activeDevices < 0 || activeDevices > cfg.PortCount {
			return nil, fmt.Errorf("active-devices %d out of range 0..%d (port-count)", activeDevices, cfg.PortCount)
		}
	}
	switch cfg.SSHAuthMode {
	case "", "password", "driver", "none":
	default:
		return nil, fmt.Errorf("invalid ssh-auth mode %q (want password|driver|none)", cfg.SSHAuthMode)
	}

	signer, err := loadOrGenerateHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}

	devs, err := configs.LoadForListener(cfg.ManifestPath, cfg.ListenIP, cfg.PortStart, cfg.PortCount)
	if err != nil {
		return nil, fmt.Errorf("load configs: %w", err)
	}
	if len(devs) == 0 {
		return nil, fmt.Errorf("no devices in manifest match %s ports %d..%d", cfg.ListenIP, cfg.PortStart, cfg.PortStart+cfg.PortCount-1)
	}

	devMap := make(map[int]*configs.Device, len(devs))
	var total int64
	for _, d := range devs {
		devMap[d.Port] = d
		total += d.Size
	}

	// Refuse to start on a manifest naming a driver we do not have, rather than silently
	// serving those devices as Cisco IOS.
	if bad := unknownDrivers(devs); len(bad) > 0 {
		return nil, fmt.Errorf("manifest names unknown driver(s) in the template column: %s", strings.Join(bad, ", "))
	}

	profiles, err := resolveProfiles(devs, cfg.DeviceProfiles, cfg.ProfileSeed)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{
		cfg:      cfg,
		signer:   signer,
		devices:  devMap,
		allDevs:  devs,
		logger:   cfg.Logger,
		ctx:      ctx,
		cancel:   cancel,
		totalMap: total,
		metrics:  metrics.New(),
		cmdFiles: newCmdFiles(cfg.CommandsRoot),
		runtime: runtime.NewHolder(&runtime.Live{
			Faults:        cfg.Faults,
			Profiles:      profiles,
			DelayMin:      time.Duration(cfg.ResponseDelayMinMS) * time.Millisecond,
			DelayMax:      time.Duration(cfg.ResponseDelayMaxMS) * time.Millisecond,
			MaxSessions:   int64(cfg.MaxConcurrentSessions),
			ActiveDevices: activeDevices,
			ProfileRules:  cfg.DeviceProfileRules,
			ProfileSeed:   cfg.ProfileSeed,
		}),
		devTable: activity.NewDeviceTable(cfg.PortCount),
		bus:      activity.NewBus(activity.DefaultRateLimit, activity.DefaultRingSize),
	}
	srv.logProfiles(devs, profiles)
	// Pre-register the command_duration label values every registered driver can
	// emit, so vendor-specific values (e.g. TL1) appear in /metrics at zero from
	// the first scrape — the same guarantee metrics.KnownCommands gives Cisco.
	for _, cmd := range registeredCommands() {
		srv.metrics.CommandDuration.WithLabelValues(cmd)
	}
	if srv.cmdFiles != nil {
		srv.metrics.CommandDuration.WithLabelValues(CmdFile.String())
	}
	return srv, nil
}

// ResolveProfiles builds the port-to-profile map for this server's devices
// from rules and seed, as New does at startup, for a new runtime.Live.
// Manifest-pinned profiles still win. Runs off the hot path.
func (s *Server) ResolveProfiles(rules []profile.Rule, seed int64) (map[int]*profile.Profile, error) {
	return resolveProfiles(s.allDevs, rules, seed)
}

// resolveProfiles gives each device its behaviour profile: the manifest
// `profile` column when set, otherwise whatever the rules assign its hostname.
func resolveProfiles(devs []*configs.Device, rules []profile.Rule, seed int64) (map[int]*profile.Profile, error) {
	out := map[int]*profile.Profile{}
	for _, d := range devs {
		p, err := profile.ParseProfile(d.Profile)
		if err != nil {
			return nil, fmt.Errorf("manifest device %s (port %d): %w", d.Hostname, d.Port, err)
		}
		if p == nil {
			p = profile.Assign(d.Hostname, seed, rules)
		}
		if p != nil {
			out[d.Port] = p
		}
	}
	return out, nil
}

// maxLoggedProfileHosts caps the hostnames logged per profile at startup.
const maxLoggedProfileHosts = 50

// logProfiles logs, per profile, how many devices have it and which ones, so
// an operator can tell which devices in rConfig should misbehave.
func (s *Server) logProfiles(devs []*configs.Device, profiles map[int]*profile.Profile) {
	if len(profiles) == 0 {
		return
	}
	hosts := map[string][]string{}
	var order []string
	for _, d := range devs {
		p, ok := profiles[d.Port]
		if !ok {
			continue
		}
		key := p.String()
		if _, seen := hosts[key]; !seen {
			order = append(order, key)
		}
		hosts[key] = append(hosts[key], d.Hostname)
	}
	for _, key := range order {
		names := hosts[key]
		logged := names
		if len(logged) > maxLoggedProfileHosts {
			logged = logged[:maxLoggedProfileHosts]
		}
		s.logger.Info("device profile active",
			"profile", key,
			"devices", len(names),
			"hostnames", strings.Join(logged, ","),
			"hostnames_truncated", len(names) > len(logged),
		)
	}
}

// Metrics returns the metrics registry — primarily for integration tests
// that need to read observed values without scraping the HTTP endpoint.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// Runtime returns the holder of the live session config. Store a new
// runtime.Live in it to change faults, profiles, delays, the session cap or
// the active device count; the change applies to new sessions only.
func (s *Server) Runtime() *runtime.Holder { return s.runtime }

// ActiveSessions returns the number of sessions admitted and not yet ended,
// the count checked against the session cap.
func (s *Server) ActiveSessions() int64 { return s.sessions.Load() }

// DeviceCount returns the number of devices loaded from the manifest.
func (s *Server) DeviceCount() int { return len(s.devices) }

// DeviceTable returns the per-device state table, indexed by port offset.
func (s *Server) DeviceTable() *activity.DeviceTable { return s.devTable }

// Bus returns the session event bus.
func (s *Server) Bus() *activity.Bus { return s.bus }

// StartedAt returns when Start was called; zero before that.
func (s *Server) StartedAt() time.Time { return s.startedAt }

// Done is closed when Shutdown begins. Long-lived handlers (event streams)
// should end on it so Shutdown is not held up.
func (s *Server) Done() <-chan struct{} { return s.ctx.Done() }

// Mount adds a handler to the metrics HTTP mux, alongside /metrics and
// /healthz. Call it before Start; it has no effect after. Does nothing when
// --metrics-addr is empty.
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mounts = append(s.mounts, mount{pattern, h})
}

// Start binds one listener per port and spawns the per-port accept goroutine.
// Also starts the metrics HTTP server (serving /metrics and /healthz) on the
// address supplied via --metrics-addr. Returns the first listen error
// encountered, after cleaning up any partially bound listeners.
func (s *Server) Start() error {
	s.startedAt = time.Now()
	for port, dev := range s.devices {
		l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.ListenIP, port))
		if err != nil {
			s.closeListeners()
			return fmt.Errorf("listen %s:%d: %w", s.cfg.ListenIP, port, err)
		}
		s.listeners = append(s.listeners, l)
		d := dev
		s.acceptWG.Add(1)
		go s.accept(l, d)
	}

	if s.cfg.MetricsAddr != "" {
		if err := s.startMetricsHTTP(); err != nil {
			s.closeListeners()
			return fmt.Errorf("metrics http: %w", err)
		}
	}

	s.logger.Info("rcfg-sim ready",
		"listen_ip", s.cfg.ListenIP,
		"port_start", s.cfg.PortStart,
		"port_count", s.cfg.PortCount,
		"devices_loaded", len(s.devices),
		"total_config_bytes", s.totalMap,
		"metrics_addr", s.cfg.MetricsAddr,
		"pid", os.Getpid(),
	)
	return nil
}

// startMetricsHTTP binds the metrics listener and serves /metrics and
// /healthz. Runs in its own goroutine; errors other than ErrServerClosed
// are logged but do not bring down the SSH simulator.
func (s *Server) startMetricsHTTP() error {
	l, err := net.Listen("tcp", s.cfg.MetricsAddr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.metrics.Handler())
	for _, m := range s.mounts {
		mux.Handle(m.pattern, m.handler)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.metricsHTTP = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := s.metricsHTTP.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("metrics http serve", "err", err)
		}
	}()
	return nil
}

func (s *Server) accept(l net.Listener, dev *configs.Device) {
	defer s.acceptWG.Done()
	for {
		conn, err := l.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			s.logger.Warn("accept error", "port", dev.Port, "err", err)
			return
		}
		s.sessionWG.Add(1)
		go func() {
			defer s.sessionWG.Done()
			s.handleConn(conn, dev)
		}()
	}
}

// Shutdown stops accepting new connections, waits up to `timeout` for in-flight
// sessions to drain, then unmaps all config files. Also stops the metrics HTTP
// server on a short independent deadline.
func (s *Server) Shutdown(timeout time.Duration) {
	s.cancel()
	s.closeListeners()
	s.acceptWG.Wait()

	done := make(chan struct{})
	go func() {
		s.sessionWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.logger.Info("all sessions drained cleanly")
	case <-time.After(timeout):
		s.logger.Warn("shutdown timed out waiting for sessions", "timeout", timeout)
	}

	if s.metricsHTTP != nil {
		mctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.metricsHTTP.Shutdown(mctx)
		cancel()
	}

	configs.UnloadAll(s.allDevs)
	s.logger.Info("rcfg-sim stopped")
}

func (s *Server) closeListeners() {
	for _, l := range s.listeners {
		_ = l.Close()
	}
}

// requireSSHAuth decides whether the SSH transport must authenticate this
// device's client before the interactive session starts, per the configured
// SSHAuthMode. Empty mode is treated as "password".
func (s *Server) requireSSHAuth(dev *configs.Device) bool {
	switch s.cfg.SSHAuthMode {
	case "none":
		return false
	case "driver":
		return driverFor(dev.Driver).RequiresSSHAuth()
	default: // "" or "password"
		return true
	}
}

func (s *Server) handleConn(conn net.Conn, dev *configs.Device) {
	defer conn.Close()

	// One RNG per connection, used for every fault decision in this session.
	// Seeded from nanosecond clock XOR a crypto-random draw, per spec.
	rng := newSessionRand()

	sessionStart := time.Now()
	outcome := &sessionOutcome{result: "ok", start: sessionStart}
	idx := dev.Port - s.cfg.PortStart
	var admitted, inactive bool
	defer func() {
		s.metrics.SessionDuration.Observe(time.Since(sessionStart).Seconds())
		result := outcome.Get()
		s.metrics.SessionsTotal.WithLabelValues(result).Inc()
		s.endActivity(dev, idx, admitted, inactive, outcome, result)
	}()

	// One snapshot of the runtime config for the whole session, so a swap
	// changes new sessions only and never alters one in flight.
	live := s.runtime.Load()
	prof := live.Profiles[dev.Port]

	// An inactive device (--active-devices) and the refuse profile both
	// accept TCP and drop it before the SSH handshake, as a box whose SSH
	// daemon is dead or rate limiting.
	inactive = !s.deviceActive(live, dev.Port)
	if inactive || prof.Is(profile.KindRefuse) {
		outcome.Set("error")
		hardCloseTCP(conn)
		return
	}

	if !s.acquireSession(live) {
		s.logger.Warn("session cap hit; refusing connection",
			"port", dev.Port, "remote", conn.RemoteAddr().String())
		outcome.Set("error")
		return
	}
	defer s.releaseSession()
	admitted = true
	s.devTable.Start(idx)
	if s.bus.Active() {
		s.bus.Publish(sessionEvent(activity.KindSessionStart, dev, outcome, time.Now()))
	}

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second)) // handshake deadline

	serverConfig := &ssh.ServerConfig{}
	if s.requireSSHAuth(dev) {
		serverConfig.PasswordCallback = func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			// auth_fail fault fires BEFORE password check so the rejection
			// is indistinguishable from a real wrong-password case to the
			// client. Increment both auth_attempts{fail} and faults_injected.
			if live.Faults.Roll(rng, fault.TypeAuthFail) {
				noteFault(s.metrics, s.bus, dev, outcome, fault.TypeAuthFail)
				s.metrics.AuthAttempts.WithLabelValues("fail").Inc()
				return nil, errors.New("invalid password")
			}
			if s.cfg.Password == "" || string(pass) == s.cfg.Password {
				s.metrics.AuthAttempts.WithLabelValues("ok").Inc()
				return nil, nil
			}
			s.metrics.AuthAttempts.WithLabelValues("fail").Inc()
			return nil, errors.New("invalid password")
		}
	} else {
		// No SSH-layer auth: the client connects unchallenged and authenticates
		// in-band (e.g. Ciena TL1 ACT-USER). The auth_fail fault and
		// auth_attempts metric do not apply in this mode.
		serverConfig.NoClientAuth = true
	}
	serverConfig.AddHostKey(s.signer)

	handshakeStart := time.Now()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, serverConfig)
	s.metrics.HandshakeDuration.Observe(time.Since(handshakeStart).Seconds())
	if err != nil {
		s.logger.Debug("handshake failed", "port", dev.Port, "remote", conn.RemoteAddr().String(), "err", err)
		// NewServerConn's error string starts with "ssh: ..." for every failure mode
		// we hit in practice; for auth rejection specifically the underlying callback
		// error is wrapped as "ssh: no auth passed yet" / similar. We look at
		// metrics.AuthAttempts elsewhere, but for session-level classification the
		// heuristic below is sufficient.
		if isAuthFailure(err) {
			outcome.Set("auth_fail")
		} else {
			outcome.Set("error")
		}
		return
	}
	defer sshConn.Close()

	// Clear handshake deadline; replace with per-read deadlines inside the shell.
	_ = conn.SetDeadline(time.Time{})

	s.metrics.ActiveSessions.Inc()
	defer s.metrics.ActiveSessions.Dec()

	// Cap on a session's life. A hang profile extends it by the hang, or the
	// cap would cut every hang longer than the cap short.
	maxSession := s.cfg.MaxSessionDuration
	if maxSession <= 0 {
		maxSession = 5 * time.Minute
	}
	hardTimer := time.AfterFunc(maxSession+prof.HangDuration(), func() { _ = sshConn.Close() })
	defer hardTimer.Stop()

	connDone := make(chan struct{})
	defer close(connDone)

	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		ch, reqs, err := newCh.Accept()
		if err != nil {
			s.logger.Warn("channel accept failed", "port", dev.Port, "err", err)
			outcome.Set("error")
			continue
		}
		go s.handleSession(ch, reqs, dev, sshConn.User(), outcome, conn, rng, live, prof, connDone)
	}
}

// deviceActive reports whether the device on port accepts sessions under
// live: its index from PortStart must be below ActiveDevices.
func (s *Server) deviceActive(live *runtime.Live, port int) bool {
	return port-s.cfg.PortStart < live.ActiveDevices
}

// acquireSession admits one session against live.MaxSessions. On true the
// caller must call releaseSession when the session ends.
func (s *Server) acquireSession(live *runtime.Live) bool {
	if s.sessions.Add(1) > live.MaxSessions {
		s.sessions.Add(-1)
		return false
	}
	return true
}

func (s *Server) releaseSession() { s.sessions.Add(-1) }

// endActivity records a finished connection in the device table and, when
// anyone is watching, publishes its session_end event. admitted means the
// session passed admission (and DeviceTable.Start ran); inactive means it
// was dropped because the device is past --active-devices, which the device
// table reports from the live count rather than storing.
func (s *Server) endActivity(dev *configs.Device, idx int, admitted, inactive bool, o *sessionOutcome, result string) {
	now := time.Now()
	st := activity.StateForOutcome(result)
	if o.hung.Load() {
		st = activity.StateHang
	}
	switch {
	case admitted:
		s.devTable.End(idx, st, now)
	case inactive:
		s.devTable.Touch(idx, now)
	default:
		s.devTable.Set(idx, st, now)
	}
	if s.bus.Active() {
		ev := sessionEvent(activity.KindSessionEnd, dev, o, now)
		ev.Outcome = result
		ev.Fault = o.lastFault()
		s.bus.Publish(ev)
	}
}

// sessionEvent builds an activity event for dev's session at now. o may be
// nil (dispatch-only unit tests).
func sessionEvent(kind string, dev *configs.Device, o *sessionOutcome, now time.Time) activity.Event {
	ev := activity.Event{T: now.UnixMilli(), Host: dev.Hostname, Port: dev.Port, Kind: kind}
	if o != nil {
		ev.Bytes = o.bytes.Load()
		ev.MS = now.Sub(o.start).Milliseconds()
	}
	return ev
}

// noteFault records an injected fault: the faults_injected metric, the
// session's last fault, and a fault event when anyone is watching. m, bus
// and o may be nil.
func noteFault(m *metrics.Registry, bus *activity.Bus, dev *configs.Device, o *sessionOutcome, t fault.Type) {
	if m != nil {
		m.FaultsInjected.WithLabelValues(t.String()).Inc()
	}
	if o != nil {
		o.setFault(t.String())
	}
	if bus.Active() && dev != nil {
		ev := sessionEvent(activity.KindFault, dev, o, time.Now())
		ev.Fault = t.String()
		bus.Publish(ev)
	}
}

// sessionOutcome tracks the terminal result string for the rcfgsim_sessions_total
// counter, plus what the activity data needs at session end. Shared between
// handleConn and any session goroutines it spawns.
// "ok" is the default; any non-"ok" value sticks (first-wins on error paths).
type sessionOutcome struct {
	mu     sync.Mutex
	result string
	fault  string // last injected fault type, "" if none

	start time.Time    // when the connection was accepted; set once
	bytes atomic.Int64 // bytes sent on the session's channels
	hung  atomic.Bool  // a hang profile hung this session
}

func (o *sessionOutcome) setFault(f string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fault = f
}

func (o *sessionOutcome) lastFault() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.fault
}

func (o *sessionOutcome) Set(r string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.result == "ok" {
		o.result = r
	}
}

func (o *sessionOutcome) Get() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.result == "" {
		return "ok"
	}
	return o.result
}

// isAuthFailure recognises ssh handshake errors caused by password rejection.
// We intentionally use string matching rather than unwrapping — x/crypto/ssh
// doesn't export a sentinel for this, and the message format is stable.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg, "no auth passed", "unable to authenticate", "permission denied")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, dev *configs.Device, loginUser string, outcome *sessionOutcome, rawConn net.Conn, rng *mrand.Rand, live *runtime.Live, prof *profile.Profile, connDone <-chan struct{}) {
	shellStarted := false
	ctx := &sessionCtx{
		ch:             ch,
		dev:            dev,
		driver:         driverFor(dev.Driver),
		username:       s.cfg.Username,
		loginUser:      loginUser,
		password:       s.cfg.Password,
		enablePassword: s.cfg.EnablePassword,
		delayMinMS:     int(live.DelayMin.Milliseconds()),
		delayMaxMS:     int(live.DelayMax.Milliseconds()),
		logger:         s.logger,
		rng:            rng,
		metrics:        s.metrics,
		outcome:        outcome,
		faults:         live.Faults,
		events:         s.bus,
		rawConn:        rawConn,
		cmdFiles:       s.cmdFiles,
		profile:        prof,
		connDone:       connDone,
		serverDone:     s.ctx.Done(),
	}
	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			if !shellStarted {
				shellStarted = true
				go func() {
					ctx.driver.Serve(ctx)
					_ = ch.Close()
				}()
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// newSessionRand seeds a per-session PRNG used today only for response-delay
// jitter. Fault injection (phase 6) will share the same pattern.
func newSessionRand() *mrand.Rand {
	var seed int64
	bi, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err == nil {
		seed = bi.Int64()
	} else {
		seed = time.Now().UnixNano()
	}
	return mrand.New(mrand.NewSource(seed))
}

// loadOrGenerateHostKey returns an ssh.Signer. If path exists, it's parsed;
// otherwise a fresh 2048-bit RSA key is generated, persisted at mode 0600,
// and returned.
func loadOrGenerateHostKey(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return ssh.ParsePrivateKey(data)
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate RSA key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir host key dir: %w", err)
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	return ssh.ParsePrivateKey(pemBytes)
}
