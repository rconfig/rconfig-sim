package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/sshsrv"
)

func main() {
	cfg := sshsrv.Config{}
	var (
		manifestPath = flag.String("manifest", "/opt/rcfg-sim/manifest.csv", "path to the generator manifest (authoritative port→config mapping)")
		logLevel     = flag.String("log-level", "info", "log level: error|warn|info|debug")
		faultTypes   = flag.String("fault-types", "", "comma-separated fault types: auth_fail,disconnect_mid,slow_response,malformed")
		faultRate    = flag.Float64("fault-rate", 0.0, "probability [0,1] that an enabled fault fires per event")
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
	flag.IntVar(&cfg.MaxConcurrentSessions, "max-concurrent-sessions", 5000, "semaphore cap on concurrent sessions")
	flag.DurationVar(&cfg.MaxSessionDuration, "max-session", 5*time.Minute, "longest a session may last before the simulator closes it; a hang profile adds its hang")

	flag.Parse()

	cfg.ManifestPath = *manifestPath
	cfg.Logger = makeLogger(*logLevel)

	faults, err := fault.NewSet(*faultTypes, *faultRate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: %v\n", err)
		os.Exit(1)
	}
	cfg.Faults = faults
	if !faults.Empty() {
		types := make([]string, 0, 4)
		for _, t := range faults.EnabledTypes() {
			types = append(types, t.String())
		}
		cfg.Logger.Info("fault injection active", "rate", faults.Rate(), "types", strings.Join(types, ","))
	}

	rules, err := profile.Parse(*devProfiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: --device-profiles: %v\n", err)
		os.Exit(1)
	}
	cfg.DeviceProfiles = rules

	server, err := sshsrv.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim: %v\n", err)
		os.Exit(1)
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
