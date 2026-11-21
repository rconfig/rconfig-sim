package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
)

// OpenEvents opens an instance's /admin/v1/events stream at the given sample
// rate. The caller reads the SSE body and closes it; cancelling ctx closes
// the connection.
func (f *Fleet) OpenEvents(ctx context.Context, name string, sample float64) (*http.Response, error) {
	v, ok := f.View(name)
	if !ok {
		return nil, fmt.Errorf("no instance %q", name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+v.AdminAddr+admin.Prefix+"events?sample="+strconv.FormatFloat(sample, 'g', -1, 64), nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("events: status %d", resp.StatusCode)
	}
	return resp, nil
}

// FetchDevices reads an instance's /admin/v1/devices.
func (f *Fleet) FetchDevices(ctx context.Context, name string) (*admin.Devices, error) {
	v, ok := f.View(name)
	if !ok {
		return nil, fmt.Errorf("no instance %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := f.adminRequest(ctx, http.MethodGet, v.AdminAddr, "devices", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("devices: status %d", resp.StatusCode)
	}
	var d admin.Devices
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("devices: %w", err)
	}
	return &d, nil
}

// Hostnames maps each instance to its hostnames by port offset (index i is
// port PortStart+i), from the manifest. Ports with no manifest row are "".
func (f *Fleet) Hostnames() map[string][]string {
	out := map[string][]string{}
	for _, v := range f.Views() {
		if v.Err != nil {
			continue
		}
		hosts := make([]string, v.PortCount)
		if rows, err := readManifest(v.Manifest); err == nil {
			for _, r := range rows {
				if r.ip == v.ListenIP && r.port >= v.PortStart && r.port < v.PortStart+v.PortCount {
					hosts[r.port-v.PortStart] = r.host
				}
			}
		}
		out[v.Name] = hosts
	}
	return out
}

// DeviceInfo describes one device for the Activity side panel.
type DeviceInfo struct {
	Instance string
	Host     string
	IP       string
	Port     int
	Profile  string // "" for a normal device
	Pinned   bool   // the profile comes from the manifest column
}

// Device returns what the manifest and the instance's profile config say
// about the device at port offset index. The profile is worked out the way
// rcfg-sim does it: the manifest's pinned profile, else the live rules and
// seed (the env file's, when the instance is not up).
func (f *Fleet) Device(name string, index int) (DeviceInfo, error) {
	v, ok := f.View(name)
	if !ok {
		return DeviceInfo{}, fmt.Errorf("no instance %q", name)
	}
	if index < 0 || index >= v.PortCount {
		return DeviceInfo{}, fmt.Errorf("device %d out of range 0..%d", index, v.PortCount-1)
	}
	d := DeviceInfo{Instance: name, IP: v.ListenIP, Port: v.PortStart + index}
	rows, err := readManifest(v.Manifest)
	if err != nil {
		return d, err
	}
	pinned := ""
	for _, r := range rows {
		if r.ip == v.ListenIP && r.port == d.Port {
			d.Host, pinned = r.host, r.pinned
			break
		}
	}
	if d.Host == "" {
		return d, nil
	}
	if pinned != "" {
		if p, err := profile.ParseProfile(pinned); err == nil && p != nil {
			d.Profile, d.Pinned = p.String(), true
		}
		return d, nil
	}
	var seed int64
	var rules []string
	switch {
	case v.State != nil:
		seed, rules = v.State.Profiles.Seed, v.State.Profiles.Rules
	case v.Desired != nil:
		seed, rules = v.Desired.ProfileSeed, v.Desired.ProfileRules
	}
	parsed, err := profile.Parse(strings.Join(rules, ","))
	if err != nil {
		return d, err
	}
	if p := profile.Assign(d.Host, seed, parsed); p != nil {
		d.Profile = p.String()
	}
	return d, nil
}
