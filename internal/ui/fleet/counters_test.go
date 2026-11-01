package fleet

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
)

const sampleMetrics = `# HELP rcfgsim_active_sessions x
# TYPE rcfgsim_active_sessions gauge
rcfgsim_active_sessions 12
rcfgsim_sessions_total{result="ok"} 100
rcfgsim_sessions_total{result="auth_fail"} 5
rcfgsim_bytes_sent_total 1.5e+06
rcfgsim_faults_injected_total{type="auth_fail"} 3
rcfgsim_faults_injected_total{type="malformed"} 0
rcfgsim_session_duration_seconds_bucket{le="0.1"} 7
rcfgsim_bytes_sent_totalx 99
`

func TestParseCounters(t *testing.T) {
	c, err := ParseCounters(strings.NewReader(sampleMetrics))
	if err != nil {
		t.Fatal(err)
	}
	if c.ActiveSessions != 12 || c.Sessions["ok"] != 100 || c.Sessions["auth_fail"] != 5 || c.Bytes != 1.5e6 || c.Faults["auth_fail"] != 3 {
		t.Fatalf("counters: %+v", c)
	}
	if _, err := ParseCounters(strings.NewReader("other_metric 1\n")); err == nil {
		t.Fatal("no rcfgsim series must be an error")
	}
}

func TestCounterDelta(t *testing.T) {
	for _, c := range []struct{ prev, cur, want float64 }{
		{10, 15, 5},
		{10, 10, 0},
		{10, 3, 3}, // reset: the instance restarted and has counted 3 since
		{0, 0, 0},
	} {
		if got := CounterDelta(c.prev, c.cur); got != c.want {
			t.Errorf("CounterDelta(%v, %v) = %v, want %v", c.prev, c.cur, got, c.want)
		}
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestRecordRatesAndReset drives record directly with fixed times: rates are
// summed across instances, and a counter reset (instance restart) counts
// from zero instead of going negative.
func TestRecordRatesAndReset(t *testing.T) {
	f := New(Options{})
	t0 := time.Unix(1_000_000, 0)
	at := func(sec int, name string, sessions, bytes, faults, active float64) View {
		return View{Instance: Instance{Name: name}, counters: &counterSample{
			at: t0.Add(time.Duration(sec) * time.Second),
			c: Counters{ActiveSessions: active, Bytes: bytes,
				Sessions: map[string]float64{"ok": sessions}, Faults: map[string]float64{"auth_fail": faults}},
		}}
	}
	poll := func(sec int, vs ...View) []View {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.record(vs, t0.Add(time.Duration(sec)*time.Second))
		return vs
	}

	poll(0, at(0, "a", 100, 1000, 0, 1), at(0, "b", 50, 0, 0, 2))
	if len(f.Series()) != 0 {
		t.Fatal("first scrape has nothing to difference against")
	}
	vs := poll(2, at(2, "a", 110, 3000, 4, 3), at(2, "b", 54, 500, 0, 4))
	s := f.Series()
	if len(s) != 1 {
		t.Fatalf("series: %+v", s)
	}
	p := s[0]
	// a: 10 sessions, 2000 bytes, 4 faults over 2s; b: 4 sessions, 500 bytes.
	if !approx(p.Sessions["ok"], 7) || !approx(p.Bytes, 1250) || !approx(p.Faults["auth_fail"], 2) || p.ActiveSessions != 7 {
		t.Fatalf("point: %+v", p)
	}
	if p.Sessions["error"] != 0 || p.Faults["malformed"] != 0 {
		t.Fatalf("every outcome and fault type must be present: %+v", p)
	}
	if len(vs[0].BytesPS) != 1 || !approx(vs[0].BytesPS[0], 1000) {
		t.Fatalf("sparkline: %v", vs[0].BytesPS)
	}

	// a restarts: its counters drop to small values.
	poll(4, at(4, "a", 3, 600, 1, 1), at(4, "b", 58, 900, 0, 4))
	p = f.Series()[1]
	if !approx(p.Sessions["ok"], (3+4)/2.0) || !approx(p.Bytes, (600+400)/2.0) || !approx(p.Faults["auth_fail"], 0.5) {
		t.Fatalf("after reset: %+v", p)
	}
	for _, v := range []float64{p.Sessions["ok"], p.Bytes, p.Faults["auth_fail"]} {
		if v < 0 {
			t.Fatalf("negative rate after reset: %+v", p)
		}
	}

	// b goes down: its history is dropped, and it starts afresh later.
	poll(6, at(6, "a", 5, 800, 1, 1), View{Instance: Instance{Name: "b"}})
	if _, ok := f.prev["b"]; ok {
		t.Fatal("a down instance must drop its last scrape")
	}
	poll(8, at(8, "a", 7, 1000, 1, 1), at(8, "b", 1000, 1e9, 0, 4))
	p = f.Series()[len(f.Series())-1]
	if !approx(p.Bytes, 100) {
		t.Fatalf("b's first scrape back must not count: %+v", p)
	}

	// Points older than the window go.
	poll(int(SeriesWindow/time.Second)+20, at(int(SeriesWindow/time.Second)+20, "a", 9, 1200, 1, 1))
	if first := f.Series()[0]; first.T < float64(t0.Unix()+20) {
		t.Fatalf("old points kept: first at %v", first.T)
	}
}

func TestPollFeedsSeries(t *testing.T) {
	dir := t.TempDir()
	in := uitest.Start(t, dir, "127.0.0.1", uitest.Options{})
	f := newFleet(t, dir, nil)
	in.Sessions.Store(20)
	in.Bytes.Store(5000)
	in.Active.Store(3)
	time.Sleep(10 * time.Millisecond)
	f.Poll(context.Background())
	s := f.Series()
	if len(s) != 1 || s[0].Sessions["ok"] <= 0 || s[0].Bytes <= 0 || s[0].ActiveSessions != 3 {
		t.Fatalf("series after two polls: %+v", s)
	}
}

func TestDeviceAndHostnames(t *testing.T) {
	dir := t.TempDir()
	a := uitest.Start(t, dir, "127.0.0.1", uitest.Options{Devices: 50})
	uitest.WriteManifest(t, dir, map[string]string{"rtr-127.0.0.1-3": "hang:60s"}, a)
	f := newFleet(t, dir, nil)

	hosts := f.Hostnames()["127.0.0.1"]
	if len(hosts) != 50 || hosts[7] != "rtr-127.0.0.1-7" {
		t.Fatalf("hostnames: %d %v", len(hosts), hosts[:3])
	}
	d, err := f.Device("127.0.0.1", 3)
	if err != nil || d.Host != "rtr-127.0.0.1-3" || d.Port != 10003 || d.Profile != "hang:1m0s" || !d.Pinned {
		t.Fatalf("pinned device: %+v %v", d, err)
	}

	// Live rules pick some devices; Device agrees with AffectedBy.
	if r := f.ApplyProfiles(context.Background(), 42, []string{"refuse@20%"}); !r[0].OK() {
		t.Fatalf("apply: %+v", r)
	}
	f.Poll(context.Background())
	affected, _ := f.AffectedBy(42, []string{"refuse@20%"})
	picked := map[int]bool{}
	for _, x := range affected {
		if !x.Pinned {
			picked[x.Port] = true
		}
	}
	for i := 0; i < 50; i++ {
		d, err := f.Device("127.0.0.1", i)
		if err != nil {
			t.Fatal(err)
		}
		if i != 3 && (d.Profile == "refuse") != picked[d.Port] {
			t.Fatalf("device %d: profile %q, AffectedBy picked %v", i, d.Profile, picked[d.Port])
		}
	}
	if _, err := f.Device("127.0.0.1", 50); err == nil {
		t.Fatal("out of range index must fail")
	}
	if _, err := f.Device("nope", 0); err == nil {
		t.Fatal("unknown instance must fail")
	}
}
