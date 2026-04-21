package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

func main() {
	cfg := configs.Config{}
	flag.IntVar(&cfg.Count, "count", 50000, "number of device configs to generate")
	flag.StringVar(&cfg.OutputDir, "output-dir", "/opt/rcfg-sim/configs", "directory for rendered .cfg files")
	flag.StringVar(&cfg.ManifestPath, "manifest", "/opt/rcfg-sim/manifest.csv", "output CSV manifest path")
	flag.StringVar(&cfg.IPBase, "ip-base", "10.50.0.1", "first IP alias")
	flag.IntVar(&cfg.IPCount, "ip-count", 20, "number of IP aliases")
	flag.IntVar(&cfg.PortStart, "port-start", 10000, "first port in range")
	flag.IntVar(&cfg.DevicesPerIP, "devices-per-ip", 2500, "devices mapped to each IP")
	flag.Int64Var(&cfg.Seed, "seed", 42, "PRNG seed for deterministic output")
	flag.StringVar(&cfg.Distribution, "distribution", "small:40,medium:40,large:15,huge:5", "size-bucket weights (percent, sum=100)")
	flag.StringVar(&cfg.Username, "username", "admin", "username written into manifest")
	flag.StringVar(&cfg.Password, "password", "admin", "password written into manifest")
	flag.StringVar(&cfg.EnablePassword, "enable-password", "enable123", "enable password written into manifest")
	flag.Parse()

	summary, err := configs.Run(cfg, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcfg-sim-gen: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(summary.String())
}
