package activity

import (
	"encoding/base64"
	"encoding/binary"
	"sync"
	"testing"
	"time"
)

func ev(i int) Event { return Event{T: int64(i), Host: "rtr", Port: 10000 + i, Kind: KindSessionEnd} }

func TestBusNoSubscribersRecordsNothing(t *testing.T) {
	b := NewBus(0, 4)
	if b.Active() {
		t.Fatal("new Bus must not be Active")
	}
	b.Publish(ev(1))
	if got := b.Recent(10); len(got) != 0 {
		t.Fatalf("Publish with no subscribers must not touch the ring: %v", got)
	}
	var nilBus *Bus
	nilBus.Publish(ev(1)) // must not panic
	if nilBus.Active() {
		t.Fatal("nil Bus must not be Active")
	}
}

func TestBusRingWraps(t *testing.T) {
	b := NewBus(1e9, 4)
	s := b.Subscribe(1, 100)
	defer b.Unsubscribe(s)

	for i := 1; i <= 3; i++ {
		b.Publish(ev(i))
	}
	if got := b.Recent(10); len(got) != 3 || got[0].T != 1 || got[2].T != 3 {
		t.Fatalf("before wrap: %v", got)
	}
	for i := 4; i <= 6; i++ {
		b.Publish(ev(i))
	}
	got := b.Recent(10)
	if len(got) != 4 || got[0].T != 3 || got[3].T != 6 {
		t.Fatalf("after wrap: want T 3..6, got %v", got)
	}
	if got := b.Recent(2); len(got) != 2 || got[0].T != 5 || got[1].T != 6 {
		t.Fatalf("Recent(2): %v", got)
	}
}

func TestBusSubscribeUnsubscribe(t *testing.T) {
	b := NewBus(1e9, 0)
	s1 := b.Subscribe(1, 10)
	s2 := b.Subscribe(1, 10)
	b.Publish(ev(1))
	if len(s1.C) != 1 || len(s2.C) != 1 {
		t.Fatal("both subscribers must get the event")
	}
	b.Unsubscribe(s1)
	b.Unsubscribe(s1) // idempotent
	b.Publish(ev(2))
	if len(s1.C) != 1 || len(s2.C) != 2 {
		t.Fatalf("after unsubscribe: s1 %d, s2 %d", len(s1.C), len(s2.C))
	}
	b.Unsubscribe(s2)
	if b.Active() {
		t.Fatal("Bus Active with no subscribers")
	}
}

func TestBusSampling(t *testing.T) {
	b := NewBus(1e9, 0)
	const n = 20000
	s := b.Subscribe(0.1, n)
	defer b.Unsubscribe(s)
	for i := 0; i < n; i++ {
		b.Publish(ev(i))
	}
	got := float64(len(s.C)) / n
	if got < 0.08 || got > 0.12 {
		t.Fatalf("sample 0.1: kept %.4f", got)
	}
	if d := s.TakeDropped(); d != 0 {
		t.Fatalf("sampled-out events are not drops: dropped %d", d)
	}
}

func TestBusRateCapAndFullBufferCountAsDropped(t *testing.T) {
	b := NewBus(10, 0)
	s := b.Subscribe(1, 1000)
	for i := 0; i < 100; i++ {
		b.Publish(ev(i))
	}
	// One-second burst of 10, plus whatever refilled during the loop.
	if got := len(s.C); got < 10 || got > 12 {
		t.Fatalf("rate 10/s: delivered %d of 100 in a burst", got)
	}
	if got, d := len(s.C), s.TakeDropped(); uint64(got)+d != 100 {
		t.Fatalf("delivered %d + dropped %d != 100", got, d)
	}
	if d := s.TakeDropped(); d != 0 {
		t.Fatalf("TakeDropped must reset: %d", d)
	}
	b.Unsubscribe(s)

	full := b.Subscribe(1, 2)
	defer b.Unsubscribe(full)
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 5; i++ {
		b.Publish(ev(i))
	}
	if len(full.C) != 2 || full.TakeDropped() != 3 {
		t.Fatalf("buffer 2: want 2 delivered and 3 dropped")
	}
}

func TestBusRateRefills(t *testing.T) {
	b := NewBus(100, 0)
	s := b.Subscribe(1, 1000)
	defer b.Unsubscribe(s)
	for i := 0; i < 200; i++ {
		b.Publish(ev(i))
	}
	first := len(s.C)
	time.Sleep(100 * time.Millisecond) // ~10 tokens back
	for i := 0; i < 200; i++ {
		b.Publish(ev(i))
	}
	if extra := len(s.C) - first; extra < 5 || extra > 30 {
		t.Fatalf("after 100ms at 100/s: want ~10 more, got %d", extra)
	}
}

// TestBusConcurrent publishes and (un)subscribes concurrently. Run under -race.
func TestBusConcurrent(t *testing.T) {
	b := NewBus(1e9, 64)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				b.Publish(ev(i))
			}
		}()
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s := b.Subscribe(0.5, 8)
				_ = b.Recent(16)
				_ = s.TakeDropped()
				b.Unsubscribe(s)
			}
		}()
	}
	wg.Wait()
}

func TestStateForOutcome(t *testing.T) {
	for in, want := range map[string]State{
		"ok": StateOK, "auth_fail": StateAuthFail, "disconnect": StateDisconnect,
		"error": StateError, "weird": StateError,
	} {
		if got := StateForOutcome(in); got != want {
			t.Errorf("%s: want %d, got %d", in, want, got)
		}
	}
}

func TestDeviceTableLifecycle(t *testing.T) {
	tb := NewDeviceTable(3)
	now := time.Unix(1761814800, 0)

	if tb.State(0) != StateNever || tb.LastSeen(0) != 0 {
		t.Fatal("new device must be never / 0")
	}

	// Two overlapping sessions: active until the last one ends.
	tb.Start(0)
	tb.Start(0)
	if tb.State(0) != StateActive {
		t.Fatal("want active during a session")
	}
	tb.End(0, StateOK, now)
	if tb.State(0) != StateActive {
		t.Fatal("one session still in progress: want active")
	}
	tb.End(0, StateAuthFail, now.Add(time.Second))
	if tb.State(0) != StateAuthFail || tb.LastSeen(0) != uint32(now.Unix()+1) {
		t.Fatalf("after both end: state %d last seen %d", tb.State(0), tb.LastSeen(0))
	}

	// A refused connection during a session keeps the session counted.
	tb.Start(1)
	tb.Set(1, StateError, now)
	if tb.State(1) != StateActive {
		t.Fatal("Set must not end a session in progress")
	}
	tb.End(1, StateHang, now)
	if tb.State(1) != StateHang {
		t.Fatalf("want hang, got %d", tb.State(1))
	}

	// Out of range is ignored.
	tb.Start(-1)
	tb.End(3, StateOK, now)
	tb.Set(99, StateOK, now)
	tb.Touch(3, now)
	if tb.State(3) != StateNever || tb.LastSeen(-1) != 0 {
		t.Fatal("out-of-range reads must be never / 0")
	}
}

func TestDeviceTableEncode(t *testing.T) {
	tb := NewDeviceTable(4)
	now := time.Unix(1761814800, 0)
	tb.End(0, StateOK, now)
	tb.Start(2)                // active, but past activeDevices below
	tb.Set(3, StateError, now) // past activeDevices: inactive
	states, err := base64.StdEncoding.DecodeString(tb.EncodeStates(2))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{byte(StateOK), byte(StateNever), byte(StateActive), byte(StateInactive)}
	if string(states) != string(want) {
		t.Fatalf("states: want %v, got %v", want, states)
	}

	seen, err := base64.StdEncoding.DecodeString(tb.EncodeLastSeen())
	if err != nil || len(seen) != 16 {
		t.Fatalf("last_seen: %d bytes, err %v", len(seen), err)
	}
	for i, want := range []uint32{1761814800, 0, 0, 1761814800} {
		if got := binary.LittleEndian.Uint32(seen[4*i:]); got != want {
			t.Errorf("last_seen[%d]: want %d, got %d", i, want, got)
		}
	}
}

// TestDeviceTableConcurrent runs Start/End/Set against one device from many
// goroutines. Run under -race. Every session ends, so the device must not be
// left active.
func TestDeviceTableConcurrent(t *testing.T) {
	tb := NewDeviceTable(1)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				tb.Start(0)
				tb.Set(0, StateError, time.Now())
				tb.End(0, StateOK, time.Now())
			}
		}()
	}
	wg.Wait()
	if s := tb.State(0); s == StateActive {
		t.Fatal("all sessions ended but device reports active")
	}
}

func BenchmarkPublishNoSubscribers(b *testing.B) {
	bus := NewBus(0, 0)
	e := ev(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bus.Publish(e)
	}
}

func BenchmarkDeviceTableStore(b *testing.B) {
	tb := NewDeviceTable(2500)
	now := time.Now()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		idx := i % 2500
		tb.Start(idx)
		tb.End(idx, StateOK, now)
	}
}

func TestHotPathAllocs(t *testing.T) {
	for name, bench := range map[string]func(*testing.B){
		"PublishNoSubscribers": BenchmarkPublishNoSubscribers,
		"DeviceTableStore":     BenchmarkDeviceTableStore,
	} {
		if a := testing.Benchmark(bench).AllocsPerOp(); a != 0 {
			t.Errorf("%s: %d allocs/op, want 0", name, a)
		}
	}
}
