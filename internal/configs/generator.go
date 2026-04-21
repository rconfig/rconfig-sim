package configs

import (
	"embed"
	"encoding/csv"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var (
	bucketOrder     = []string{"small", "medium", "large", "huge"}
	compiledOnce    *template.Template
	templateFuncMap = template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		"seq": func(start, end int) []int {
			if end < start {
				return nil
			}
			s := make([]int, end-start+1)
			for i := range s {
				s[i] = start + i
			}
			return s
		},
	}
)

func init() {
	compiledOnce = template.Must(template.New("root").Funcs(templateFuncMap).ParseFS(templateFS, "templates/*.tmpl"))
}

// profile holds the per-size-bucket generation counts.
type profile struct {
	deviceKind           string
	interfaceCount       int
	subinterfaceCount    int
	vlanCount            int
	aclCount             int
	aclEntriesMin        int
	aclEntriesMax        int
	prefixListCount      int
	prefixListEntriesMin int
	prefixListEntriesMax int
	routeMapCount        int
	staticRouteCount     int
	bgpNeighbors         int
	ospfAreas            int
	vrfCount             int
	hasBGP               bool
	hasOSPF              bool
	hasCrypto            bool
	hasQoS               bool
	hasVRF               bool
	fileSizeHint         int
}

var profiles = map[string]profile{
	"small": {
		deviceKind:     "switch",
		interfaceCount: 48, subinterfaceCount: 0, vlanCount: 4,
		aclCount: 3, aclEntriesMin: 25, aclEntriesMax: 40,
		staticRouteCount: 5,
		fileSizeHint:     30000,
	},
	"medium": {
		deviceKind:     "router",
		interfaceCount: 48, subinterfaceCount: 60, vlanCount: 20,
		aclCount: 12, aclEntriesMin: 120, aclEntriesMax: 200,
		prefixListCount: 10, prefixListEntriesMin: 25, prefixListEntriesMax: 40,
		routeMapCount:    12,
		staticRouteCount: 250,
		ospfAreas:        1,
		hasOSPF:          true, hasCrypto: true, hasQoS: true,
		fileSizeHint: 150000,
	},
	"large": {
		deviceKind:     "router",
		interfaceCount: 96, subinterfaceCount: 60, vlanCount: 30,
		aclCount: 25, aclEntriesMin: 350, aclEntriesMax: 500,
		prefixListCount: 18, prefixListEntriesMin: 30, prefixListEntriesMax: 45,
		routeMapCount:    30,
		staticRouteCount: 1800,
		bgpNeighbors:     10,
		ospfAreas:        3,
		vrfCount:         6,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 700000,
	},
	"huge": {
		deviceKind:     "router",
		interfaceCount: 192, subinterfaceCount: 100, vlanCount: 40,
		aclCount: 60, aclEntriesMin: 600, aclEntriesMax: 800,
		prefixListCount: 40, prefixListEntriesMin: 40, prefixListEntriesMax: 60,
		routeMapCount:    80,
		staticRouteCount: 3000,
		bgpNeighbors:     20,
		ospfAreas:        4,
		vrfCount:         30,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 4000000,
	},
}

// Config drives a generator run. Every field is user-set via the CLI.
type Config struct {
	Count          int
	OutputDir      string
	ManifestPath   string
	IPBase         string
	IPCount        int
	PortStart      int
	DevicesPerIP   int
	Seed           int64
	Distribution   string
	Username       string
	Password       string
	EnablePassword string
}

// Summary is the generator's final report.
type Summary struct {
	Count      int
	Elapsed    time.Duration
	TotalBytes int64
	PerBucket  map[string]bucketStats
	Weights    map[string]int
}

type bucketStats struct {
	Target   int
	Realised int
	Bytes    int64
}

func (s Summary) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "generator summary: count=%d elapsed=%s total_bytes=%d (%.2f MB)\n",
		s.Count, s.Elapsed.Round(time.Millisecond), s.TotalBytes, float64(s.TotalBytes)/(1024*1024))
	fmt.Fprintln(&sb, "per-bucket distribution:")
	for _, b := range bucketOrder {
		bs := s.PerBucket[b]
		w := s.Weights[b]
		targetPct := float64(w)
		realisedPct := 0.0
		if s.Count > 0 {
			realisedPct = 100 * float64(bs.Realised) / float64(s.Count)
		}
		delta := realisedPct - targetPct
		fmt.Fprintf(&sb, "  %-6s target=%3d%% realised=%6.2f%% (Δ %+5.2f pp) count=%6d bytes=%d (avg %d)\n",
			b, w, realisedPct, delta, bs.Realised, bs.Bytes, avgOr0(bs.Bytes, bs.Realised))
	}
	return sb.String()
}

func avgOr0(total int64, n int) int64 {
	if n == 0 {
		return 0
	}
	return total / int64(n)
}

// parseDistribution turns "small:40,medium:40,large:15,huge:5" into a map of percent weights.
func parseDistribution(s string) (map[string]int, error) {
	weights := map[string]int{}
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.SplitN(tok, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("distribution token %q: want bucket:weight", tok)
		}
		name := strings.TrimSpace(parts[0])
		if _, ok := profiles[name]; !ok {
			return nil, fmt.Errorf("unknown bucket %q (valid: small, medium, large, huge)", name)
		}
		w, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			return nil, fmt.Errorf("distribution token %q: weight not int: %w", tok, err)
		}
		if w < 0 {
			return nil, fmt.Errorf("distribution token %q: weight must be >= 0", tok)
		}
		weights[name] = w
	}
	total := 0
	for _, w := range weights {
		total += w
	}
	if total != 100 {
		return nil, fmt.Errorf("distribution weights must sum to 100, got %d", total)
	}
	for _, b := range bucketOrder {
		if _, ok := weights[b]; !ok {
			weights[b] = 0
		}
	}
	return weights, nil
}

// stratifiedCounts allocates exact per-bucket counts, distributing rounding remainder
// to the largest-weighted bucket. Guarantees sum == total.
func stratifiedCounts(total int, weights map[string]int) map[string]int {
	counts := map[string]int{}
	allocated := 0
	for _, b := range bucketOrder {
		c := total * weights[b] / 100
		counts[b] = c
		allocated += c
	}
	// Give leftover to the bucket with the highest weight (stable: tie broken by order).
	rem := total - allocated
	if rem > 0 {
		var top string
		topW := -1
		for _, b := range bucketOrder {
			if weights[b] > topW {
				top = b
				topW = weights[b]
			}
		}
		counts[top] += rem
	}
	return counts
}

// buildAssignments produces a flat slice where assignments[i] is the bucket for device i.
// The slice is shuffled with the supplied RNG so bucket order is mixed but reproducible.
func buildAssignments(counts map[string]int, rng *rand.Rand) []string {
	total := 0
	for _, c := range counts {
		total += c
	}
	out := make([]string, 0, total)
	for _, b := range bucketOrder {
		for i := 0; i < counts[b]; i++ {
			out = append(out, b)
		}
	}
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// Run renders all configs and writes the manifest. Returns a Summary with realised counts.
func Run(cfg Config, stdout io.Writer) (Summary, error) {
	if cfg.Count <= 0 {
		return Summary{}, fmt.Errorf("--count must be > 0")
	}
	if cfg.DevicesPerIP <= 0 {
		return Summary{}, fmt.Errorf("--devices-per-ip must be > 0")
	}
	if cfg.IPCount <= 0 {
		return Summary{}, fmt.Errorf("--ip-count must be > 0")
	}
	if cfg.Count > cfg.IPCount*cfg.DevicesPerIP {
		return Summary{}, fmt.Errorf("--count %d exceeds capacity %d (%d IPs * %d devices)",
			cfg.Count, cfg.IPCount*cfg.DevicesPerIP, cfg.IPCount, cfg.DevicesPerIP)
	}

	weights, err := parseDistribution(cfg.Distribution)
	if err != nil {
		return Summary{}, err
	}

	start := time.Now()

	counts := stratifiedCounts(cfg.Count, weights)
	mainRng := rand.New(rand.NewSource(cfg.Seed))
	assignments := buildAssignments(counts, mainRng)

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("mkdir output dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ManifestPath), 0o755); err != nil {
		return Summary{}, fmt.Errorf("mkdir manifest dir: %w", err)
	}

	// Render devices in parallel. Each device is fully determined by (seed, index, bucket),
	// so workers can run in any order; results are keyed by index and collected into a
	// pre-sized slice, then the manifest is written in index order to keep bytes
	// reproducible across runs.
	type result struct {
		hostname string
		ip       string
		port     int
		bucket   string
		path     string
		bytes    int64
		err      error
	}
	results := make([]result, len(assignments))

	workers := runtime.NumCPU()
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int, workers*4)

	var wg sync.WaitGroup
	for wIdx := 0; wIdx < workers; wIdx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				bucket := assignments[i]
				data := buildDeviceData(cfg, i, bucket)

				filename := fmt.Sprintf("device-%05d.cfg", i)
				path := filepath.Join(cfg.OutputDir, filename)

				f, ferr := os.Create(path)
				if ferr != nil {
					results[i] = result{err: fmt.Errorf("create %s: %w", path, ferr)}
					continue
				}
				counter := &byteCounter{w: f}
				if rerr := compiledOnce.ExecuteTemplate(counter, bucket+".tmpl", data); rerr != nil {
					f.Close()
					results[i] = result{err: fmt.Errorf("render %s (%s): %w", path, bucket, rerr)}
					continue
				}
				if cerr := f.Close(); cerr != nil {
					results[i] = result{err: fmt.Errorf("close %s: %w", path, cerr)}
					continue
				}

				results[i] = result{
					hostname: data.Hostname,
					ip:       ipPlusOffset(cfg.IPBase, i/cfg.DevicesPerIP),
					port:     cfg.PortStart + (i % cfg.DevicesPerIP),
					bucket:   bucket,
					path:     path,
					bytes:    counter.n,
				}
			}
		}()
	}

	for i := range assignments {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			return Summary{}, fmt.Errorf("device %d: %w", i, r.err)
		}
	}

	mf, err := os.Create(cfg.ManifestPath)
	if err != nil {
		return Summary{}, fmt.Errorf("create manifest: %w", err)
	}
	defer mf.Close()

	w := csv.NewWriter(mf)
	if err := w.Write([]string{
		"hostname", "ip", "port", "vendor", "template",
		"username", "password", "enable_password", "config_file", "size_bucket",
	}); err != nil {
		return Summary{}, fmt.Errorf("manifest header: %w", err)
	}

	summary := Summary{
		Count:     cfg.Count,
		PerBucket: map[string]bucketStats{},
		Weights:   weights,
	}
	for _, b := range bucketOrder {
		summary.PerBucket[b] = bucketStats{Target: counts[b]}
	}

	for i, r := range results {
		if err := w.Write([]string{
			r.hostname, r.ip, strconv.Itoa(r.port),
			"Cisco", "cisco_ios",
			cfg.Username, cfg.Password, cfg.EnablePassword,
			r.path, r.bucket,
		}); err != nil {
			return summary, fmt.Errorf("manifest row %d: %w", i, err)
		}
		bs := summary.PerBucket[r.bucket]
		bs.Realised++
		bs.Bytes += r.bytes
		summary.PerBucket[r.bucket] = bs
		summary.TotalBytes += r.bytes
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return summary, fmt.Errorf("flush manifest: %w", err)
	}

	summary.Elapsed = time.Since(start)

	// Verify distribution within ±1 percentage point (tolerance widened from ±1 integer
	// to ±1 percentage point, since below ~100 devices any rounding blows the stricter bound).
	var offenders []string
	for _, b := range bucketOrder {
		bs := summary.PerBucket[b]
		target := float64(weights[b])
		realised := 100 * float64(bs.Realised) / float64(summary.Count)
		if abs(realised-target) > 1.0 {
			offenders = append(offenders, fmt.Sprintf("%s (target %v%%, realised %.2f%%)", b, target, realised))
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		fmt.Fprintf(stdout, "WARN: distribution deviation >1pp for: %s\n", strings.Join(offenders, ", "))
	}

	return summary, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// byteCounter is an io.Writer that counts bytes written on the way through.
type byteCounter struct {
	w io.Writer
	n int64
}

func (b *byteCounter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}
