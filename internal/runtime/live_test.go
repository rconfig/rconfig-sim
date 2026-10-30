package runtime

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

func TestHolderZeroValue(t *testing.T) {
	var h Holder
	if h.Load() != nil {
		t.Fatal("zero Holder must Load nil")
	}
	if got := h.Store(&Live{}).Generation; got != 1 {
		t.Fatalf("first Store: generation want 1, got %d", got)
	}
}

func TestHolderStoreBumpsGeneration(t *testing.T) {
	h := NewHolder(&Live{MaxSessions: 10, Generation: 99})
	first := h.Load()
	if first.Generation != 1 {
		t.Fatalf("initial generation: want 1 (input ignored), got %d", first.Generation)
	}

	in := first.Clone()
	in.MaxSessions = 20
	stored := h.Store(in)
	if stored.Generation != 2 || h.Load() != stored {
		t.Fatalf("second Store: want generation 2 and Load()==stored, got %d", stored.Generation)
	}
	if in.Generation != 1 {
		t.Errorf("Store must not modify its argument; generation now %d", in.Generation)
	}
}

// TestHolderSnapshotUnchangedBySwap is the session contract: a Live loaded
// before a swap keeps its values after it.
func TestHolderSnapshotUnchangedBySwap(t *testing.T) {
	noFaults, _ := fault.NewSet("", 0)
	h := NewHolder(&Live{Faults: noFaults, DelayMin: 50 * time.Millisecond, MaxSessions: 5, ActiveDevices: 100})
	snap := h.Load()

	allFaults, _ := fault.ParseRates("auth_fail=1")
	next := snap.Clone()
	next.Faults = allFaults
	next.DelayMin = 0
	next.MaxSessions = 1
	next.ActiveDevices = 0
	h.Store(next)

	if !snap.Faults.Empty() || snap.DelayMin != 50*time.Millisecond || snap.MaxSessions != 5 || snap.ActiveDevices != 100 || snap.Generation != 1 {
		t.Fatalf("snapshot changed after swap: %+v", snap)
	}
	cur := h.Load()
	if !cur.Faults.Enabled(fault.TypeAuthFail) || cur.MaxSessions != 1 || cur.ActiveDevices != 0 || cur.Generation != 2 {
		t.Fatalf("new generation wrong: %+v", cur)
	}
}

// TestHolderConcurrent runs Load against concurrent Stores. Run under -race.
// Generations seen by any one reader must never go backwards, and the final
// generation must count every Store.
func TestHolderConcurrent(t *testing.T) {
	h := NewHolder(&Live{})
	const writers, stores, readers = 4, 500, 4

	stop := make(chan struct{})
	var rwg sync.WaitGroup
	for r := 0; r < readers; r++ {
		rwg.Add(1)
		go func() {
			defer rwg.Done()
			var last uint64
			for {
				select {
				case <-stop:
					return
				default:
				}
				l := h.Load()
				if l.Generation < last {
					t.Errorf("generation went backwards: %d after %d", l.Generation, last)
					return
				}
				last = l.Generation
				_ = l.MaxSessions
			}
		}()
	}

	var wwg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wwg.Add(1)
		go func(w int) {
			defer wwg.Done()
			for i := 0; i < stores; i++ {
				next := h.Load().Clone()
				next.MaxSessions = int64(w*stores + i)
				h.Store(next)
			}
		}(w)
	}
	wwg.Wait()
	close(stop)
	rwg.Wait()

	if got, want := h.Load().Generation, uint64(1+writers*stores); got != want {
		t.Fatalf("final generation: want %d, got %d", want, got)
	}
}

func BenchmarkHolderLoad(b *testing.B) {
	h := NewHolder(&Live{MaxSessions: 5000})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = h.Load().MaxSessions
	}
}

func TestHolderUpdate(t *testing.T) {
	h := NewHolder(&Live{MaxSessions: 5, ActiveDevices: 10})

	got, err := h.Update(func(l *Live) error {
		l.MaxSessions = 7
		return nil
	})
	if err != nil || got.Generation != 2 || got.MaxSessions != 7 || got.ActiveDevices != 10 || h.Load() != got {
		t.Fatalf("Update: got %+v, err %v", got, err)
	}

	wantErr := errors.New("nope")
	if _, err := h.Update(func(l *Live) error {
		l.MaxSessions = 99
		return wantErr
	}); err != wantErr {
		t.Fatalf("Update error: want %v, got %v", wantErr, err)
	}
	if cur := h.Load(); cur != got {
		t.Fatalf("failed Update must not store: now %+v", cur)
	}

	var empty Holder
	if l, err := empty.Update(func(l *Live) error { l.MaxSessions = 1; return nil }); err != nil || l.Generation != 1 {
		t.Fatalf("Update on empty Holder: %+v, %v", l, err)
	}
}

// TestHolderUpdateConcurrentFieldsSurvive is the lost-update case Update
// exists for: writers changing different fields at once must all land.
// Run under -race.
func TestHolderUpdateConcurrentFieldsSurvive(t *testing.T) {
	h := NewHolder(&Live{})
	const n = 500
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			_, _ = h.Update(func(l *Live) error { l.MaxSessions = int64(i); return nil })
		}
	}()
	go func() {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			_, _ = h.Update(func(l *Live) error { l.ActiveDevices = i; return nil })
		}
	}()
	wg.Wait()
	cur := h.Load()
	if cur.MaxSessions != n || cur.ActiveDevices != n || cur.Generation != 1+2*n {
		t.Fatalf("lost update: %+v", cur)
	}
}
