// Package fault implements the configurable fault injector used by the SSH
// simulator. A single Set is built at startup from the --fault-rate and
// --fault-types flags; every session consults the Set with its own *rand.Rand
// to decide whether a given event (auth, command, running-config stream)
// should misbehave.
//
// When the Set is nil, empty, or configured with rate=0 the hot path is
// effectively free — Enabled/Roll short-circuit on a cheap scalar compare and
// do not touch the RNG.
package fault

import (
	"fmt"
	"math/rand"
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

// Set captures a parsed --fault-types + --fault-rate pair.
// Safe to call Enabled/Roll on a nil *Set.
type Set struct {
	enabled [numTypes]bool
	rate    float64
}

// NewSet parses a comma-separated list of fault types and a [0,1] rate.
// Empty typesCSV or rate==0 produce a Set where Empty() reports true.
//
// Returns an error for unknown types or rate out of range. Whitespace around
// tokens is tolerated ("auth_fail, slow_response" is valid).
func NewSet(typesCSV string, rate float64) (*Set, error) {
	if rate < 0 || rate > 1 {
		return nil, fmt.Errorf("--fault-rate %.3f out of [0,1]", rate)
	}
	s := &Set{rate: rate}
	for _, tok := range strings.Split(typesCSV, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		t, ok := parseType(tok)
		if !ok {
			return nil, fmt.Errorf("unknown fault type %q (valid: %s)", tok, strings.Join(typeNames[:], ", "))
		}
		s.enabled[t] = true
	}
	return s, nil
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
	if s == nil || s.rate == 0 {
		return true
	}
	for _, e := range s.enabled {
		if e {
			return false
		}
	}
	return true
}

// Enabled reports whether a specific fault type would be rolled — i.e. it is
// listed in --fault-types AND rate > 0. Nil-safe. Does not touch the RNG.
func (s *Set) Enabled(t Type) bool {
	if s == nil || s.rate == 0 {
		return false
	}
	if int(t) < 0 || int(t) >= int(numTypes) {
		return false
	}
	return s.enabled[t]
}

// Roll returns true when the supplied RNG draws under the rate AND the type
// is enabled. The RNG is only touched when Enabled(t) is true — which means
// in the fault-disabled hot path the fast path costs one scalar compare.
// Nil-safe.
func (s *Set) Roll(rng *rand.Rand, t Type) bool {
	if !s.Enabled(t) {
		return false
	}
	return rng.Float64() < s.rate
}

// Rate returns the configured rate. Used by tests and the server log line.
func (s *Set) Rate() float64 {
	if s == nil {
		return 0
	}
	return s.rate
}

// EnabledTypes returns the Types currently active. Stable order. Empty slice
// on nil/empty Set.
func (s *Set) EnabledTypes() []Type {
	if s == nil {
		return nil
	}
	var out []Type
	for _, t := range AllTypes {
		if s.enabled[t] {
			out = append(out, t)
		}
	}
	return out
}
