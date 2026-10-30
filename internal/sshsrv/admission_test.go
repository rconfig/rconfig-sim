package sshsrv

import (
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

func TestDeviceActive(t *testing.T) {
	s := &Server{cfg: Config{PortStart: 10000}}
	cases := []struct {
		active int
		port   int
		want   bool
	}{
		{3, 10000, true},
		{3, 10002, true},
		{3, 10003, false},
		{0, 10000, false},
		{2500, 12499, true},
	}
	for _, tc := range cases {
		live := &runtime.Live{ActiveDevices: tc.active}
		if got := s.deviceActive(live, tc.port); got != tc.want {
			t.Errorf("active=%d port=%d: want %v, got %v", tc.active, tc.port, tc.want, got)
		}
	}
}

func TestAcquireSessionCap(t *testing.T) {
	s := &Server{}
	live := &runtime.Live{MaxSessions: 2}
	if !s.acquireSession(live) || !s.acquireSession(live) {
		t.Fatal("first two sessions must be admitted")
	}
	if s.acquireSession(live) {
		t.Fatal("third session must be refused at cap 2")
	}
	if got := s.sessions.Load(); got != 2 {
		t.Fatalf("a refused session must not hold a slot: count %d", got)
	}

	// Raising the cap in a new generation admits more at once; lowering it
	// leaves admitted sessions alone and refuses new ones until they drain.
	if !s.acquireSession(&runtime.Live{MaxSessions: 3}) {
		t.Fatal("cap raised to 3: third session must be admitted")
	}
	lower := &runtime.Live{MaxSessions: 1}
	if s.acquireSession(lower) {
		t.Fatal("cap lowered to 1 with 3 in flight: must refuse")
	}
	s.releaseSession()
	s.releaseSession()
	if s.acquireSession(lower) {
		t.Fatal("1 in flight at cap 1: must refuse")
	}
	s.releaseSession()
	if !s.acquireSession(lower) {
		t.Fatal("0 in flight at cap 1: must admit")
	}
}

// BenchmarkSessionAdmission is the per-session gate in handleConn: one
// runtime load, the profile lookup, the active-devices check and the session
// counter. It must not allocate.
//
// Run with:  go test -run XXX -bench BenchmarkSessionAdmission ./internal/sshsrv/
func BenchmarkSessionAdmission(b *testing.B) {
	s := &Server{
		cfg:     Config{PortStart: 10000},
		runtime: runtime.NewHolder(&runtime.Live{MaxSessions: 5000, ActiveDevices: 2500}),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		live := s.runtime.Load()
		port := 10000 + i%2500
		prof := live.Profiles[port]
		if !s.deviceActive(live, port) || prof != nil {
			b.Fatal("device must be active and unprofiled")
		}
		if !s.acquireSession(live) {
			b.Fatal("must admit")
		}
		s.releaseSession()
	}
}

func TestSessionAdmissionAllocs(t *testing.T) {
	res := testing.Benchmark(BenchmarkSessionAdmission)
	if a := res.AllocsPerOp(); a != 0 {
		t.Fatalf("session admission allocates: %d allocs/op", a)
	}
}
