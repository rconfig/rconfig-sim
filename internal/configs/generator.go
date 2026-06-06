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
	bucketOrder  = []string{"sm", "md", "lg", "xl", "2xl", "3xl", "4xl", "5xl", "6xl"}
	compiledOnce *template.Template
	// templateAliases lets new buckets share an existing .tmpl file. Tiers above
	// "xl" differ only in profile counts (more interfaces, ACLs, routes); they
	// share xl.tmpl's feature set rather than duplicating 11 KB per tier.
	templateAliases = map[string]string{
		"2xl": "xl.tmpl",
		"3xl": "xl.tmpl",
		"4xl": "xl.tmpl",
		"5xl": "xl.tmpl",
		"6xl": "xl.tmpl",
	}
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

// model is one fully-qualified, generatable device model. The registry key is
// the model name used in --distribution and written to the manifest size_bucket
// column. Each model carries its manifest vendor/template strings, the embedded
// template to render, and a per-vendor data-builder.
//
// Adding a vendor/model is one registry entry plus a template file — the
// generator never special-cases a vendor beyond this struct.
type model struct {
	name     string
	vendor   string // manifest "vendor" column
	template string // manifest "template" column = runtime driver id
	tmplFile string // embedded template name to ExecuteTemplate
	build    func(cfg Config, index int, m model) any
}

// registry maps a model name to its model. The Cisco size buckets are derived
// mechanically from the existing profiles + templateAliases so their resolved
// template, builder, and manifest output are byte-identical to before the
// registry existed. The Ciena 6500 is appended as the first non-Cisco model.
var registry = func() map[string]model {
	r := make(map[string]model, len(bucketOrder)+1)
	for _, name := range bucketOrder {
		tmplFile := name + ".tmpl"
		if alias, ok := templateAliases[name]; ok {
			tmplFile = alias
		}
		r[name] = model{
			name:     name,
			vendor:   "Cisco",
			template: "cisco_ios",
			tmplFile: tmplFile,
			build: func(cfg Config, index int, m model) any {
				return buildDeviceData(cfg, index, m.name)
			},
		}
	}
	r[cienaModelName] = cienaModel()
	return r
}()

// modelOrder is the canonical iteration order: the Cisco buckets in their
// existing order, then non-Cisco models appended. Keeping Cisco first and
// unchanged is what preserves deterministic assignment for legacy invocations.
var modelOrder = append(append([]string{}, bucketOrder...), cienaModelName)

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
	"sm": {
		deviceKind:     "switch",
		interfaceCount: 48, subinterfaceCount: 0, vlanCount: 4,
		aclCount: 3, aclEntriesMin: 25, aclEntriesMax: 40,
		staticRouteCount: 5,
		fileSizeHint:     30000,
	},
	"md": {
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
	"lg": {
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
	"xl": {
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
	"2xl": {
		deviceKind:     "router",
		interfaceCount: 256, subinterfaceCount: 150, vlanCount: 60,
		aclCount: 100, aclEntriesMin: 750, aclEntriesMax: 1000,
		prefixListCount: 60, prefixListEntriesMin: 50, prefixListEntriesMax: 80,
		routeMapCount:    120,
		staticRouteCount: 6000,
		bgpNeighbors:     30,
		ospfAreas:        6,
		vrfCount:         50,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 8000000,
	},
	"3xl": {
		deviceKind:     "router",
		interfaceCount: 384, subinterfaceCount: 200, vlanCount: 80,
		aclCount: 160, aclEntriesMin: 1000, aclEntriesMax: 1300,
		prefixListCount: 90, prefixListEntriesMin: 60, prefixListEntriesMax: 100,
		routeMapCount:    180,
		staticRouteCount: 12000,
		bgpNeighbors:     40,
		ospfAreas:        8,
		vrfCount:         70,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 16000000,
	},
	"4xl": {
		deviceKind:     "router",
		interfaceCount: 512, subinterfaceCount: 280, vlanCount: 100,
		aclCount: 240, aclEntriesMin: 1300, aclEntriesMax: 1700,
		prefixListCount: 130, prefixListEntriesMin: 80, prefixListEntriesMax: 130,
		routeMapCount:    260,
		staticRouteCount: 24000,
		bgpNeighbors:     60,
		ospfAreas:        10,
		vrfCount:         100,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 32000000,
	},
	"5xl": {
		deviceKind:     "router",
		interfaceCount: 768, subinterfaceCount: 400, vlanCount: 140,
		aclCount: 380, aclEntriesMin: 1700, aclEntriesMax: 2300,
		prefixListCount: 200, prefixListEntriesMin: 100, prefixListEntriesMax: 170,
		routeMapCount:    400,
		staticRouteCount: 48000,
		bgpNeighbors:     100,
		ospfAreas:        14,
		vrfCount:         150,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 64000000,
	},
	"6xl": {
		deviceKind:     "router",
		interfaceCount: 1024, subinterfaceCount: 600, vlanCount: 200,
		aclCount: 600, aclEntriesMin: 2200, aclEntriesMax: 3000,
		prefixListCount: 320, prefixListEntriesMin: 130, prefixListEntriesMax: 220,
		routeMapCount:    640,
		staticRouteCount: 96000,
		bgpNeighbors:     160,
		ospfAreas:        20,
		vrfCount:         220,
		hasBGP:           true, hasOSPF: true, hasCrypto: true, hasQoS: true, hasVRF: true,
		fileSizeHint: 128000000,
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
	for _, b := range modelOrder {
		bs := s.PerBucket[b]
		w := s.Weights[b]
		// Suppress models that are absent from this run (no target, none
		// realised) so legacy Cisco-only output is unchanged.
		if w == 0 && bs.Target == 0 && bs.Realised == 0 {
			continue
		}
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

// parseDistribution turns "sm:40,md:40,lg:15,xl:5" into a map of percent weights.
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
		if _, ok := registry[name]; !ok {
			return nil, fmt.Errorf("unknown model %q (valid: %s)", name, strings.Join(modelOrder, ", "))
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
	for _, b := range modelOrder {
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
	for _, b := range modelOrder {
		c := total * weights[b] / 100
		counts[b] = c
		allocated += c
	}
	// Give leftover to the model with the highest weight (stable: tie broken by order).
	rem := total - allocated
	if rem > 0 {
		var top string
		topW := -1
		for _, b := range modelOrder {
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
	for _, b := range modelOrder {
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
				m := registry[bucket]
				data := m.build(cfg, i, m)

				filename := fmt.Sprintf("device-%05d.cfg", i)
				path := filepath.Join(cfg.OutputDir, filename)

				f, ferr := os.Create(path)
				if ferr != nil {
					results[i] = result{err: fmt.Errorf("create %s: %w", path, ferr)}
					continue
				}
				counter := &byteCounter{w: f}
				if rerr := compiledOnce.ExecuteTemplate(counter, m.tmplFile, data); rerr != nil {
					f.Close()
					results[i] = result{err: fmt.Errorf("render %s (%s): %w", path, bucket, rerr)}
					continue
				}
				if cerr := f.Close(); cerr != nil {
					results[i] = result{err: fmt.Errorf("close %s: %w", path, cerr)}
					continue
				}

				results[i] = result{
					hostname: modelHostname(data),
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
	for _, b := range modelOrder {
		summary.PerBucket[b] = bucketStats{Target: counts[b]}
	}

	for i, r := range results {
		m := registry[r.bucket]
		if err := w.Write([]string{
			r.hostname, r.ip, strconv.Itoa(r.port),
			m.vendor, m.template,
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
	for _, b := range modelOrder {
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
