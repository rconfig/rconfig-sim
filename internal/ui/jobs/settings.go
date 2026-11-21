package jobs

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Settings is a build's build.json: what it was generated with. Options
// holds the other rcfg-sim-gen flags (for example rne-dual-home-pct) so a
// new build carries them over although the UI does not expose them.
type Settings struct {
	Name         string            `json:"name"`
	Created      time.Time         `json:"created"`
	Adopted      bool              `json:"adopted,omitempty"` // made by make adopt-data from a pre-UI build
	Count        int               `json:"count"`
	Seed         *int64            `json:"seed,omitempty"`         // nil: unknown (adopted build)
	Distribution string            `json:"distribution,omitempty"` // as the Build screen asked; "" for a testbench or adopted build
	Counts       map[string]int    `json:"counts,omitempty"`       // exact devices per model; nil for an adopted or older build
	IPBase       string            `json:"ip_base,omitempty"`
	IPCount      int               `json:"ip_count,omitempty"`
	PortStart    int               `json:"port_start,omitempty"`
	DevicesPerIP int               `json:"devices_per_ip,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
	Previous     string            `json:"previous,omitempty"` // the build that was live before this one
	// SettingsHash identifies what the build was made from (HashSettings),
	// so a testbench can reuse it instead of generating. Empty for an
	// adopted build.
	SettingsHash string `json:"settings_hash,omitempty"`
	// LastUsed is when the build last went live; retention drops the least
	// recently used first.
	LastUsed time.Time `json:"last_used,omitempty"`
	// Scenario is the scenario package a build was installed from, as
	// "<id> v<version>"; "" for a generated or adopted build (Phase 9).
	Scenario string `json:"scenario,omitempty"`
}

// HashSettings is a hash of the normalised build settings: devices per
// model, seed, generator options and IP layout. Two builds with the same
// hash have the same configs (the generator is deterministic, and equal
// counts give equal output whether asked as counts or a distribution).
func HashSettings(counts map[string]int, seed int64, options map[string]string, l Layout) string {
	var b strings.Builder
	total := 0
	for _, m := range slices.Sorted(maps.Keys(counts)) {
		if counts[m] > 0 {
			fmt.Fprintf(&b, "n.%s=%d\n", m, counts[m])
			total += counts[m]
		}
	}
	fmt.Fprintf(&b, "count=%d\nseed=%d\n", total, seed)
	for _, k := range slices.Sorted(maps.Keys(options)) {
		fmt.Fprintf(&b, "o.%s=%s\n", k, options[k])
	}
	fmt.Fprintf(&b, "ip_base=%s\nip_count=%d\nport_start=%d\ndevices_per_ip=%d\n", l.IPBase, l.IPCount, l.PortStart, l.DevicesPerIP)
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Generator defaults, used when the live build's settings are unknown.
const (
	DefaultSeed         int64 = 42
	DefaultDistribution       = "sm:40,md:40,lg:15,xl:5"
)

// ReadSettings reads dir/build.json.
func ReadSettings(dir string) (*Settings, error) {
	b, err := os.ReadFile(filepath.Join(dir, "build.json"))
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s/build.json: %w", dir, err)
	}
	return &s, nil
}

func writeSettings(dir string, s *Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(dir, "build.json")
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Chmod(p, 0o644) // whatever the controller's umask
}

// Credentials are the logins written into every manifest row.
type Credentials struct {
	Username, Password, EnablePassword string
}

// DefaultCredentials are rcfg-sim-gen's and the unit's defaults.
var DefaultCredentials = Credentials{Username: "admin", Password: "admin", EnablePassword: "enable123"}

// credentialsOf reads the logins from the first device row of a manifest.
func credentialsOf(manifest string) (Credentials, error) {
	f, err := os.Open(manifest)
	if err != nil {
		return Credentials{}, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	if _, err := r.Read(); err != nil { // header
		return Credentials{}, fmt.Errorf("%s: %w", manifest, err)
	}
	row, err := r.Read()
	if errors.Is(err, io.EOF) {
		return Credentials{}, fmt.Errorf("%s: no device rows", manifest)
	}
	if err != nil {
		return Credentials{}, fmt.Errorf("%s: %w", manifest, err)
	}
	if len(row) < 8 {
		return Credentials{}, fmt.Errorf("%s: %d columns, want at least 8", manifest, len(row))
	}
	return Credentials{Username: row[5], Password: row[6], EnablePassword: row[7]}, nil
}
