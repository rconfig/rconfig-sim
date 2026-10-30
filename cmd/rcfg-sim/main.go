package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

// version is set at build time with -ldflags "-X main.version=..." (see the
// Makefile). Empty falls back to the module version from the build info.
var version string

func main() {
	cfg := sshsrv.Config{}
	var (
		manifestPath = flag.String("manifest", "/opt/rcfg-sim/manifest.csv", "path to the generator manifest (authoritative port→config mapping)")
		logLevel     = flag.String("log-level", "info", "log level: error|warn|info|debug")
		faultTypes   = flag.String("fault-types", "", "comma-separated fault types: auth_fail,disconnect_mid,slow_response,malformed")
		faultRate    = flag.Float64("fault-rate", 0.0, "probability [0,1] that an enabled fault fires per event")
		faultRates   = flag.String("fault-rates", "", "per-type fault probabilities [0,1]: type=rate,... e.g. auth_fail=0.02,disconnect_mid=0.005 (replaces --fault-rate/--fault-types; setting both is an error)")
		adminOn      = flag.Bool("admin", false, "serve the admin API (/admin/v1/*) on --metrics-addr")
		adminToken   = flag.String("admin-token", "", "if set, admin API requests must send Authorization: Bearer <token>")
		activeDevs   = flag.String("active-devices", "all", "how many devices, in port order from --port-start, accept sessions: all, or 0..port-count; the rest drop connections like the refuse profile")
		devProfiles  = flag.String("device-profiles", "", "per-device behaviour for a share of the fleet: profile@percent,... e.g. hang:3600s@3%,latency:200-800ms@10% (profiles: hang:<duration>, latency:<min>-<max>, refuse, disconnect)")
	)
	flag.StringVar(&cfg.MetricsAddr, "metrics-addr", "0.0.0.0:9100", "Prometheus metrics listen address (empty = disable)")
	flag.StringVar(&cfg.ListenIP, "listen-ip", "10.50.0.1", "IP address to bind")
	flag.IntVar(&cfg.PortStart, "port-start", 10000, "first port in contiguous range")
	flag.IntVar(&cfg.PortCount, "port-count", 2500, "number of ports to bind")
	flag.StringVar(&cfg.HostKeyPath, "host-key", "/etc/rcfg-sim/ssh_host_rsa_key", "path to SSH host key (generated if missing)")
	flag.StringVar(&cfg.Username, "username", "admin", "accepted SSH username (currently informational; auth is password-only)")
	flag.StringVar(&cfg.Password, "password", "admin", "accepted SSH password (empty = accept any)")
	flag.StringVar(&cfg.EnablePassword, "enable-password", "enable123", "enable-mode password")
	flag.StringVar(&cfg.SSHAuthMode, "ssh-auth", "password", "SSH transport auth: password (all devices) | driver (per-driver: Cisco IOS yes, TL1 drivers no) | none (in-band auth only)")
	flag.IntVar(&cfg.ResponseDelayMinMS, "response-delay-ms-min", 50, "min per-command response delay (ms)")
	flag.IntVar(&cfg.ResponseDelayMaxMS, "response-delay-ms-max", 500, "max per-command response delay (ms)")
	flag.StringVar(&cfg.CommandsRoot, "commands-root", "", "directory of per-device command output files, DIR/<hostname>/<slug>.txt (empty = disabled)")
	flag.Int64Var(&cfg.ProfileSeed, "profile-seed", 42, "seed deciding which devices --device-profiles picks; same seed, same devices")
	flag.IntVar(&cfg.MaxConcurrentSessions, "max-concurrent-sessions", 5000, "cap on concurrent sessions")
	flag.DurationVar(&cfg.MaxSessionDuration, "max-session", 5*time.Minute, "longest a session may last before the simulator closes it; a hang profile adds its hang")

	flag.Parse()

	cfg.ManifestPath = *manifestPath
	cfg.Logger = makeLogger(*logLevel)

	faults, err := parseFaults(*faultRates, *faultTypes, *faultRate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: %v\n", err)
		os.Exit(1)
	}
	cfg.Faults = faults
	if !faults.Empty() {
		cfg.Logger.Info("fault injection active", "rates", faults.String())
	}

	if *activeDevs != "all" {
		n, err := strconv.Atoi(*activeDevs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rcfg-sim: --active-devices %q: want all or a number\n", *activeDevs)
			os.Exit(1)
		}
		cfg.ActiveDevices = &n
	}

	rules, err := profile.Parse(*devProfiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: --device-profiles: %v\n", err)
		os.Exit(1)
	}
	cfg.DeviceProfiles = rules
	cfg.DeviceProfileRules = profileItems(*devProfiles)

	server, err := sshsrv.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: %v\n", err)
		os.Exit(1)
	}
	if *adminOn {
		if cfg.MetricsAddr == "" {
			fmt.Fprintln(os.Stderr, "rcfg-sim: --admin needs --metrics-addr")
			os.Exit(1)
		}
		server.Mount(admin.Prefix, admin.Handler(server, admin.Options{
			Version:   buildVersion(),
			ListenIP:  cfg.ListenIP,
			PortStart: cfg.PortStart,
			PortCount: cfg.PortCount,
			Devices:   server.DeviceCount(),
			Token:     *adminToken,
			Logger:    cfg.Logger,
		}))
		if *adminToken == "" && !loopback(cfg.MetricsAddr) {
			cfg.Logger.Warn("admin API is on a non-loopback address with no --admin-token; anyone who can reach it can change faults and capacity", "metrics_addr", cfg.MetricsAddr)
		}
		cfg.Logger.Info("admin API enabled", "addr", cfg.MetricsAddr, "prefix", admin.Prefix, "token", *adminToken != "")
	} else if *adminToken != "" {
		cfg.Logger.Warn("--admin-token is set but --admin is not; the admin API is off")
	}

	if err := server.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: %v\n", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	cfg.Logger.Info("shutdown requested")
	server.Shutdown(30 * time.Second)
}

// profileItems splits a --device-profiles spec into its items, as given,
// for reporting.
func profileItems(spec string) []string {
	var out []string
	for _, item := range strings.Split(spec, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// buildVersion returns the version without a leading "v": the -X value if
// set, else the module version (set by go install), else "dev".
func buildVersion() string {
	v := version
	if v == "" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "(devel)" {
			v = bi.Main.Version
		}
	}
	if v == "" {
		return "dev"
	}
	return strings.TrimPrefix(v, "v")
}

// loopback reports whether addr (host:port) binds only a loopback address.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// parseFaults builds the fault Set from --fault-rates, or from the legacy
// --fault-types and --fault-rate pair. Setting --fault-rates together with a
// non-zero --fault-rate or non-empty --fault-types is an error, so the
// systemd unit can pass all three with the legacy pair at its defaults.
func parseFaults(rates, types string, rate float64) (*fault.Set, error) {
	if strings.TrimSpace(rates) == "" {
		return fault.NewSet(types, rate)
	}
	if rate != 0 || strings.TrimSpace(types) != "" {
		return nil, fmt.Errorf("--fault-rates cannot be combined with --fault-rate or --fault-types")
	}
	return fault.ParseRates(rates)
}

func makeLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
