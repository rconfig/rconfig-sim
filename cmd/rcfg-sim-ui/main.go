// Command rcfg-sim-ui is the web controller for a host's rcfg-sim fleet: it
// finds the instances from their env files, shows them, starts and stops
// them through systemd, and changes faults, profiles and capacity through
// each instance's admin API, saving every change to the env file.
// See docs/UI-ARCHITECTURE.md.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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

	"golang.org/x/term"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		if err := hashPasswordCmd(os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "rcfg-sim-ui hash-password: %v\n", err)
			os.Exit(1)
		}
		return
	}
	var (
		addr         = flag.String("ui-addr", "127.0.0.1:8080", "listen address for the UI")
		allowedHosts = flag.String("ui-allowed-hosts", "", "extra Host header names accepted, comma-separated (for example a lab DNS name); the listen address, localhost and 127.0.0.1 always are")
		envDir       = flag.String("env-dir", "/etc/rcfg-sim", "where instance env files live; each *.env is one instance")
		genBin       = flag.String("gen-bin", "/opt/rcfg-sim/bin/rcfg-sim-gen", "generator binary used by build jobs")
		dataDir      = flag.String("data-dir", "/var/lib/rcfg-sim-ui", "presets, job logs")
		poll         = flag.Duration("poll", 2*time.Second, "instance poll interval")
		systemctl    = flag.String("systemctl", "systemctl", `command used for unit control, e.g. "sudo -n systemctl" on hosts without polkit`)
		logLevel     = flag.String("log-level", "info", "log level: error|warn|info|debug")
	)
	flag.Usage = usage
	if err := checkRemovedFlags(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: %v\n", err)
		os.Exit(2)
	}
	flag.Parse()
	logger := makeLogger(*logLevel)

	// The login (section 8): credentials only from the environment (the
	// unit's EnvironmentFile), never flags. No credentials, no UI.
	auth, err := web.NewAuth(web.AuthOptions{Username: os.Getenv("UI_USERNAME"), PasswordHash: os.Getenv("UI_PASSWORD_HASH")})
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: %v\n", err)
		os.Exit(1)
	}

	if *poll < 500*time.Millisecond {
		fmt.Fprintln(os.Stderr, "rcfg-sim-ui: --poll must be at least 500ms")
		os.Exit(1)
	}
	if fi, err := os.Stat(*envDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: --env-dir %s: not a readable directory\n", *envDir)
		os.Exit(1)
	}

	units := systemd.New(*systemctl, nil)
	fl := fleet.New(fleet.Options{EnvDir: *envDir, Units: units, Logger: logger})
	runner := jobs.NewRunner(jobs.Options{
		EnvDir:  *envDir,
		GenBin:  *genBin,
		LogDir:  filepath.Join(*dataDir, "jobs"),
		Units:   units,
		Healthy: fl.Healthy,
		Logger:  logger,
	})
	defer runner.Close()
	srv := web.New(web.Options{
		Fleet:        fl,
		Units:        units,
		Jobs:         runner,
		Presets:      presets.NewStore(filepath.Join(*dataDir, "presets")),
		Auth:         auth,
		ListenAddr:   *addr,
		AllowedHosts: strings.Split(*allowedHosts, ","),
		Poll:         *poll,
		Logger:       logger,
	})
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go fl.Run(ctx, *poll)

	if !loopback(*addr) {
		logger.Warn("plain HTTP: use an SSH tunnel or a private network", "ui_addr", *addr)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: %v\n", err)
		os.Exit(1)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	logger.Info("rcfg-sim-ui ready",
		"ui_addr", l.Addr().String(), "env_dir", *envDir, "data_dir", *dataDir, "gen_bin", *genBin,
		"poll", poll.String(), "systemctl", *systemctl, "user", auth.Username(), "version", buildVersion())
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

func usage() {
	fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  rcfg-sim-ui [flags]          run the UI (needs UI_USERNAME and UI_PASSWORD_HASH in the environment)
  rcfg-sim-ui hash-password    read a password on stdin, print its bcrypt hash for UI_PASSWORD_HASH

Flags:
`)
	flag.PrintDefaults()
}

// removedFlags are flags taken out of rcfg-sim-ui (Phase 6, D9).
var removedFlags = map[string]string{
	"ui-token":    "--ui-token was removed: the UI has a login now. Set UI_USERNAME and UI_PASSWORD_HASH (from rcfg-sim-ui hash-password) in the UI env file and drop UI_TOKEN",
	"admin-token": "--admin-token was removed: the admin API has no authentication. Drop ADMIN_TOKEN from the env files",
}

// checkRemovedFlags turns a removed flag into a clear error instead of
// the flag package's "flag provided but not defined".
func checkRemovedFlags(args []string) error {
	for _, a := range args {
		if a == "--" {
			return nil
		}
		if !strings.HasPrefix(a, "-") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if msg, ok := removedFlags[name]; ok {
			return errors.New(msg)
		}
	}
	return nil
}

// hashPasswordCmd reads a password and prints its bcrypt hash. From a
// terminal it prompts twice without echo; otherwise it reads one line.
func hashPasswordCmd(in *os.File, out, prompt io.Writer) error {
	var pw string
	if term.IsTerminal(int(in.Fd())) {
		fmt.Fprint(prompt, "Password: ")
		a, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return err
		}
		fmt.Fprint(prompt, "Again: ")
		b, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return errors.New("the passwords do not match")
		}
		pw = string(a)
	} else {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	hash, err := web.HashPassword(pw)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, hash)
	return nil
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
