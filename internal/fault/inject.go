// Package fault implements the configurable fault injector used by the SSH
// simulator. A Set is built from the --fault-rates flag, or from the legacy
// --fault-rate and --fault-types pair, and holds one rate per fault type.
// Each session consults the Set with its own *rand.Rand to decide whether a
// given event (auth, command, running-config stream) should misbehave.
//
// When the Set is nil, empty, or a type's rate is 0 the hot path is
// effectively free — Enabled/Roll short-circuit on a cheap scalar compare and
// do not touch the RNG.
package fault

import (
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
)

// Type enumerates the fault modes the simulator can inject.
type Type int

const (
	TypeAuthFail Type = iota
	TypeDisconnectMid
	TypeSlowResponse
	TypeMalformed
	numTypes // sentinel; not a real type
)

// typeNames is both the CLI-token-to-enum map and the Prometheus
// `faults_injected_total{type=…}` label value. Keep the two in sync.
var typeNames = [...]string{
	TypeAuthFail:      "auth_fail",
	TypeDisconnectMid: "disconnect_mid",
	TypeSlowResponse:  "slow_response",
	TypeMalformed:     "malformed",
}

// AllTypes is a small slice of every Type, in stable order. Handy for tests
// and for pre-registration of Prometheus label sets.
var AllTypes = []Type{TypeAuthFail, TypeDisconnectMid, TypeSlowResponse, TypeMalformed}

// String returns the CLI/label form ("auth_fail" etc.). Zero value and
// out-of-range inputs produce the empty string.
func (t Type) String() string {
	if int(t) < 0 || int(t) >= len(typeNames) {
		return ""
	}
	return typeNames[t]
}

// Set holds one injection rate per fault Type. A type is enabled when its
// rate is above zero. Built from either the legacy --fault-types +
// --fault-rate pair (NewSet) or the per-type --fault-rates form
// (ParseRates / NewSetFromRates); both produce the same Set for the same
// effective rates. Immutable once built.
// Safe to call Enabled/Roll on a nil *Set.
type Set struct {
	rates [numTypes]float64
}

// NewSet parses a comma-separated list of fault types and a [0,1] rate. Every
// listed type gets that rate. Empty typesCSV or rate==0 produce a Set where
// Empty() reports true.
//
// Returns an error for unknown types or rate out of range. Whitespace around
// tokens is tolerated ("auth_fail, slow_response" is valid).
func NewSet(typesCSV string, rate float64) (*Set, error) {
	if rate < 0 || rate > 1 {
		return nil, fmt.Errorf("--fault-rate %.3f out of [0,1]", rate)
	}
	s := &Set{}
	for _, tok := range strings.Split(typesCSV, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		t, ok := parseType(tok)
		if !ok {
			return nil, unknownType(tok)
		}
		s.rates[t] = rate
	}
	return s, nil
}

// ParseRates parses the --fault-rates form: comma-separated type=rate pairs,
// e.g. "auth_fail=0.02,disconnect_mid=0.005". Types not listed get rate 0.
// Empty input produces an empty Set. Whitespace around tokens is tolerated.
//
// Returns an error for unknown or repeated types, a missing "=", an
// unparseable rate, or a rate out of [0,1].
func ParseRates(csv string) (*Set, error) {
	rates := map[string]float64{}
	for _, tok := range strings.Split(csv, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, val, ok := strings.Cut(tok, "=")
		if !ok {
			return nil, fmt.Errorf("--fault-rates entry %q: want type=rate", tok)
		}
		name = strings.TrimSpace(name)
		if _, dup := rates[name]; dup {
			return nil, fmt.Errorf("--fault-rates: type %q listed twice", name)
		}
		r, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return nil, fmt.Errorf("--fault-rates entry %q: bad rate: %w", tok, err)
		}
		rates[name] = r
	}
	return NewSetFromRates(rates)
}

// NewSetFromRates builds a Set from a type-name to rate map, the shape the
// admin API uses. Missing types get rate 0. Returns an error for unknown
// types or a rate out of [0,1].
func NewSetFromRates(rates map[string]float64) (*Set, error) {
	s := &Set{}
	for name, r := range rates {
		t, ok := parseType(name)
		if !ok {
			return nil, unknownType(name)
		}
		if math.IsNaN(r) || r < 0 || r > 1 {
			return nil, fmt.Errorf("fault rate for %s %v out of [0,1]", name, r)
		}
		s.rates[t] = r
	}
	return s, nil
}

func unknownType(tok string) error {
	return fmt.Errorf("unknown fault type %q (valid: %s)", tok, strings.Join(typeNames[:], ", "))
}

func parseType(s string) (Type, bool) {
	for i, n := range typeNames {
		if n == s {
			return Type(i), true
		}
	}
	return 0, false
}

// Empty reports whether the Set will never inject a fault. Callers may use
// this to skip entire code paths; Enabled/Roll already handle the zero case
// cheaply, so using Empty is only worth it to avoid more expensive surrounding
// work (e.g. RNG setup).
func (s *Set) Empty() bool {
	if s == nil {
		return true
	}
	for _, r := range s.rates {
		if r > 0 {
			return false
		}
	}
	return true
}

// Enabled reports whether a specific fault type would be rolled — i.e. its
// rate is above zero. Nil-safe. Does not touch the RNG.
func (s *Set) Enabled(t Type) bool {
	if s == nil {
		return false
	}
	if int(t) < 0 || int(t) >= int(numTypes) {
		return false
	}
	return s.rates[t] > 0
}

// Roll returns true when the supplied RNG draws under the type's rate. The
// RNG is only touched when Enabled(t) is true — which means in the
// fault-disabled hot path the fast path costs one scalar compare.
// Nil-safe.
func (s *Set) Roll(rng *rand.Rand, t Type) bool {
	if !s.Enabled(t) {
		return false
	}
	return rng.Float64() < s.rates[t]
}

// Rate returns the configured rate for one type. Zero for a nil Set or an
// out-of-range type.
func (s *Set) Rate(t Type) float64 {
	if !s.Enabled(t) {
		return 0
	}
	return s.rates[t]
}

// Rates returns every type's rate keyed by its CLI/label name, including
// types at 0. Used for reporting (logs, admin API). Nil-safe: a nil Set
// reports every type at 0.
func (s *Set) Rates() map[string]float64 {
	out := make(map[string]float64, numTypes)
	for _, t := range AllTypes {
		out[t.String()] = s.Rate(t)
	}
	return out
}

// String returns the enabled types in --fault-rates form, in stable order,
// e.g. "auth_fail=0.02,disconnect_mid=0.005". Empty for an empty Set.
func (s *Set) String() string {
	var b strings.Builder
	for _, t := range s.EnabledTypes() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t.String())
		b.WriteByte('=')
		b.WriteString(strconv.FormatFloat(s.rates[t], 'g', -1, 64))
	}
	return b.String()
}

// EnabledTypes returns the Types with a rate above zero. Stable order. Empty
// slice on nil/empty Set.
func (s *Set) EnabledTypes() []Type {
	if s == nil {
		return nil
	}
	var out []Type
	for _, t := range AllTypes {
		if s.rates[t] > 0 {
			out = append(out, t)
		}
	}
	return out
}
