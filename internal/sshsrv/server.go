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
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/metrics"
)

// Config is everything the Server needs to run. Only fields supplied by the CLI.
type Config struct {
	ListenIP              string
	PortStart             int
	PortCount             int
	ManifestPath          string
	HostKeyPath           string
	Username              string
	Password              string
	EnablePassword        string
	ResponseDelayMinMS    int
	ResponseDelayMaxMS    int
	MaxConcurrentSessions int
	MetricsAddr           string
	Faults                *fault.Set
	Logger                *slog.Logger
}

// Server owns the TCP listeners, SSH handshake machinery, loaded devices,
// and the session-concurrency semaphore. One instance per IP alias.
type Server struct {
	cfg       Config
	signer    ssh.Signer
	devices   map[int]*configs.Device
	allDevs   []*configs.Device
	logger    *slog.Logger
	sem       chan struct{}
	listeners []net.Listener
	acceptWG  sync.WaitGroup
	sessionWG sync.WaitGroup
	ctx       context.Context
	cancel    context.CancelFunc
	totalMap  int64

	metrics     *metrics.Registry
	metricsHTTP *http.Server
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

	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg:      cfg,
		signer:   signer,
		devices:  devMap,
		allDevs:  devs,
		logger:   cfg.Logger,
		sem:      make(chan struct{}, cfg.MaxConcurrentSessions),
		ctx:      ctx,
		cancel:   cancel,
		totalMap: total,
		metrics:  metrics.New(),
	}, nil
}

// Metrics returns the metrics registry — primarily for integration tests
// that need to read observed values without scraping the HTTP endpoint.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

// Start binds one listener per port and spawns the per-port accept goroutine.
// Also starts the metrics HTTP server (serving /metrics and /healthz) on the
// address supplied via --metrics-addr. Returns the first listen error
// encountered, after cleaning up any partially bound listeners.
func (s *Server) Start() error {
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

func (s *Server) handleConn(conn net.Conn, dev *configs.Device) {
	defer conn.Close()

	// One RNG per connection, used for every fault decision in this session.
	// Seeded from nanosecond clock XOR a crypto-random draw, per spec.
	rng := newSessionRand()

	sessionStart := time.Now()
	outcome := &sessionOutcome{result: "ok"}
	defer func() {
		s.metrics.SessionDuration.Observe(time.Since(sessionStart).Seconds())
		s.metrics.SessionsTotal.WithLabelValues(outcome.Get()).Inc()
	}()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.logger.Warn("session cap hit; refusing connection",
			"port", dev.Port, "remote", conn.RemoteAddr().String())
		outcome.Set("error")
		return
	}

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second)) // handshake deadline

	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			// auth_fail fault fires BEFORE password check so the rejection
			// is indistinguishable from a real wrong-password case to the
			// client. Increment both auth_attempts{fail} and faults_injected.
			if s.cfg.Faults.Roll(rng, fault.TypeAuthFail) {
				s.metrics.FaultsInjected.WithLabelValues(fault.TypeAuthFail.String()).Inc()
				s.metrics.AuthAttempts.WithLabelValues("fail").Inc()
				return nil, errors.New("invalid password")
			}
			if s.cfg.Password == "" || string(pass) == s.cfg.Password {
				s.metrics.AuthAttempts.WithLabelValues("ok").Inc()
				return nil, nil
			}
			s.metrics.AuthAttempts.WithLabelValues("fail").Inc()
			return nil, errors.New("invalid password")
		},
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

	hardTimer := time.AfterFunc(5*time.Minute, func() { _ = sshConn.Close() })
	defer hardTimer.Stop()

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
		go s.handleSession(ch, reqs, dev, outcome, conn, rng)
	}
}

// sessionOutcome tracks the terminal result string for the rcfgsim_sessions_total
// counter. Shared between handleConn and any session goroutines it spawns.
// "ok" is the default; any non-"ok" value sticks (first-wins on error paths).
type sessionOutcome struct {
	mu     sync.Mutex
	result string
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

func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request, dev *configs.Device, outcome *sessionOutcome, rawConn net.Conn, rng *mrand.Rand) {
	shellStarted := false
	ctx := &sessionCtx{
		ch:             ch,
		dev:            dev,
		enablePassword: s.cfg.EnablePassword,
		delayMinMS:     s.cfg.ResponseDelayMinMS,
		delayMaxMS:     s.cfg.ResponseDelayMaxMS,
		logger:         s.logger,
		rng:            rng,
		metrics:        s.metrics,
		outcome:        outcome,
		faults:         s.cfg.Faults,
		rawConn:        rawConn,
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
					runShell(ctx)
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
