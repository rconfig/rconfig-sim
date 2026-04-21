package fault

import (
	"math"
	"math/rand"
	"testing"
)

func TestNewSetParsing(t *testing.T) {
	cases := []struct {
		in        string
		rate      float64
		wantErr   bool
		wantTypes []Type
	}{
		{"", 0.0, false, nil},
		{"", 0.5, false, nil},
		{"auth_fail", 0.1, false, []Type{TypeAuthFail}},
		{"auth_fail,slow_response", 0.1, false, []Type{TypeAuthFail, TypeSlowResponse}},
		{"  auth_fail ,  malformed  ", 0.1, false, []Type{TypeAuthFail, TypeMalformed}},
		{"all,things", 0.1, true, nil},
		{"auth_fail,typo", 0.1, true, nil},
		{"auth_fail", -0.1, true, nil},
		{"auth_fail", 1.1, true, nil},
	}
	for _, tc := range cases {
		s, err := NewSet(tc.in, tc.rate)
		gotErr := err != nil
		if gotErr != tc.wantErr {
			t.Errorf("NewSet(%q, %v): wantErr=%v, got err=%v", tc.in, tc.rate, tc.wantErr, err)
			continue
		}
		if tc.wantErr {
			continue
		}
		got := s.EnabledTypes()
		if len(got) != len(tc.wantTypes) {
			t.Errorf("NewSet(%q): want types %v, got %v", tc.in, tc.wantTypes, got)
			continue
		}
		for i, w := range tc.wantTypes {
			if got[i] != w {
				t.Errorf("NewSet(%q): idx %d: want %v, got %v", tc.in, i, w, got[i])
			}
		}
	}
}

func TestSetEmptyConditions(t *testing.T) {
	// nil receiver
	var nilSet *Set
	if !nilSet.Empty() {
		t.Error("nil *Set should be Empty")
	}

	// rate=0 with types
	s, _ := NewSet("auth_fail,malformed", 0.0)
	if !s.Empty() {
		t.Error("rate=0 Set should be Empty regardless of types")
	}

	// rate>0 but no types
	s, _ = NewSet("", 0.5)
	if !s.Empty() {
		t.Error("empty types Set should be Empty regardless of rate")
	}

	// both non-empty
	s, _ = NewSet("auth_fail", 0.5)
	if s.Empty() {
		t.Error("rate>0 + auth_fail should NOT be Empty")
	}
}

// TestRollDisabledReturnsFalse confirms the hot-path short-circuits: a
// nil Set, a rate=0 Set, or a Set missing the target type must all skip
// the RNG call. We prove the RNG-skip by using a rand.Source that panics.
func TestRollDisabledReturnsFalse(t *testing.T) {
	var nilSet *Set
	panicRng := rand.New(&panicSource{t: t})

	if nilSet.Roll(panicRng, TypeAuthFail) {
		t.Error("nil Set must not fire")
	}

	zeroRate, _ := NewSet("auth_fail", 0.0)
	if zeroRate.Roll(panicRng, TypeAuthFail) {
		t.Error("rate=0 Set must not fire")
	}

	typed, _ := NewSet("auth_fail", 1.0)
	if typed.Roll(panicRng, TypeMalformed) {
		t.Error("Type not listed must not fire")
	}
}

// TestRollRateWithinTolerance drives Roll 20000 times against a seeded RNG
// and asserts the fire rate is within 1.5 percentage points of the configured
// rate. The test is deterministic — same seed every run.
func TestRollRateWithinTolerance(t *testing.T) {
	const trials = 20000
	const tolerance = 0.015

	for _, rate := range []float64{0.01, 0.1, 0.5, 0.9} {
		s, _ := NewSet("auth_fail", rate)
		rng := rand.New(rand.NewSource(42))
		fires := 0
		for i := 0; i < trials; i++ {
			if s.Roll(rng, TypeAuthFail) {
				fires++
			}
		}
		observed := float64(fires) / float64(trials)
		if math.Abs(observed-rate) > tolerance {
			t.Errorf("rate=%.2f: observed %.4f over %d trials, tolerance=%.3f",
				rate, observed, trials, tolerance)
		}
	}
}

// TestRollDeterministic verifies that two Sets seeded identically produce the
// same sequence of decisions. Important for reproducing fault-injection bugs.
func TestRollDeterministic(t *testing.T) {
	s, _ := NewSet("auth_fail", 0.3)

	runA, runB := make([]bool, 1000), make([]bool, 1000)
	rngA := rand.New(rand.NewSource(12345))
	rngB := rand.New(rand.NewSource(12345))
	for i := 0; i < 1000; i++ {
		runA[i] = s.Roll(rngA, TypeAuthFail)
		runB[i] = s.Roll(rngB, TypeAuthFail)
	}
	for i := range runA {
		if runA[i] != runB[i] {
			t.Fatalf("diverged at i=%d (A=%v B=%v)", i, runA[i], runB[i])
		}
	}
}

// BenchmarkRollDisabled measures the overhead of Roll when no faults should
// fire — this is the cost paid by every command dispatch in production when
// fault injection is off. It should sit in the single-digit nanosecond range.
func BenchmarkRollDisabled(b *testing.B) {
	s, _ := NewSet("auth_fail,disconnect_mid,slow_response,malformed", 0.0)
	rng := rand.New(rand.NewSource(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Roll(rng, TypeAuthFail)
		_ = s.Roll(rng, TypeDisconnectMid)
		_ = s.Roll(rng, TypeSlowResponse)
		_ = s.Roll(rng, TypeMalformed)
	}
}

// BenchmarkRollNilSet confirms the nil-receiver path is free.
func BenchmarkRollNilSet(b *testing.B) {
	var s *Set
	rng := rand.New(rand.NewSource(1))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Roll(rng, TypeAuthFail)
	}
}

// panicSource is a rand.Source that panics if used. We pair it with a Set
// we expect to short-circuit before hitting the RNG; if it does get called,
// the test fails with a descriptive panic message.
type panicSource struct{ t *testing.T }

func (p *panicSource) Int63() int64 {
	p.t.Fatal("RNG was consulted but the Set should have short-circuited")
	return 0
}
func (p *panicSource) Seed(int64) {}
