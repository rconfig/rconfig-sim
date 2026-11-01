package jobs

import (
	"sort"
	"time"

	"golang.org/x/sys/unix"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

// modelAvgBytes is the average config size per model. The Cisco buckets are
// the README's size table; the TL1 models were measured (400 devices each
// with rcfg-sim-gen). TestEstimateMatchesGenerator keeps them honest.
var modelAvgBytes = map[string]int64{
	"sm":                 25_600,
	"md":                 192_500,
	"lg":                 940_000,
	"xl":                 3_180_000,
	"2xl":                6_200_000,
	"3xl":                12_700_000,
	"4xl":                24_700_000,
	"5xl":                52_100_000,
	"6xl":                106_700_000,
	"ciena-6500-tl1":     1_700,
	"ciena-6500-tl1-gne": 7_300,
	"infinera-dtnx-tl1":  7_400,
	"cisco-ons15454-tl1": 2_950,
}

// Throughput figures for the time estimate, measured with rcfg-sim-gen:
// roughly 12 MB/s of config written plus a per-file cost.
const (
	genBytesPerSec = 12_000_000
	genPerFile     = 200 * time.Microsecond
	blockSize      = 4096 // files occupy whole filesystem blocks
	manifestRow    = 220  // bytes per manifest row, roughly
	diskHeadroom   = 1.10 // refuse unless free space is the estimate plus 10%
)

// Estimate is what a build will need.
type Estimate struct {
	ConfigBytes int64 // config content: also the page cache if every config is read (RAM)
	DiskBytes   int64 // on disk: whole blocks per file, plus the manifest
	NeedFree    int64 // DiskBytes plus 10%
	Generate    time.Duration
	PerModel    []ModelCount
}

// ModelCount is how many devices of a model a build makes.
type ModelCount struct {
	Model string
	N     int
	Bytes int64
}

// EstimateBuild estimates a build of count devices with the given
// distribution (as returned by configs.ParseDistribution). Device counts
// per model follow the generator's rounding closely enough for an estimate.
func EstimateBuild(count int, dist map[string]int) Estimate {
	var e Estimate
	files := 0
	for _, m := range configs.Models() {
		n := count * dist[m] / 100
		if n == 0 {
			continue
		}
		avg := modelAvgBytes[m]
		blocks := (avg + blockSize - 1) / blockSize * blockSize
		e.ConfigBytes += int64(n) * avg
		e.DiskBytes += int64(n) * blocks
		e.PerModel = append(e.PerModel, ModelCount{Model: m, N: n, Bytes: int64(n) * avg})
		files += n
	}
	sort.SliceStable(e.PerModel, func(i, j int) bool { return e.PerModel[i].Bytes > e.PerModel[j].Bytes })
	e.DiskBytes += int64(count) * manifestRow
	e.NeedFree = int64(float64(e.DiskBytes) * diskHeadroom)
	e.Generate = time.Duration(float64(e.ConfigBytes)/genBytesPerSec*float64(time.Second)) + time.Duration(files)*genPerFile
	return e
}

// FreeBytes is the space available to an unprivileged user on the
// filesystem holding path.
func FreeBytes(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
