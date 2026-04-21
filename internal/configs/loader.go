package configs

import (
	"encoding/csv"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Device is a single simulated network device: port, hostname, and an mmap'd
// view of its rendered config. Data is MAP_SHARED/PROT_READ — never written.
type Device struct {
	Hostname     string
	IP           string
	Port         int
	SizeBucket   string
	ConfigPath   string
	Data         []byte
	Size         int64
	SerialNumber string
}

// LoadForListener reads the generator's manifest, selects rows matching the
// listener's IP and port range, and mmaps each config file read-only.
// On any mmap error, previously-mapped devices are unmapped so no leak occurs.
func LoadForListener(manifestPath, listenIP string, portStart, portCount int) ([]*Device, error) {
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(rows) < 1 {
		return nil, fmt.Errorf("manifest empty")
	}

	var devices []*Device
	portEnd := portStart + portCount
	for i, row := range rows {
		if i == 0 {
			continue // header
		}
		if len(row) < 10 {
			return nil, fmt.Errorf("manifest row %d: %d columns, want 10", i, len(row))
		}
		if row[1] != listenIP {
			continue
		}
		port, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, fmt.Errorf("manifest row %d: bad port %q: %w", i, row[2], err)
		}
		if port < portStart || port >= portEnd {
			continue
		}
		devices = append(devices, &Device{
			Hostname:     row[0],
			IP:           row[1],
			Port:         port,
			ConfigPath:   row[8],
			SizeBucket:   row[9],
			SerialNumber: serialFor(row[0]),
		})
	}

	for _, d := range devices {
		b, size, err := mmapRO(d.ConfigPath)
		if err != nil {
			UnloadAll(devices)
			return nil, fmt.Errorf("mmap %s: %w", d.ConfigPath, err)
		}
		d.Data = b
		d.Size = size
	}
	return devices, nil
}

// UnloadAll releases every mmap held by the given devices. Safe to call multiple times.
func UnloadAll(devices []*Device) {
	for _, d := range devices {
		if d.Data != nil {
			_ = unix.Munmap(d.Data)
			d.Data = nil
		}
	}
}

func mmapRO(path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, 0, nil
	}
	b, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, 0, err
	}
	return b, size, nil
}
