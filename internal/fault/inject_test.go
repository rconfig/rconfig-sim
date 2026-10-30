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

func TestParseRates(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		want    map[Type]float64
	}{
		{"", false, nil},
		{"auth_fail=0.02", false, map[Type]float64{TypeAuthFail: 0.02}},
		{"auth_fail=0.02,disconnect_mid=0.005", false, map[Type]float64{TypeAuthFail: 0.02, TypeDisconnectMid: 0.005}},
		{" slow_response = 1 , malformed=0 ", false, map[Type]float64{TypeSlowResponse: 1}},
		{"auth_fail=0,disconnect_mid=0", false, nil},
		{"auth_fail", true, nil},
		{"auth_fail=", true, nil},
		{"auth_fail=abc", true, nil},
		{"auth_fail=-0.1", true, nil},
		{"auth_fail=1.5", true, nil},
		{"auth_fail=NaN", true, nil},
		{"typo=0.1", true, nil},
		{"auth_fail=0.1,auth_fail=0.2", true, nil},
	}
	for _, tc := range cases {
		s, err := ParseRates(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRates(%q): wantErr=%v, got err=%v", tc.in, tc.wantErr, err)
			continue
		}
		if tc.wantErr {
			continue
		}
		for _, ft := range AllTypes {
			if got := s.Rate(ft); got != tc.want[ft] {
				t.Errorf("ParseRates(%q): %s rate want %v, got %v", tc.in, ft, tc.want[ft], got)
			}
			if got := s.Enabled(ft); got != (tc.want[ft] > 0) {
				t.Errorf("ParseRates(%q): %s Enabled want %v, got %v", tc.in, ft, tc.want[ft] > 0, got)
			}
		}
		if s.Empty() != (len(tc.want) == 0) {
			t.Errorf("ParseRates(%q): Empty want %v, got %v", tc.in, len(tc.want) == 0, s.Empty())
		}
	}
}

func TestNewSetFromRatesValidation(t *testing.T) {
	if _, err := NewSetFromRates(map[string]float64{"bogus": 0.1}); err == nil {
		t.Error("unknown type must error")
	}
	if _, err := NewSetFromRates(map[string]float64{"auth_fail": 2}); err == nil {
		t.Error("rate > 1 must error")
	}
	s, err := NewSetFromRates(nil)
	if err != nil || !s.Empty() {
		t.Errorf("nil map: want empty Set, got %v, err %v", s, err)
	}
}

// TestLegacyFormEquivalence pins D5: --fault-rate R --fault-types a,b is the
// same Set as --fault-rates a=R,b=R, rolled decision for decision.
func TestLegacyFormEquivalence(t *testing.T) {
	legacy, err := NewSet("auth_fail,slow_response", 0.1)
	if err != nil {
		t.Fatal(err)
	}
	perType, err := ParseRates("auth_fail=0.1,slow_response=0.1")
	if err != nil {
		t.Fatal(err)
	}
	if *legacy != *perType {
		t.Fatalf("legacy %v != per-type %v", legacy, perType)
	}
	rngA := rand.New(rand.NewSource(7))
	rngB := rand.New(rand.NewSource(7))
	for i := 0; i < 1000; i++ {
		ft := AllTypes[i%len(AllTypes)]
		if a, b := legacy.Roll(rngA, ft), perType.Roll(rngB, ft); a != b {
			t.Fatalf("i=%d type=%s: legacy=%v per-type=%v", i, ft, a, b)
		}
	}
}

// TestRollPerTypeIndependent checks each type fires at its own rate, and a
// type at 0 never touches the RNG even when its neighbours are at 1.
func TestRollPerTypeIndependent(t *testing.T) {
	s, _ := ParseRates("auth_fail=1,malformed=0.25")
	panicRng := rand.New(&panicSource{t: t})
	if s.Roll(panicRng, TypeDisconnectMid) || s.Roll(panicRng, TypeSlowResponse) {
		t.Fatal("rate-0 types must not fire")
	}

	const trials = 20000
	rng := rand.New(rand.NewSource(42))
	auth, mal := 0, 0
	for i := 0; i < trials; i++ {
		if s.Roll(rng, TypeAuthFail) {
			auth++
		}
		if s.Roll(rng, TypeMalformed) {
			mal++
		}
	}
	if auth != trials {
		t.Errorf("auth_fail at 1.0: fired %d/%d", auth, trials)
	}
	if obs := float64(mal) / trials; math.Abs(obs-0.25) > 0.015 {
		t.Errorf("malformed at 0.25: observed %.4f", obs)
	}
}

func TestRatesAndString(t *testing.T) {
	var nilSet *Set
	if got := nilSet.Rates(); len(got) != len(AllTypes) || got["auth_fail"] != 0 {
		t.Errorf("nil Rates: want every type at 0, got %v", got)
	}
	if got := nilSet.String(); got != "" {
		t.Errorf("nil String: want empty, got %q", got)
	}

	s, _ := ParseRates("malformed=0.5,auth_fail=0.02")
	r := s.Rates()
	want := map[string]float64{"auth_fail": 0.02, "disconnect_mid": 0, "slow_response": 0, "malformed": 0.5}
	for k, v := range want {
		if r[k] != v {
			t.Errorf("Rates()[%s]: want %v, got %v", k, v, r[k])
		}
	}
	if got := s.String(); got != "auth_fail=0.02,malformed=0.5" {
		t.Errorf("String: got %q", got)
	}
	// String round-trips through ParseRates.
	back, err := ParseRates(s.String())
	if err != nil || *back != *s {
		t.Errorf("round trip: got %v, err %v", back, err)
	}
}
