package fleet

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/profile"
)

// Affected is one device a set of profile rules gives a profile to.
type Affected struct {
	Host     string
	IP       string
	Port     int
	Instance string
	Profile  string // e.g. "hang:1h0m0s"
	Pinned   bool   // from the manifest's profile column, not the rules
}

type manifestRow struct {
	host, ip string
	port     int
	pinned   string
}

type manifestCache struct {
	mu   sync.Mutex
	byPF map[string]cachedManifest
}

type cachedManifest struct {
	mod  time.Time
	size int64
	rows []manifestRow
}

var manifests = manifestCache{byPF: map[string]cachedManifest{}}

// readManifest returns the rows of a generator manifest, cached until the
// file changes. Only the columns the affected list needs are kept.
func readManifest(path string) ([]manifestRow, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	manifests.mu.Lock()
	defer manifests.mu.Unlock()
	if c, ok := manifests.byPF[path]; ok && c.mod.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.rows, nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	r := csv.NewReader(fh)
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	var rows []manifestRow
	for line := 0; ; line++ {
		rec, err := r.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if line == 0 || len(rec) < 3 {
			continue
		}
		port, err := strconv.Atoi(rec[2])
		if err != nil {
			continue
		}
		row := manifestRow{host: rec[0], ip: rec[1], port: port}
		if len(rec) > 10 {
			row.pinned = strings.TrimSpace(rec[10])
		}
		rows = append(rows, row)
	}
	manifests.byPF[path] = cachedManifest{fi.ModTime(), fi.Size(), rows}
	return rows, nil
}

// AffectedBy lists, across every discovered instance, the devices that
// rules and seed give a profile, plus devices pinned in the manifest. The
// selection is the one rcfg-sim makes (profile.Assign), so the list can be
// checked against rConfig's error logs. In instance (IP) order, then
// manifest order.
func (f *Fleet) AffectedBy(seed int64, rules []string) ([]Affected, error) {
	parsed, err := profile.Parse(strings.Join(rules, ","))
	if err != nil {
		return nil, err
	}
	var out []Affected
	var errs []string
	for _, v := range f.Views() {
		if v.Err != nil {
			continue
		}
		rows, err := readManifest(v.Manifest)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		end := v.PortStart + v.PortCount
		for _, r := range rows {
			if r.ip != v.ListenIP || r.port < v.PortStart || r.port >= end {
				continue
			}
			a := Affected{Host: r.host, IP: r.ip, Port: r.port, Instance: v.Name}
			if r.pinned != "" {
				p, err := profile.ParseProfile(r.pinned)
				if err != nil || p == nil {
					continue
				}
				a.Profile, a.Pinned = p.String(), true
			} else if p := profile.Assign(r.host, seed, parsed); p != nil {
				a.Profile = p.String()
			} else {
				continue
			}
			out = append(out, a)
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("manifest: %s", strings.Join(slices.Compact(errs), "; "))
	}
	return out, nil
}
