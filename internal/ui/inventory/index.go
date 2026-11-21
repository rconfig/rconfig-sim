// Package inventory is the Devices screen's data: an in-memory index of a
// build's manifest, each device's status joined at request time from the
// fleet's live state, filtering and sorting, the CSV and JSON exports, and
// Test login (docs/UI-ARCHITECTURE.md, Phase 7).
package inventory

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Device is one manifest row, with the fields as the manifest has them.
type Device struct {
	Hostname       string
	IP             string
	Port           int
	Vendor         string
	Template       string // the driver id, e.g. cisco_ios
	Username       string
	Password       string
	EnablePassword string
	ConfigFile     string
	Model          string // the size_bucket column, e.g. sm or ciena-6500-tl1
	Pinned         string // the optional profile column; "" for none
}

// Index is one build's manifest, parsed once.
type Index struct {
	Build   string // build name, e.g. 20261031-0915
	SHA256  string // of manifest.csv
	Devices []Device

	byHost map[string]int
}

// Lookup returns the device with this hostname.
func (x *Index) Lookup(hostname string) (Device, bool) {
	i, ok := x.byHost[hostname]
	if !ok {
		return Device{}, false
	}
	return x.Devices[i], true
}

// Cache keeps one Index per build, keyed by build name and manifest
// SHA-256, so a manifest is parsed once and a rewritten one is parsed
// again.
type Cache struct {
	mu sync.Mutex
	m  map[string]*Index
}

// NewCache returns an empty Cache.
func NewCache() *Cache { return &Cache{m: map[string]*Index{}} }

// Load returns the index of build's manifest at path, from the cache when
// the file's hash is unchanged.
func (c *Cache) Load(build, path string) (*Index, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	c.mu.Lock()
	if x, ok := c.m[build]; ok && x.SHA256 == sha {
		c.mu.Unlock()
		return x, nil
	}
	c.mu.Unlock()
	x, err := Parse(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	x.Build, x.SHA256 = build, sha
	c.mu.Lock()
	c.m[build] = x
	c.mu.Unlock()
	return x, nil
}

// Drop forgets build's index, for example when retention deletes the build.
func (c *Cache) Drop(build string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, build)
}

// Builds lists the cached build names, for tests.
func (c *Cache) Builds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for b := range c.m {
		out = append(out, b)
	}
	return out
}

// manifestColumns are the generator's manifest header names. profile is
// optional.
var manifestColumns = []string{"hostname", "ip", "port", "vendor", "template", "username", "password",
	"enable_password", "config_file", "size_bucket", "profile"}

// Parse reads a generator manifest. Columns are found by their header
// names; hostname, ip and port are required.
func Parse(r io.Reader) (*Index, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("manifest header: %w", err)
	}
	col := map[string]int{}
	for _, name := range manifestColumns {
		col[name] = -1
	}
	for i, h := range head {
		if _, ok := col[strings.TrimSpace(h)]; ok {
			col[strings.TrimSpace(h)] = i
		}
	}
	for _, need := range []string{"hostname", "ip", "port"} {
		if col[need] < 0 {
			return nil, fmt.Errorf("manifest has no %q column", need)
		}
	}
	x := &Index{byHost: map[string]int{}}
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		get := func(name string) string {
			if i := col[name]; i >= 0 && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		port, err := strconv.Atoi(get("port"))
		if err != nil {
			return nil, fmt.Errorf("line %d: port %q", line, get("port"))
		}
		d := Device{Hostname: get("hostname"), IP: get("ip"), Port: port, Vendor: get("vendor"), Template: get("template"),
			Username: get("username"), Password: get("password"), EnablePassword: get("enable_password"),
			ConfigFile: get("config_file"), Model: get("size_bucket"), Pinned: get("profile")}
		x.byHost[d.Hostname] = len(x.Devices)
		x.Devices = append(x.Devices, d)
	}
	return x, nil
}
