package web

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// Hub settings.
const (
	// upstreamSample is the sample rate the hub asks each instance for. The
	// instance caps every stream at 200 events/s and reports what it drops,
	// so asking for everything is safe at any fleet size.
	upstreamSample = 1.0
	// viewerRate caps the events each browser is sent per second, across
	// the whole fleet. More are counted as dropped and reported.
	viewerRate = 200
	// recentEvents is how many events the hub keeps for the device panel.
	recentEvents = 5000
	// upstreamRetry is the wait before reconnecting a broken upstream.
	upstreamRetry = 2 * time.Second
)

// Hub is the controller's live-data hub (Phase 4). While at least one
// browser is on the Activity screen it holds one upstream event stream per
// up instance (/admin/v1/events), merged and fanned out to the browsers,
// and one shared poller for /admin/v1/devices that assembles a fleet frame
// every frame interval. With no browser connected it holds nothing: no
// upstream connections and no poller.
type Hub struct {
	fleet      *fleet.Fleet
	logger     *slog.Logger
	frameEvery time.Duration

	bus     *activity.Bus                                  // fan-out to browsers; ring of recent events
	frame   atomic.Pointer[Frame]                          // latest fleet frame
	dropped atomic.Uint64                                  // events the instances reported dropping
	conns   atomic.Int32                                   // open upstream event connections
	mu      sync.Mutex                                     // guards viewers, cancel, done
	viewers int                                            // browsers on the Activity screen
	cancel  context.CancelFunc                             // stops the running hub; nil when idle
	done    chan struct{}                                  // closed when the running hub has stopped
	devices func(context.Context) map[string]*deviceResult // replaced in tests
}

// NewHub returns an idle hub.
func NewHub(f *fleet.Fleet, logger *slog.Logger, frameEvery time.Duration) *Hub {
	h := &Hub{
		fleet:      f,
		logger:     logger,
		frameEvery: frameEvery,
		bus:        activity.NewBus(viewerRate, recentEvents),
	}
	h.devices = h.fetchDevices
	return h
}

// Join registers a browser. The first one starts the upstream streams and
// the frame poller. The returned leave must be called exactly once; the
// last leave stops them and waits until every upstream connection is
// closed.
func (h *Hub) Join() (sub *activity.Subscriber, leave func()) {
	sub = h.bus.Subscribe(1, 1024)
	h.mu.Lock()
	h.viewers++
	if h.viewers == 1 {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel, h.done = cancel, make(chan struct{})
		go h.run(ctx, h.done)
	}
	h.mu.Unlock()

	var once sync.Once
	return sub, func() {
		once.Do(func() {
			h.bus.Unsubscribe(sub)
			h.mu.Lock()
			h.viewers--
			var done chan struct{}
			if h.viewers == 0 {
				h.cancel()
				h.cancel, done = nil, h.done
			}
			h.mu.Unlock()
			if done != nil {
				<-done
			}
		})
	}
}

// Viewers returns how many browsers are connected.
func (h *Hub) Viewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.viewers
}

// UpstreamConns returns how many instance event streams are open.
func (h *Hub) UpstreamConns() int { return int(h.conns.Load()) }

// Frame returns the latest fleet frame, or nil before the first.
func (h *Hub) Frame() *Frame { return h.frame.Load() }

// UpstreamDropped returns the running total of events the instances
// reported dropping.
func (h *Hub) UpstreamDropped() uint64 { return h.dropped.Load() }

// Recent returns the most recent events for one host, newest last.
func (h *Hub) Recent(host string, n int) []activity.Event {
	var out []activity.Event
	for _, ev := range h.bus.Recent(recentEvents) {
		if ev.Host == host {
			out = append(out, ev)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// upstreams is one run's set of instance event streams.
type upstreams struct {
	cancel map[string]context.CancelFunc
	wg     sync.WaitGroup
}

// run keeps upstreams matched to the up instances and assembles a frame
// every frameEvery, until ctx ends. Everything it starts is local to it, so
// a new run can start while an old one is still closing down.
func (h *Hub) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ups := &upstreams{cancel: map[string]context.CancelFunc{}}
	defer func() {
		for _, cancel := range ups.cancel {
			cancel()
		}
		ups.wg.Wait()
	}()
	t := time.NewTicker(h.frameEvery)
	defer t.Stop()
	for {
		views := h.fleet.Views()
		h.reconcile(ctx, views, ups)
		h.frame.Store(AssembleFrame(views, h.devices(ctx), time.Now()))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcile opens an upstream for each up instance that has none and
// closes those whose instance is no longer up.
func (h *Hub) reconcile(ctx context.Context, views []fleet.View, ups *upstreams) {
	want := map[string]bool{}
	for _, v := range views {
		if v.Status == fleet.StatusUp {
			want[v.Name] = true
		}
	}
	for name, cancel := range ups.cancel {
		if !want[name] {
			cancel()
			delete(ups.cancel, name)
		}
	}
	for name := range want {
		if _, ok := ups.cancel[name]; ok {
			continue
		}
		uctx, cancel := context.WithCancel(ctx)
		ups.cancel[name] = cancel
		ups.wg.Add(1)
		go func() {
			defer ups.wg.Done()
			h.follow(uctx, name)
		}()
	}
}

// follow reads one instance's event stream into the bus, reconnecting after
// a break, until ctx ends.
func (h *Hub) follow(ctx context.Context, name string) {
	for {
		if err := h.readUpstream(ctx, name); err != nil && ctx.Err() == nil {
			h.logger.Debug("upstream events", "instance", name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(upstreamRetry):
		}
	}
}

func (h *Hub) readUpstream(ctx context.Context, name string) error {
	resp, err := h.fleet.OpenEvents(ctx, name, upstreamSample)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h.conns.Add(1)
	defer h.conns.Add(-1)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 16<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			activity.Event
			N uint64 `json:"n"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Kind == activity.KindDropped {
			h.dropped.Add(ev.N)
			continue
		}
		h.bus.Publish(ev.Event)
	}
	return sc.Err()
}

// deviceResult is one instance's /admin/v1/devices answer, or why not.
type deviceResult struct {
	states   []byte
	lastSeen []byte
	err      error
}

// fetchDevices polls /admin/v1/devices on every up instance in parallel.
func (h *Hub) fetchDevices(ctx context.Context) map[string]*deviceResult {
	out := map[string]*deviceResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, v := range h.fleet.Views() {
		if v.Status != fleet.StatusUp {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &deviceResult{}
			d, err := h.fleet.FetchDevices(ctx, v.Name)
			if err == nil {
				r.states, r.lastSeen, err = decodeDevices(d)
			}
			r.err = err
			mu.Lock()
			out[v.Name] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}
