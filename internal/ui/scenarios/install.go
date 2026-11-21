package scenarios

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
)

// Slot is where a device runs on this host.
type Slot struct {
	Instance string // the instance name, which is its LISTEN_IP
	IP       string
	Port     int
}

// Place puts the devices on the host's instances in manifest order: the
// first instance first, ports from PORT_START, then the next instance
// when one is full. Hostnames are kept.
func Place(s Scenario, l jobs.Layout) ([]Slot, error) {
	if why := Fit(s, l); why != "" {
		return nil, fmt.Errorf("%s", why)
	}
	base, err := netip.ParseAddr(l.IPBase)
	if err != nil {
		return nil, fmt.Errorf("instance IP %q: %w", l.IPBase, err)
	}
	out := make([]Slot, len(s.Devices))
	for i := range s.Devices {
		k := i / l.DevicesPerIP
		ip := base
		for range k {
			ip = ip.Next()
		}
		out[i] = Slot{Instance: l.Instances[k], IP: ip.String(), Port: l.PortStart + i%l.DevicesPerIP}
	}
	return out, nil
}

// Instances is how many instances the scenario uses on this layout.
func Instances(s Scenario, l jobs.Layout) int {
	if l.DevicesPerIP < 1 {
		return 0
	}
	return (len(s.Devices) + l.DevicesPerIP - 1) / l.DevicesPerIP
}

// Fit is why the scenario cannot run on this layout, or "". A scenario
// needs no more devices than the instances have ports; unlike a generated
// fleet it need not fill every instance (the rest are not used).
func Fit(s Scenario, l jobs.Layout) string {
	capacity := l.IPCount * l.DevicesPerIP
	if len(s.Devices) > capacity {
		return fmt.Sprintf("%d devices, but this host's %d instances have %d ports", len(s.Devices), l.IPCount, capacity)
	}
	return ""
}

// Hash identifies an installed build of the scenario on this layout: the
// package's files and where its devices go. The same hash means the same
// build, so a second start reuses it.
func Hash(s Scenario, l jobs.Layout) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "scenario=%s\nip_base=%s\nport_start=%d\ndevices_per_ip=%d\n", s.ID, l.IPBase, l.PortStart, l.DevicesPerIP)
	err := fs.WalkDir(s.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(s.FS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "file=%s %d\n", p, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BuildName is the build directory name: <id>-v<version>-<hash prefix>.
func BuildName(s Scenario, hash string) string {
	return fmt.Sprintf("%s-v%d-%s", s.ID, s.Version, hash[:12])
}

// Install copies the package into dir (a new, empty build directory) and
// writes its manifest with every device moved to its slot on this host
// and config_file pointing into dir. Hostnames, credentials and the rest
// are kept.
func Install(s Scenario, dir string, l jobs.Layout) error {
	if p := s.Problem(); p != "" {
		return fmt.Errorf("scenario %s: %s", s.ID, p)
	}
	slots, err := Place(s, l)
	if err != nil {
		return err
	}
	err = fs.WalkDir(s.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." || p == "manifest.csv" {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // no symlinks out of a dropped-in package
		}
		return copyFile(s.FS, p, dst)
	})
	if err != nil {
		return fmt.Errorf("copy package: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "manifest.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write(manifestHeader)
	for i, d := range s.Devices {
		_ = w.Write([]string{d.Hostname, slots[i].IP, strconv.Itoa(slots[i].Port), d.Vendor, d.Driver,
			d.Username, d.Password, d.EnablePassword, filepath.Join(dir, filepath.FromSlash(path.Clean(d.ConfigFile))), d.Model})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func copyFile(fsys fs.FS, src, dst string) error {
	in, err := fsys.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
