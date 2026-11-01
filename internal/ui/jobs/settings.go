package jobs

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Distribution string            `json:"distribution,omitempty"` // "": unknown (adopted build)
	IPBase       string            `json:"ip_base,omitempty"`
	IPCount      int               `json:"ip_count,omitempty"`
	PortStart    int               `json:"port_start,omitempty"`
	DevicesPerIP int               `json:"devices_per_ip,omitempty"`
	Options      map[string]string `json:"options,omitempty"`
	Previous     string            `json:"previous,omitempty"` // the build that was live before this one
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

// credentialsOf reads the logins from the first device row of a manifest.
// A new build reuses them, so rConfig's stored credentials keep working.
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
