// Command rcfg-sim-ui is the web controller for a host's rcfg-sim fleet: it
// finds the instances from their env files, shows them, starts and stops
// them through systemd, and changes faults, profiles and capacity through
// each instance's admin API, saving every change to the env file.
// See docs/UI-ARCHITECTURE.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

func main() {
	var (
		addr       = flag.String("ui-addr", "127.0.0.1:8080", "listen address for the UI")
		uiToken    = flag.String("ui-token", "", "if set, required as a cookie (set via /login?token=) or Authorization: Bearer header; unset falls back to the UI_TOKEN environment variable")
		envDir     = flag.String("env-dir", "/etc/rcfg-sim", "where instance env files live; each *.env is one instance")
		adminToken = flag.String("admin-token", "", "token sent to instance admin APIs; unset falls back to the ADMIN_TOKEN environment variable")
		genBin     = flag.String("gen-bin", "/opt/rcfg-sim/bin/rcfg-sim-gen", "generator binary used by build jobs")
		dataDir    = flag.String("data-dir", "/var/lib/rcfg-sim-ui", "presets and job logs")
		poll       = flag.Duration("poll", 2*time.Second, "instance poll interval")
		systemctl  = flag.String("systemctl", "systemctl", `command used for unit control, e.g. "sudo -n systemctl" on hosts without polkit`)
		logLevel   = flag.String("log-level", "info", "log level: error|warn|info|debug")
	)
	flag.Parse()
	// Tokens are best set in the environment: a command line is visible in ps.
	if *uiToken == "" {
		*uiToken = os.Getenv("UI_TOKEN")
	}
	if *adminToken == "" {
		*adminToken = os.Getenv("ADMIN_TOKEN")
	}
	logger := makeLogger(*logLevel)

	if *poll < 500*time.Millisecond {
		fmt.Fprintln(os.Stderr, "rcfg-sim-ui: --poll must be at least 500ms")
		os.Exit(1)
	}
	if fi, err := os.Stat(*envDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: --env-dir %s: not a readable directory\n", *envDir)
		os.Exit(1)
	}

	units := systemd.New(*systemctl, nil)
	fl := fleet.New(fleet.Options{EnvDir: *envDir, AdminToken: *adminToken, Units: units, Logger: logger})
	srv := web.New(web.Options{
		Fleet:   fl,
		Units:   units,
		Presets: presets.NewStore(filepath.Join(*dataDir, "presets")),
		Token:   *uiToken,
		Poll:    *poll,
		Logger:  logger,
	})
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go fl.Run(ctx, *poll)

	if *uiToken == "" && !loopback(*addr) {
		logger.Warn("UI is on a non-loopback address with no UI token; anyone who can reach it can stop the fleet", "ui_addr", *addr)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: %v\n", err)
		os.Exit(1)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	logger.Info("rcfg-sim-ui ready",
		"ui_addr", l.Addr().String(), "env_dir", *envDir, "data_dir", *dataDir, "gen_bin", *genBin,
		"poll", poll.String(), "systemctl", *systemctl, "ui_token", *uiToken != "", "admin_token", *adminToken != "",
		"version", buildVersion())
	go func() {
		<-ctx.Done()
		srv.Close() // ends open Activity streams, which Shutdown would wait for
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	if err := hs.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: %v\n", err)
		os.Exit(1)
	}
	logger.Info("rcfg-sim-ui stopped")
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

// buildVersion returns the version without a leading "v": the -X value if
// set, else the module version, else "dev".
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
