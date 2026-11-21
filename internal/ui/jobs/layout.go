// Package jobs runs the controller's build jobs (docs/UI-ARCHITECTURE.md
// 7.5): regenerate the fleet's configs with rcfg-sim-gen into a new build
// directory, swap the data/current symlink to it, and restart the fleet
// one instance at a time; and roll back to the previous build.
//
// A build never renames or rewrites a live build. Each build lives in
// data/builds/<name>/ with its own configs/, manifest.csv and build.json,
// and the instances read data/current/manifest.csv.
package jobs

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// Layout is the existing instances' layout, in the generator's terms.
type Layout struct {
	Root         string // the data directory, e.g. /opt/rcfg-sim/data
	Manifest     string // every instance's MANIFEST: Root/current/manifest.csv
	IPBase       string
	IPCount      int
	PortStart    int
	DevicesPerIP int
	Instances    []string // instance names, in IP order
	EnvDir       string
	metrics      []string // each instance's METRICS_ADDR, for the add-IPs steps
	admins       []string // each instance's ADMIN_ADDR, likewise
}

// NotAdoptedError means the install has not been moved to the data/ layout,
// so the Build screen shows the adoption steps instead of building.
type NotAdoptedError struct{ Reason string }

func (e *NotAdoptedError) Error() string { return "not adopted: " + e.Reason }

// LayoutOf checks that the instances form a layout the generator can
// reproduce (consecutive IPs, the same PORT_START, PORT_COUNT and MANIFEST
// everywhere) and that MANIFEST is data/current/manifest.csv.
func LayoutOf(insts []fleet.Instance, envDir string) (Layout, error) {
	l := Layout{EnvDir: envDir}
	if len(insts) == 0 {
		return l, errors.New("no instances: each *.env in the env directory is one")
	}
	var first netip.Addr
	for i, in := range insts {
		if in.Err != nil {
			return l, fmt.Errorf("instance %s: env file: %v", in.Name, in.Err)
		}
		ip, err := netip.ParseAddr(in.ListenIP)
		if err != nil || !ip.Is4() {
			return l, fmt.Errorf("instance %s: LISTEN_IP %q is not an IPv4 address; the generator lays devices out on consecutive IPv4 addresses", in.Name, in.ListenIP)
		}
		if i == 0 {
			first = ip
			l.IPBase, l.PortStart, l.DevicesPerIP, l.Manifest = in.ListenIP, in.PortStart, in.PortCount, in.Manifest
		} else {
			if want := addIP(first, i); ip != want {
				return l, fmt.Errorf("instances are not on consecutive IPs: expected %s after %s, found %s", want, insts[i-1].ListenIP, in.ListenIP)
			}
			switch {
			case in.PortStart != l.PortStart:
				return l, fmt.Errorf("instance %s: PORT_START %d differs from %d; every instance needs the same", in.Name, in.PortStart, l.PortStart)
			case in.PortCount != l.DevicesPerIP:
				return l, fmt.Errorf("instance %s: PORT_COUNT %d differs from %d; every instance needs the same", in.Name, in.PortCount, l.DevicesPerIP)
			case in.Manifest != l.Manifest:
				return l, fmt.Errorf("instance %s: MANIFEST %s differs from %s; every instance needs the same", in.Name, in.Manifest, l.Manifest)
			}
		}
		l.Instances = append(l.Instances, in.Name)
		l.metrics = append(l.metrics, in.MetricsAddr)
		l.admins = append(l.admins, in.AdminAddr)
	}
	l.IPCount = len(insts)

	dir := filepath.Dir(l.Manifest)
	if filepath.Base(l.Manifest) != "manifest.csv" || filepath.Base(dir) != "current" {
		return l, &NotAdoptedError{Reason: "MANIFEST is " + l.Manifest + ", not <data>/current/manifest.csv"}
	}
	l.Root = filepath.Dir(dir)
	fi, err := os.Lstat(dir)
	if err != nil {
		return l, &NotAdoptedError{Reason: err.Error()}
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return l, &NotAdoptedError{Reason: dir + " is not a symlink to a build"}
	}
	return l, nil
}

func addIP(a netip.Addr, n int) netip.Addr {
	for ; n > 0; n-- {
		a = a.Next()
	}
	return a
}

// CountBounds is the range of device counts the layout takes: every
// instance must get at least one device (the generator fills IPs in order,
// and an instance with none refuses to start), and no more than the layout
// holds.
func (l Layout) CountBounds() (min, max int) {
	return (l.IPCount-1)*l.DevicesPerIP + 1, l.IPCount * l.DevicesPerIP
}

// CountError is a device count the layout cannot take. For a count needing
// more IPs, Steps are the manual steps to add them.
type CountError struct {
	Count, Min, Max int
	Instances       int
	Steps           []string
}

func (e *CountError) Error() string {
	if e.Count > e.Max {
		return fmt.Sprintf("%d devices need more IP aliases than the %d instances configured hold (at most %d); add them first (steps below), then build again",
			e.Count, e.Instances, e.Max)
	}
	return fmt.Sprintf("%d devices would leave an instance with none (the minimum for this layout is %d). To run fewer devices, use Active devices on the Fleet screen",
		e.Count, e.Min)
}

// CheckCount returns a *CountError for a count outside CountBounds.
func (l Layout) CheckCount(count int) error {
	lo, hi := l.CountBounds()
	if count >= lo && count <= hi {
		return nil
	}
	e := &CountError{Count: count, Min: lo, Max: hi, Instances: l.IPCount}
	if count > hi {
		e.Steps = l.addIPSteps((count + l.DevicesPerIP - 1) / l.DevicesPerIP)
	}
	return e
}

// addIPSteps are the root-only steps to grow the layout to ipCount
// instances: aliases, env files, and enabling (not starting) the units,
// which have no devices until the next build.
func (l Layout) addIPSteps(ipCount int) []string {
	base, _ := netip.ParseAddr(l.IPBase)
	extra := ipCount - l.IPCount
	next := addIP(base, l.IPCount)
	tmpl := filepath.Join(l.EnvDir, l.Instances[0]+".env")
	steps := []string{
		fmt.Sprintf("sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface <iface> --base-ip %s --count %d   # the interface the existing aliases are on; also add them to your boot-time alias setup", next, extra),
	}
	for i := 0; i < extra; i++ {
		ip := addIP(next, i).String()
		env := filepath.Join(l.EnvDir, ip+".env")
		admin := ""
		if a := l.adminFor(l.IPCount + i); a != "" {
			admin = "; s|^ADMIN_ADDR=.*|ADMIN_ADDR=" + a + "|"
		}
		steps = append(steps,
			fmt.Sprintf("sudo install -m 0660 -g rcfgsim-ui %s %s", tmpl, env),
			fmt.Sprintf("sudo sed -i 's|^LISTEN_IP=.*|LISTEN_IP=%s|; s|^METRICS_ADDR=.*|METRICS_ADDR=%s|; s|^HOST_KEY=.*|HOST_KEY=/opt/rcfg-sim/host-keys/%s.key|%s' %s",
				ip, l.metricsFor(l.IPCount+i, ip), ip, admin, env),
			fmt.Sprintf("sudo systemctl enable rcfg-sim@%s   # enable only: it has no devices until the build", ip),
		)
	}
	return steps
}

// metricsFor suggests a METRICS_ADDR for a new instance at index i: the
// same port on its own IP when the existing instances listen on their own
// IPs, otherwise the next port after the highest in use.
func (l Layout) metricsFor(i int, ip string) string {
	if len(l.metrics) == 0 {
		return ip + ":9100"
	}
	host, port, err := net.SplitHostPort(l.metrics[0])
	if err == nil && host == l.IPBase {
		return net.JoinHostPort(ip, port)
	}
	high := 0
	for _, m := range l.metrics {
		if _, p, err := net.SplitHostPort(m); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > high {
				high = n
			}
		}
	}
	if err != nil || high == 0 {
		return ip + ":9100"
	}
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(high+i-l.IPCount+1))
}

// adminFor suggests an ADMIN_ADDR for a new instance at index i: loopback,
// the next port after the highest in use. Empty when the template
// instance's admin API is off, so the copy's is off too.
func (l Layout) adminFor(i int) string {
	if len(l.admins) == 0 || l.admins[0] == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(l.admins[0])
	if err != nil {
		return ""
	}
	high := 0
	for _, a := range l.admins {
		if _, p, err := net.SplitHostPort(a); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > high {
				high = n
			}
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(high+i-l.IPCount+1))
}

// WritableError means this process cannot write the data directory. The
// directory comes from MANIFEST, but the controller unit may only write
// /opt/rcfg-sim/data, so a MANIFEST elsewhere fails here (section 7.5).
type WritableError struct {
	Dir string
	Err error
}

func (e *WritableError) Error() string {
	return fmt.Sprintf("cannot write the data directory %s (%v). The UI unit may write only /opt/rcfg-sim/data: keep the build data there, owned root:rcfgsim-ui 2775 (make adopt-data sets this up)", e.Dir, e.Err)
}

// checkWritable creates and removes a file in root (where current is
// swapped) and in root/builds (where builds are made).
func checkWritable(root string) error {
	for _, dir := range []string{root, filepath.Join(root, "builds")} {
		f, err := os.CreateTemp(dir, ".write-check-*")
		if err != nil {
			return &WritableError{Dir: dir, Err: err}
		}
		name := f.Name()
		f.Close()
		if err := os.Remove(name); err != nil {
			return &WritableError{Dir: dir, Err: err}
		}
	}
	return nil
}
