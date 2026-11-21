package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "catalogue" {
		os.Exit(catalogue(os.Args[2:]))
	}
	cfg := configs.Config{}
	flag.IntVar(&cfg.Count, "count", 50000, "number of device configs to generate")
	flag.StringVar(&cfg.OutputDir, "output-dir", "/opt/rcfg-sim/configs", "directory for rendered .cfg files")
	flag.StringVar(&cfg.ManifestPath, "manifest", "/opt/rcfg-sim/manifest.csv", "output CSV manifest path")
	flag.StringVar(&cfg.IPBase, "ip-base", "10.50.0.1", "first IP alias")
	flag.IntVar(&cfg.IPCount, "ip-count", 20, "number of IP aliases")
	flag.IntVar(&cfg.PortStart, "port-start", 10000, "first port in range")
	flag.IntVar(&cfg.DevicesPerIP, "devices-per-ip", 2500, "devices mapped to each IP")
	flag.Int64Var(&cfg.Seed, "seed", 42, "PRNG seed for deterministic output")
	flag.StringVar(&cfg.Distribution, "distribution", "sm:40,md:40,lg:15,xl:5", "model weights (percent, sum=100; models: sm, md, lg, xl, 2xl, 3xl, 4xl, 5xl, 6xl, ciena-6500-tl1, ciena-6500-tl1-gne, infinera-dtnx-tl1, cisco-ons15454-tl1)")
	flag.StringVar(&cfg.Counts, "counts", "", `exact devices per model, e.g. "sm:6000,md:4000,ciena-6500-tl1-gne:2000"; instead of --distribution, and --count defaults to their sum`)
	flag.IntVar(&cfg.RNEDualHomePct, "rne-dual-home-pct", 0, "percent of GNE remote NEs drawn from a shared pool, so the same RNE is reachable through more than one gateway (0 = none)")
	flag.StringVar(&cfg.Username, "username", "admin", "username written into manifest")
	flag.StringVar(&cfg.Password, "password", "admin", "password written into manifest")
	flag.StringVar(&cfg.EnablePassword, "enable-password", "enable123", "enable password written into manifest")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n  rcfg-sim-gen [flags]            generate configs and a manifest\n  rcfg-sim-gen catalogue --json   print the device families, models, options and drivers\n\nFlags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if cfg.Counts != "" {
		set := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["distribution"] {
			fmt.Fprintln(os.Stderr, "rcfg-sim-gen: --counts and --distribution are mutually exclusive")
			os.Exit(2)
		}
		_, total, err := configs.ParseCounts(cfg.Counts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rcfg-sim-gen: %v\n", err)
			os.Exit(2)
		}
		if !set["count"] {
			cfg.Count = total
		}
	}

	summary, err := configs.Run(cfg, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-gen: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(summary.String())
}

// catalogue prints the device catalogue (D14): what a front end needs to
// offer the generator's models without a hardcoded list.
func catalogue(args []string) int {
	fs := flag.NewFlagSet("catalogue", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print as JSON (the only format)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !*asJSON {
		fmt.Fprintln(os.Stderr, "rcfg-sim-gen catalogue: pass --json")
		return 2
	}
	if err := configs.WriteCatalogueJSON(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-gen catalogue: %v\n", err)
		return 1
	}
	return 0
}
