// Package profile implements per-device behaviour profiles: a fixed way a
// given simulated device misbehaves on every session, as opposed to the
// fleet-wide random faults in package fault.
//
// Profiles come from two places. The optional `profile` manifest column pins
// one device to one profile. The --device-profiles flag assigns profiles to a
// percentage of the fleet; which devices are chosen depends only on the seed
// and the hostname, so the same devices misbehave on every run, on every host,
// however the fleet is split across listeners.
package profile

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"
)

// Kind enumerates the profile behaviours.
type Kind int

const (
	// KindHang: after login the device answers nothing from its first command
	// for Hang, then answers every queued command normally. Each new session
	// hangs again.
	KindHang Kind = iota + 1
	// KindLatency: every command waits uniform[LatencyMin, LatencyMax] instead
	// of the global --response-delay-ms-min/max jitter.
	KindLatency
	// KindRefuse: the TCP connection is accepted and closed before the SSH
	// handshake.
	KindRefuse
	// KindDisconnect: every config stream is cut part way, exactly as the
	// disconnect_mid fault does at rate 1.
	KindDisconnect
)

var kindNames = map[Kind]string{
	KindHang:       "hang",
	KindLatency:    "latency",
	KindRefuse:     "refuse",
	KindDisconnect: "disconnect",
}

// Profile is one device's behaviour. A nil *Profile is a normal device.
type Profile struct {
	Kind       Kind
	Hang       time.Duration
	LatencyMin time.Duration
	LatencyMax time.Duration
}

// String returns the spec form, e.g. "hang:1h0m0s" or "latency:200ms-800ms".
func (p *Profile) String() string {
	if p == nil {
		return ""
	}
	switch p.Kind {
	case KindHang:
		return "hang:" + p.Hang.String()
	case KindLatency:
		if p.LatencyMin == p.LatencyMax {
			return "latency:" + p.LatencyMin.String()
		}
		return "latency:" + p.LatencyMin.String() + "-" + p.LatencyMax.String()
	default:
		return kindNames[p.Kind]
	}
}

// Is reports whether p is a profile of kind k. Nil-safe.
func (p *Profile) Is(k Kind) bool {
	return p != nil && p.Kind == k
}

// HangDuration returns how long the device hangs per session; zero unless the
// profile is a hang. Nil-safe.
func (p *Profile) HangDuration() time.Duration {
	if p.Is(KindHang) {
		return p.Hang
	}
	return 0
}

// Rule assigns Profile to Fraction (0,1] of the fleet.
type Rule struct {
	Profile  Profile
	Fraction float64
}

// ParseProfile parses one profile without a percentage, as used in the
// manifest column: "hang:3600s", "latency:200-800ms", "latency:500ms",
// "refuse", "disconnect". An empty string is no profile (nil, nil).
func ParseProfile(s string) (*Profile, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	kindName, arg, hasArg := strings.Cut(s, ":")

	switch kindName {
	case "hang":
		if !hasArg {
			return nil, fmt.Errorf("profile %q: hang needs a duration, e.g. hang:3600s", s)
		}
		d, err := time.ParseDuration(arg)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("profile %q: hang duration must be positive, e.g. 3600s or 1h", s)
		}
		return &Profile{Kind: KindHang, Hang: d}, nil

	case "latency":
		if !hasArg {
			return nil, fmt.Errorf("profile %q: latency needs a delay, e.g. latency:200-800ms", s)
		}
		lo, hi, err := parseRange(arg)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", s, err)
		}
		return &Profile{Kind: KindLatency, LatencyMin: lo, LatencyMax: hi}, nil

	case "refuse", "disconnect":
		if hasArg {
			return nil, fmt.Errorf("profile %q: %s takes no argument", s, kindName)
		}
		if kindName == "refuse" {
			return &Profile{Kind: KindRefuse}, nil
		}
		return &Profile{Kind: KindDisconnect}, nil
	}

	return nil, fmt.Errorf("unknown profile %q (valid: hang:<duration>, latency:<min>-<max>, refuse, disconnect)", s)
}

// parseRange reads "500ms" or "200-800ms". A bare number on the left takes the
// unit of the right, so "200-800ms" means 200ms to 800ms.
func parseRange(s string) (time.Duration, time.Duration, error) {
	left, right, isRange := strings.Cut(s, "-")
	if !isRange {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return 0, 0, fmt.Errorf("bad delay %q", s)
		}
		return d, d, nil
	}

	hi, err := time.ParseDuration(right)
	if err != nil {
		return 0, 0, fmt.Errorf("bad delay %q", right)
	}
	if _, numErr := strconv.ParseFloat(left, 64); numErr == nil {
		left += strings.TrimLeft(right, "0123456789.")
	}
	lo, err := time.ParseDuration(left)
	if err != nil {
		return 0, 0, fmt.Errorf("bad delay %q", left)
	}
	if lo < 0 || hi < lo {
		return 0, 0, fmt.Errorf("delay range %q must be min-max with 0 <= min <= max", s)
	}
	return lo, hi, nil
}

// Parse reads the --device-profiles flag: comma-separated profile@percent
// items, e.g. "hang:3600s@3%,latency:200-800ms@10%". The percentages may add
// up to at most 100. An empty spec gives no rules.
func Parse(spec string) ([]Rule, error) {
	var rules []Rule
	total := 0.0

	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		profileSpec, pctSpec, ok := strings.Cut(item, "@")
		if !ok {
			return nil, fmt.Errorf("device profile %q: missing @percent, e.g. hang:3600s@3%%", item)
		}
		pct, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(pctSpec), "%"), 64)
		if err != nil || pct <= 0 || pct > 100 {
			return nil, fmt.Errorf("device profile %q: percent must be in (0,100]", item)
		}
		p, err := ParseProfile(profileSpec)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("device profile %q: missing profile before @", item)
		}
		total += pct
		rules = append(rules, Rule{Profile: *p, Fraction: pct / 100})
	}

	if total > 100+1e-9 {
		return nil, fmt.Errorf("device profiles add up to %.4g%%, more than 100%%", total)
	}
	return rules, nil
}

// Assign returns the profile the rules give hostname under seed, or nil for a
// normal device. The hostname and seed are hashed to a point in [0,1) and the
// rules' fractions are laid end to end, so each rule takes its share of the
// fleet and no device gets two.
func Assign(hostname string, seed int64, rules []Rule) *Profile {
	if len(rules) == 0 {
		return nil
	}
	h := fnv.New64a()
	var seedBytes [8]byte
	binary.LittleEndian.PutUint64(seedBytes[:], uint64(seed))
	_, _ = h.Write(seedBytes[:])
	_, _ = h.Write([]byte(hostname))
	point := float64(mix64(h.Sum64())>>11) / float64(uint64(1)<<53)

	edge := 0.0
	for _, r := range rules {
		edge += r.Fraction
		if point < edge {
			p := r.Profile
			return &p
		}
	}
	return nil
}

// mix64 is the SplitMix64 finaliser. FNV-1a alone barely moves its top bits
// between hostnames that differ only in their last characters (rtr-00001,
// rtr-00002, ...), which skewed the shares; mixing spreads every input bit
// across the whole word.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}
