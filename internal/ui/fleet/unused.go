package fleet

import (
	"encoding/csv"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// StatusUnused is an instance with no devices in the live manifest, as
// when a scenario testbench needs fewer instances than the host has
// (Phase 9). rcfg-sim will not start without devices, so it stays
// stopped and is left out of fan-out, the active-device split, drift and
// rolling restarts.
const StatusUnused Status = "unused"

// UnusedDetail is the card and Devices screen text for StatusUnused.
const UnusedDetail = "not used by this testbench"

// ManifestUse counts each instance's devices in manifest: the rows on its
// LISTEN_IP within its port range.
func ManifestUse(manifest string, insts []Instance) (map[string]int, error) {
	f, err := os.Open(manifest)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	if _, err := r.Read(); err != nil {
		return nil, err
	}
	type key struct {
		ip   string
		port int
	}
	counts := map[string]int{}
	byIP := map[string][]Instance{}
	for _, in := range insts {
		counts[in.Name] = 0
		byIP[in.ListenIP] = append(byIP[in.ListenIP], in)
	}
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			return counts, nil
		}
		if err != nil {
			return nil, err
		}
		if len(row) < 3 {
			continue
		}
		port, err := strconv.Atoi(row[2])
		if err != nil {
			continue
		}
		for _, in := range byIP[row[1]] {
			if port >= in.PortStart && port < in.PortStart+in.PortCount {
				counts[in.Name]++
			}
		}
	}
}

// Split divides names into the instances with devices in manifest and
// those without. When the manifest cannot be read every instance counts
// as used, as before Phase 9.
func Split(manifest string, insts []Instance, names []string) (used, unused []string) {
	counts, err := ManifestUse(manifest, insts)
	for _, n := range names {
		if c, ok := counts[n]; err == nil && ok && c == 0 {
			unused = append(unused, n)
		} else {
			used = append(used, n)
		}
	}
	return used, unused
}

// useCache keeps the last ManifestUse per manifest until the file changes,
// so a poll does not reread a 50,000-row manifest every few seconds.
type useCache struct {
	mu      sync.Mutex
	path    string
	size    int64
	mod     time.Time
	fi      os.FileInfo
	insts   string
	counts  map[string]int
	lastErr error
}

func (c *useCache) get(manifest string, insts []Instance) (map[string]int, error) {
	fi, err := os.Stat(manifest)
	if err != nil {
		return nil, err
	}
	sig := ""
	for _, in := range insts {
		sig += in.Name + "|" + in.ListenIP + "|" + strconv.Itoa(in.PortStart) + "|" + strconv.Itoa(in.PortCount) + ";"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fi != nil && c.path == manifest && os.SameFile(c.fi, fi) && c.size == fi.Size() && c.mod.Equal(fi.ModTime()) && c.insts == sig {
		return c.counts, c.lastErr
	}
	c.counts, c.lastErr = ManifestUse(manifest, insts)
	c.path, c.fi, c.size, c.mod, c.insts = manifest, fi, fi.Size(), fi.ModTime(), sig
	return c.counts, c.lastErr
}
