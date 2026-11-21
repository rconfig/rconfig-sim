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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/inventory"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/runs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/testbench"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web"
	shippedscenarios "github.com/rcfg-sim/rcfg-sim/scenarios"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "settings-hash" {
		h, err := settingsHashCmd(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rcfg-sim-ui settings-hash: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(h)
		return
	}
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
		dataDir      = flag.String("data-dir", "/var/lib/rcfg-sim-ui", "presets, testbenches, runs, job logs")
		poll         = flag.Duration("poll", 2*time.Second, "instance poll interval")
		systemctl    = flag.String("systemctl", "systemctl", `command used for unit control, e.g. "sudo -n systemctl" on hosts without polkit`)
		logLevel     = flag.String("log-level", "info", "log level: error|warn|info|debug")
		keepBuilds   = flag.Int("keep-builds", 3, "builds kept on disk, least recently used removed first; the live build is always kept")
		keepRuns     = flag.Int("keep-runs", 200, "testbench run records kept, oldest removed first")
		startTimeout = flag.Duration("start-timeout", 120*time.Second, "how long a testbench start waits for every instance to match its settings (after any rolling restart)")
		scenarioDir  = flag.String("scenarios-dir", "/var/lib/rcfg-sim-ui/scenarios", "your scenario packages, one folder each (scenario.json, manifest.csv, configs/, optional commands/); read on every page load, beside the shipped ones")
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
	if *keepBuilds < 1 || *keepRuns < 1 || *startTimeout <= 0 {
		fmt.Fprintln(os.Stderr, "rcfg-sim-ui: --keep-builds and --keep-runs must be at least 1, --start-timeout positive")
		os.Exit(1)
	}
	if fi, err := os.Stat(*envDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "rcfg-sim-ui: --env-dir %s: not a readable directory\n", *envDir)
		os.Exit(1)
	}

	units := systemd.New(*systemctl, nil)
	fl := fleet.New(fleet.Options{EnvDir: *envDir, Units: units, Logger: logger})
	inv := inventory.NewCache()
	// The device catalogue comes from the generator (D14), read once and
	// again whenever the binary changes.
	cat := catalogue.NewSource(*genBin, nil)
	if _, err := cat.Get(); err != nil {
		logger.Warn("device catalogue not readable yet; testbenches and builds need it", "err", err)
	}
	runner := jobs.NewRunner(jobs.Options{
		EnvDir:     *envDir,
		GenBin:     *genBin,
		LogDir:     filepath.Join(*dataDir, "jobs"),
		Units:      units,
		Healthy:    fl.Healthy,
		Logger:     logger,
		KeepBuilds: *keepBuilds,
		Catalogue:  cat,
		Removed:    inv.Drop,
	})
	defer runner.Close()
	benches := testbench.NewStore(filepath.Join(*dataDir, "testbenches"), cat)
	benches.UseScenarios(scenarios.NewLibrary(shippedscenarios.Shipped, *scenarioDir, cat))
	runStore := runs.NewStore(filepath.Join(*dataDir, "runs"), *keepRuns, nil)
	bench := testbench.NewManager(testbench.ManagerOptions{
		DataDir: *dataDir, Store: benches, Runs: runStore, Jobs: runner, Fleet: fl, Units: units,
		StartTimeout: *startTimeout, Logger: logger,
	})
	defer bench.Close()
	srv := web.New(web.Options{
		Fleet:        fl,
		Units:        units,
		Jobs:         runner,
		Bench:        bench,
		Testbenches:  benches,
		Runs:         runStore,
		Inventory:    inv,
		Catalogue:    cat,
		DataDir:      *dataDir,
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
  rcfg-sim-ui settings-hash    print the settings_hash for build.json (used by make setup; see -h)

Flags:
`)
	flag.PrintDefaults()
}

// settingsHashCmd prints the settings_hash a build made with these
// rcfg-sim-gen settings gets in its build.json, so a build made outside the
// UI (make setup) is reused by a testbench with the same settings. A
// distribution is turned into exact counts the generator's way, using the
// catalogue of the generator at --gen-bin.
func settingsHashCmd(args []string) (string, error) {
	fs := flag.NewFlagSet("settings-hash", flag.ContinueOnError)
	genBin := fs.String("gen-bin", "/opt/rcfg-sim/bin/rcfg-sim-gen", "generator binary, for its device catalogue")
	count := fs.Int("count", 0, "device count (with --distribution)")
	dist := fs.String("distribution", "", "model distribution, as rcfg-sim-gen takes it")
	counts := fs.String("counts", "", "exact devices per model, as rcfg-sim-gen --counts takes it")
	seed := fs.Int64("seed", jobs.DefaultSeed, "generator seed")
	ipBase := fs.String("ip-base", "", "first IP")
	ipCount := fs.Int("ip-count", 0, "number of IPs")
	portStart := fs.Int("port-start", 0, "first port")
	perIP := fs.Int("devices-per-ip", 0, "devices per IP")
	opts := map[string]string{}
	fs.Func("option", "other rcfg-sim-gen flag as name=value (repeatable)", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return fmt.Errorf("want name=value, got %q", v)
		}
		opts[k] = val
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if *ipBase == "" || *ipCount < 1 || *portStart < 1 || *perIP < 1 {
		return "", errors.New("--ip-base, --ip-count, --port-start and --devices-per-ip are required")
	}
	if (*counts == "") == (*dist == "") {
		return "", errors.New("give either --counts, or --count with --distribution")
	}
	cat, err := catalogue.NewSource(*genBin, nil).Get()
	if err != nil {
		return "", err
	}
	byModel := map[string]int{}
	if *counts != "" {
		for _, tok := range strings.Split(*counts, ",") {
			m, n, ok := strings.Cut(strings.TrimSpace(tok), ":")
			v, err := strconv.Atoi(strings.TrimSpace(n))
			if !ok || err != nil {
				return "", fmt.Errorf("--counts token %q: want model:count", tok)
			}
			byModel[strings.TrimSpace(m)] += v
		}
	} else {
		if *count < 1 {
			return "", errors.New("--count is required with --distribution")
		}
		w, err := cat.ParseDistribution(*dist)
		if err != nil {
			return "", err
		}
		byModel = cat.Counts(*count, w)
	}
	if _, err := cat.CheckCounts(byModel); err != nil {
		return "", err
	}
	return jobs.HashSettings(byModel, *seed, opts,
		jobs.Layout{IPBase: *ipBase, IPCount: *ipCount, PortStart: *portStart, DevicesPerIP: *perIP}), nil
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
