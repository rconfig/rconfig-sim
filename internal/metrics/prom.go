package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// KnownCommands is the closed set of values that may appear as the `command`
// label on rcfgsim_command_duration_seconds. Strings match the Go identifier
// of the sshsrv.Command constants — dispatcher-side code maps Command→string.
// Keeping this list here lets the metrics package pre-register each series so
// /metrics always shows the full set, even at zero count.
var KnownCommands = []string{
	"CmdUnknown",
	"CmdEmpty",
	"CmdAmbiguous",
	"CmdTerminalLength",
	"CmdTerminalPager",
	"CmdEnable",
	"CmdShowVersion",
	"CmdShowRunningConfig",
	"CmdShowStartupConfig",
	"CmdShowInventory",
	"CmdExit",
}

// KnownFaultTypes is the closed set of values that may appear as the `type`
// label on rcfgsim_faults_injected_total. These match the CLI fault-type
// strings. Pre-registered at zero count; phase 6 wires increments.
var KnownFaultTypes = []string{
	"auth_fail",
	"disconnect_mid",
	"slow_response",
	"malformed",
}

// KnownSessionResults is the closed set of values for the `result` label on
// rcfgsim_sessions_total. Pre-registered at zero count.
var KnownSessionResults = []string{
	"ok",
	"auth_fail",
	"disconnect",
	"error",
}

// KnownAuthResults is the closed set of values for the `result` label on
// rcfgsim_auth_attempts_total.
var KnownAuthResults = []string{"ok", "fail"}

// Registry owns all simulator-specific Prometheus metrics plus the Go runtime
// collectors. Exactly one is constructed per rcfg-sim process.
type Registry struct {
	reg *prometheus.Registry

	ActiveSessions    prometheus.Gauge
	SessionsTotal     *prometheus.CounterVec
	SessionDuration   prometheus.Histogram
	CommandDuration   *prometheus.HistogramVec
	BytesSent         prometheus.Counter
	AuthAttempts      *prometheus.CounterVec
	HandshakeDuration prometheus.Histogram
	FaultsInjected    *prometheus.CounterVec
}

// New constructs a Registry with every metric created, every bounded label
// combination pre-initialised at zero, and the Go / process collectors added.
// Callers get a ready-to-Inc/Observe structure.
func New() *Registry {
	reg := prometheus.NewRegistry()
	r := &Registry{reg: reg}

	r.ActiveSessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rcfgsim_active_sessions",
		Help: "Number of SSH sessions currently open across all listeners.",
	})

	r.SessionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rcfgsim_sessions_total",
		Help: "Total SSH sessions closed, partitioned by terminal result.",
	}, []string{"result"})

	r.SessionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rcfgsim_session_duration_seconds",
		Help:    "End-to-end SSH session lifetime, from TCP accept to connection close.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	})

	r.CommandDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rcfgsim_command_duration_seconds",
		Help:    "Per-command dispatch+response latency, labeled by canonical Command name.",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"command"})

	r.BytesSent = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rcfgsim_bytes_sent_total",
		Help: "Total bytes written to SSH channels across all sessions.",
	})

	r.AuthAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rcfgsim_auth_attempts_total",
		Help: "SSH password-auth attempts, partitioned by result.",
	}, []string{"result"})

	r.HandshakeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "rcfgsim_handshake_duration_seconds",
		Help:    "Time from TCP accept to completed SSH handshake (success or failure).",
		Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5},
	})

	r.FaultsInjected = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rcfgsim_faults_injected_total",
		Help: "Faults deliberately injected, partitioned by type. Wired in phase 6.",
	}, []string{"type"})

	reg.MustRegister(
		r.ActiveSessions,
		r.SessionsTotal,
		r.SessionDuration,
		r.CommandDuration,
		r.BytesSent,
		r.AuthAttempts,
		r.HandshakeDuration,
		r.FaultsInjected,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// Pre-initialise every bounded label combination so /metrics exposes the
	// full series set at zero count from the first scrape. Prometheus can
	// then alert on absence rather than needing to wait for traffic.
	for _, cmd := range KnownCommands {
		r.CommandDuration.WithLabelValues(cmd)
	}
	for _, ft := range KnownFaultTypes {
		r.FaultsInjected.WithLabelValues(ft)
	}
	for _, sr := range KnownSessionResults {
		r.SessionsTotal.WithLabelValues(sr)
	}
	for _, ar := range KnownAuthResults {
		r.AuthAttempts.WithLabelValues(ar)
	}

	return r
}

// Gatherer returns the raw Prometheus gatherer, used by tests for parsing
// scraped output.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// Handler returns an http.Handler serving /metrics in the standard
// Prometheus exposition format.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		Registry: r.reg,
	})
}

// ObserveSeconds is a small sugar that records a time.Duration against an
// Observer. Zero-alloc — no *prometheus.Timer involved.
func ObserveSeconds(h prometheus.Observer, d time.Duration) {
	h.Observe(d.Seconds())
}
