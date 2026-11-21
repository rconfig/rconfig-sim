// Package scenarios is the scenario library (docs/UI-ARCHITECTURE.md
// section 10.8, Phase 9): curated device packages such as mesh-campus,
// validated on load and installed as a build. A package is a folder with
// scenario.json, manifest.csv, configs/, optional commands/ and an optional
// README.md. Shipped packages are embedded in the binary; user packages
// are dropped into --scenarios-dir and read fresh on every list.
package scenarios

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
)

// Auth modes: where the instances' --password and --enable-password come
// from while the scenario runs.
const (
	AuthManifest    = "manifest"     // the manifest's credentials
	AuthAnyPassword = "any_password" // empty, so any password works
)

// Package is scenario.json.
type Package struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	Version          int               `json:"version"`
	Drivers          []string          `json:"drivers"`
	Auth             Auth              `json:"auth"`
	SSHAuth          string            `json:"ssh_auth,omitempty"` // --ssh-auth; "" is password
	Topology         Topology          `json:"topology"`
	SuggestedRuntime *SuggestedRuntime `json:"suggested_runtime,omitempty"`
}

// Auth is how devices log in.
type Auth struct {
	Mode string `json:"mode"`
}

// Topology is the diagram: nodes (manifest hostnames) in tiers, top
// first, and links between them.
type Topology struct {
	Nodes []Node      `json:"nodes"`
	Links [][2]string `json:"links"`
}

// Node is one device in the diagram.
type Node struct {
	ID   string `json:"id"`
	Tier int    `json:"tier"`
}

// SuggestedRuntime is the behaviour a new testbench from this scenario
// starts with.
type SuggestedRuntime struct {
	Faults  map[string]float64 `json:"faults,omitempty"`
	DelayMS *DelayMS           `json:"delay_ms,omitempty"`
}

// DelayMS is a response delay window.
type DelayMS struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Device is one manifest row of a package.
type Device struct {
	Hostname       string
	IP             string
	Port           int
	Vendor         string
	Driver         string // the manifest template column
	Username       string
	Password       string
	EnablePassword string
	ConfigFile     string // relative to the package
	Model          string // size_bucket
}

// Scenario is a loaded package: scenario.json, its devices, and why it
// cannot be used, if it cannot.
type Scenario struct {
	Package
	Folder   string // the folder name
	Shipped  bool
	Devices  []Device
	Commands []string // hostnames with a commands/ folder
	README   string
	Problems []string // validation failures; a scenario with any cannot be used
	FS       fs.FS    // the package files
}

// Problem is the validation failures as one line, "" when valid.
func (s Scenario) Problem() string { return strings.Join(s.Problems, "; ") }

// SSHAuthMode is --ssh-auth while the scenario runs.
func (s Scenario) SSHAuthMode() string { return cmp.Or(s.SSHAuth, "password") }

// Credentials are --password and --enable-password while the scenario
// runs: empty for any_password, else the first manifest row's (load
// checks every row has the same).
func (s Scenario) Credentials() (password, enable string) {
	if s.Auth.Mode != AuthManifest || len(s.Devices) == 0 {
		return "", ""
	}
	return s.Devices[0].Password, s.Devices[0].EnablePassword
}

// DeviceDrivers is the distinct drivers of the manifest's devices, sorted.
func (s Scenario) DeviceDrivers() []string {
	var out []string
	for _, d := range s.Devices {
		if !slices.Contains(out, d.Driver) {
			out = append(out, d.Driver)
		}
	}
	slices.Sort(out)
	return out
}

// Tier is a device's tier in the topology, or -1 when it is not a node.
func (s Scenario) Tier(hostname string) int {
	for _, n := range s.Topology.Nodes {
		if n.ID == hostname {
			return n.Tier
		}
	}
	return -1
}

// manifestHeader is rcfg-sim-gen's manifest header (a public format).
var manifestHeader = []string{"hostname", "ip", "port", "vendor", "template", "username", "password", "enable_password", "config_file", "size_bucket"}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads and validates the package in fsys (its root is the package
// folder). It always returns a Scenario; one that fails validation has
// Problems set. cat is the generator's catalogue, for the registered
// drivers.
func Load(fsys fs.FS, folder string, cat *catalogue.Catalogue) Scenario {
	s := Scenario{Folder: folder, FS: fsys}
	s.ID, s.Name = folder, folder
	bad := func(format string, args ...any) { s.Problems = append(s.Problems, fmt.Sprintf(format, args...)) }

	b, err := fs.ReadFile(fsys, "scenario.json")
	if err != nil {
		bad("scenario.json: %v", err)
		return s
	}
	var p Package
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		bad("scenario.json: %v", err)
		return s
	}
	s.Package = p
	if s.ID == "" {
		s.ID = folder
	}
	if s.Name == "" {
		s.Name = s.ID
	}
	switch {
	case !idPattern.MatchString(p.ID):
		bad("scenario.json: id %q must be lower-case letters, digits and dashes", p.ID)
	case p.ID != folder:
		bad("scenario.json: id %q does not match its folder %q", p.ID, folder)
	}
	if p.Version < 1 {
		bad("scenario.json: version must be 1 or more")
	}
	if p.Auth.Mode != AuthManifest && p.Auth.Mode != AuthAnyPassword {
		bad("scenario.json: auth.mode %q: want manifest or any_password", p.Auth.Mode)
	}
	if !slices.Contains([]string{"", "password", "driver", "none"}, p.SSHAuth) {
		bad("scenario.json: ssh_auth %q: want password, driver or none", p.SSHAuth)
	}
	if r, err := fs.ReadFile(fsys, "README.md"); err == nil {
		s.README = string(r)
	}

	s.Devices, err = readManifest(fsys)
	if err != nil {
		bad("manifest.csv: %v", err)
	}
	hosts := map[string]bool{}
	for _, d := range s.Devices {
		if hosts[d.Hostname] {
			bad("manifest.csv: hostname %s appears twice", d.Hostname)
		}
		hosts[d.Hostname] = true
		if !configInPackage(fsys, d.ConfigFile) {
			bad("%s has no config file (%s)", d.Hostname, cmp.Or(d.ConfigFile, "config_file is empty"))
		}
	}
	if p.Auth.Mode == AuthManifest {
		for _, d := range s.Devices[min(1, len(s.Devices)):] {
			if d.Password != s.Devices[0].Password || d.EnablePassword != s.Devices[0].EnablePassword {
				bad("auth.mode manifest needs the same password and enable password on every row: rcfg-sim takes one of each")
				break
			}
		}
	}

	if entries, err := fs.ReadDir(fsys, "commands"); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if !hosts[e.Name()] {
				bad("commands/%s matches no manifest hostname", e.Name())
				continue
			}
			s.Commands = append(s.Commands, e.Name())
		}
	}

	known := map[string]bool{}
	for _, d := range cat.Drivers {
		known[d.ID] = true
	}
	drivers := slices.Clone(p.Drivers)
	for _, d := range s.DeviceDrivers() {
		if !slices.Contains(drivers, d) {
			drivers = append(drivers, d)
		}
	}
	for _, d := range drivers {
		if !known[d] {
			bad("driver %q is not registered in this rcfg-sim", d)
		}
	}

	nodes := map[string]bool{}
	for _, n := range p.Topology.Nodes {
		if !hosts[n.ID] {
			bad("topology node %s is not a manifest hostname", n.ID)
		}
		if n.Tier < 0 {
			bad("topology node %s: tier must be 0 or more", n.ID)
		}
		nodes[n.ID] = true
	}
	for _, l := range p.Topology.Links {
		for _, end := range l {
			if !nodes[end] {
				bad("topology link %s to %s: %s is not a node", l[0], l[1], end)
			}
		}
	}
	return s
}

// configInPackage reports whether a manifest config_file is a relative path
// to a regular file inside the package.
func configInPackage(fsys fs.FS, p string) bool {
	p = filepath.ToSlash(p)
	if p == "" || path.IsAbs(p) || !fs.ValidPath(p) {
		return false
	}
	fi, err := fs.Stat(fsys, p)
	return err == nil && fi.Mode().IsRegular()
}

func readManifest(fsys fs.FS) ([]Device, error) {
	f, err := fsys.Open("manifest.csv")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if len(head) < len(manifestHeader) || !slices.Equal(head[:len(manifestHeader)], manifestHeader) {
		return nil, fmt.Errorf("header must start %s", strings.Join(manifestHeader, ","))
	}
	var out []Device
	for line := 2; ; line++ {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, err
		}
		if len(row) < len(manifestHeader) {
			return out, fmt.Errorf("line %d: %d columns, want %d", line, len(row), len(manifestHeader))
		}
		port, err := strconv.Atoi(row[2])
		if err != nil {
			return out, fmt.Errorf("line %d: port %q", line, row[2])
		}
		out = append(out, Device{Hostname: row[0], IP: row[1], Port: port, Vendor: row[3], Driver: row[4],
			Username: row[5], Password: row[6], EnablePassword: row[7], ConfigFile: row[8], Model: row[9]})
	}
	if len(out) == 0 {
		return nil, errors.New("no device rows")
	}
	return out, nil
}

// Library is the shipped packages plus the user's --scenarios-dir.
type Library struct {
	shipped fs.FS // one folder per package
	dir     string
	cat     *catalogue.Source
}

// NewLibrary returns a library over the shipped packages and dir (which
// need not exist).
func NewLibrary(shipped fs.FS, dir string, cat *catalogue.Source) *Library {
	return &Library{shipped: shipped, dir: dir, cat: cat}
}

// Dir is --scenarios-dir.
func (l *Library) Dir() string { return l.dir }

// List loads every package: the shipped ones, then the user's, each by
// name. A folder without scenario.json is not a package and is skipped. A
// user package whose id a shipped one already has is listed with that
// problem.
func (l *Library) List() ([]Scenario, error) {
	cat, err := l.cat.Get()
	if err != nil {
		return nil, err
	}
	var shipped, user []Scenario
	if l.shipped != nil {
		entries, _ := fs.ReadDir(l.shipped, ".")
		for _, e := range entries {
			sub, err := fs.Sub(l.shipped, e.Name())
			if !e.IsDir() || err != nil || !exists(sub, "scenario.json") {
				continue
			}
			s := Load(sub, e.Name(), cat)
			s.Shipped = true
			shipped = append(shipped, s)
		}
	}
	if l.dir != "" {
		entries, err := os.ReadDir(l.dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("scenarios dir: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := os.DirFS(filepath.Join(l.dir, e.Name()))
			if !exists(sub, "scenario.json") {
				continue
			}
			s := Load(sub, e.Name(), cat)
			if slices.ContainsFunc(shipped, func(x Scenario) bool { return x.ID == s.ID }) {
				s.Problems = append(s.Problems, fmt.Sprintf("id %q is already used by a shipped scenario", s.ID))
			}
			user = append(user, s)
		}
	}
	byName := func(a, b Scenario) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) }
	slices.SortFunc(shipped, byName)
	slices.SortFunc(user, byName)
	return append(shipped, user...), nil
}

// Get loads the package with this id: the shipped one first.
func (l *Library) Get(id string) (Scenario, error) {
	all, err := l.List()
	if err != nil {
		return Scenario{}, err
	}
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return Scenario{}, fmt.Errorf("no scenario %q", id)
}

func exists(fsys fs.FS, name string) bool {
	_, err := fs.Stat(fsys, name)
	return err == nil
}
