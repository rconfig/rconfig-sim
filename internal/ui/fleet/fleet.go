package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

// Status is an instance card's state, in order of precedence (section 7.2).
type Status string

const (
	StatusBadEnv    Status = "bad-env"    // env file unreadable or has bad values
	StatusPortClash Status = "port-clash" // shares a metrics port with another instance
	StatusDown      Status = "down"       // /healthz does not answer
	StatusAdminOff  Status = "admin-off"  // /healthz answers, /admin/v1/state is 404
	StatusError     Status = "error"      // admin API answered something unexpected
	StatusUp        Status = "up"
)

// historyLen is how many bytes/s points a card's sparkline keeps.
const historyLen = 30

// View is one instance as last polled.
type View struct {
	Instance
	Status   Status
	Detail   string       // why the status is not Up, for the card
	Unit     string       // systemctl is-active, when down
	State    *admin.State // nil unless Up
	Drift    []string     // settings where the env file and live config differ
	Desired  *Desired     // from the env file; nil if it does not parse
	BytesPS  []float64    // bytes/s per poll, oldest first
	LastPoll time.Time

	counters *counterSample // this poll's /metrics scrape; nil if none
}

// UnitStater reports a unit's systemctl is-active state.
type UnitStater interface {
	IsActive(ctx context.Context, instance string) (string, error)
}

// Options configure a Fleet.
type Options struct {
	EnvDir string
	Units  UnitStater // may be nil
	Client *http.Client
	Logger *slog.Logger
	// ApplyTimeout bounds each instance's part of a fan-out. Zero means 5s.
	ApplyTimeout time.Duration
}

// Fleet tracks every instance on the host.
type Fleet struct {
	opt Options

	mu     sync.RWMutex
	views  []View
	prev   map[string]counterSample // last scrape per instance
	hist   map[string][]float64     // bytes/s per instance, for the sparkline
	series []RatePoint              // fleet-wide chart points, last SeriesWindow

	use    useCache     // devices per instance in the live manifest
	envMu  sync.Map     // env path -> *sync.Mutex, serialising writes per file
	stream *http.Client // no overall timeout, for event streams

	obsMu     sync.Mutex
	observers []func(Sample)
}

// Sample is one poll's counter growth per instance, for run totals. An
// instance appears once it has been scraped in two polls in a row; an
// instance restart shows as a counter reset (see CounterDelta).
type Sample struct {
	At        time.Time
	Instances map[string]InstanceSample
}

// InstanceSample is one instance's counter growth since the previous poll
// and its open sessions now.
type InstanceSample struct {
	Delta          Counters
	ActiveSessions float64
}

// Observe calls fn after every poll with that poll's Sample. fn must not
// block for long.
func (f *Fleet) Observe(fn func(Sample)) {
	f.obsMu.Lock()
	defer f.obsMu.Unlock()
	f.observers = append(f.observers, fn)
}

// New returns a Fleet. Call Poll (or Run) to fill it.
func New(opt Options) *Fleet {
	if opt.Client == nil {
		opt.Client = &http.Client{Timeout: 5 * time.Second}
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opt.ApplyTimeout <= 0 {
		opt.ApplyTimeout = 5 * time.Second
	}
	return &Fleet{
		opt:    opt,
		prev:   map[string]counterSample{},
		hist:   map[string][]float64{},
		stream: &http.Client{Transport: opt.Client.Transport},
	}
}

// Run polls every interval until ctx is done.
func (f *Fleet) Run(ctx context.Context, every time.Duration) {
	f.Poll(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.Poll(ctx)
		}
	}
}

// Poll rediscovers the instances and polls them all in parallel.
func (f *Fleet) Poll(ctx context.Context) {
	insts, err := Discover(f.opt.EnvDir)
	if err != nil {
		f.opt.Logger.Error("discover instances", "dir", f.opt.EnvDir, "err", err)
	}
	unused := f.unused(insts)
	views := make([]View, len(insts))
	var wg sync.WaitGroup
	for i, in := range insts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i] = f.poll(ctx, in, unused[in.Name])
		}()
	}
	wg.Wait()
	f.mu.Lock()
	sample := f.record(views, time.Now())
	f.views = views
	f.mu.Unlock()
	f.obsMu.Lock()
	obs := slices.Clone(f.observers)
	f.obsMu.Unlock()
	for _, fn := range obs {
		fn(sample)
	}
}

// Views returns a copy of the last poll.
func (f *Fleet) Views() []View {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return slices.Clone(f.views)
}

// View returns the named instance from the last poll.
func (f *Fleet) View(name string) (View, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	for _, v := range f.views {
		if v.Name == name {
			return v, true
		}
	}
	return View{}, false
}

// unused names the instances with no devices in their manifest. Every
// instance shares one manifest on the data/ layout; one that cannot be
// read marks nothing unused.
func (f *Fleet) unused(insts []Instance) map[string]bool {
	out := map[string]bool{}
	var ok []Instance
	for _, in := range insts {
		if in.Err == nil && in.Manifest == insts[0].Manifest {
			ok = append(ok, in)
		}
	}
	if len(ok) == 0 {
		return out
	}
	counts, err := f.use.get(ok[0].Manifest, ok)
	if err != nil {
		return out
	}
	for n, c := range counts {
		out[n] = c == 0
	}
	return out
}

func (f *Fleet) poll(ctx context.Context, in Instance, unused bool) View {
	v := View{Instance: in, LastPoll: time.Now()}
	if d, err := DesiredFromEnv(in); err == nil {
		v.Desired = &d
	}
	switch {
	case in.Err != nil:
		v.Status, v.Detail = StatusBadEnv, in.Err.Error()
		return v
	case len(in.PortClash) > 0:
		v.Status = StatusPortClash
		v.Detail = fmt.Sprintf("a listener port (METRICS_ADDR %s, ADMIN_ADDR %s) is also used by %s", in.MetricsAddr, orNone(in.AdminAddr), strings.Join(in.PortClash, ", "))
		return v
	case unused:
		v.Status, v.Detail = StatusUnused, UnusedDetail
		return v
	}

	if err := f.healthz(ctx, in.DialAddr); err != nil {
		v.Status, v.Detail = StatusDown, err.Error()
		if f.opt.Units != nil {
			if s, err := f.opt.Units.IsActive(ctx, in.Name); err == nil {
				v.Unit = s
			} else {
				v.Unit = "unknown"
			}
		}
		return v
	}
	if c, err := f.scrapeCounters(ctx, in.DialAddr); err == nil {
		v.counters = &counterSample{at: time.Now(), c: c}
	}

	// The admin API: its own loopback listener (D3). Off when ADMIN_ADDR is
	// empty or does not answer (section 7.2).
	if in.AdminAddr == "" {
		v.Status, v.Detail = StatusAdminOff, "admin API is off: add ADMIN_ADDR=127.0.0.1:<port> to "+in.EnvPath+" and restart"
		return v
	}
	st, code, err := f.getState(ctx, in.AdminAddr)
	switch {
	case err != nil, code == http.StatusNotFound:
		v.Status, v.Detail = StatusAdminOff, "admin API at ADMIN_ADDR "+in.AdminAddr+" does not answer: check "+in.EnvPath+" and restart"
	case code != http.StatusOK:
		v.Status, v.Detail = StatusError, fmt.Sprintf("admin API answered %d", code)
	default:
		v.Status, v.State = StatusUp, st
		if v.Desired == nil {
			v.Drift = []string{"env file"}
		} else {
			v.Drift = Drift(*v.Desired, *st)
		}
	}
	return v
}

func (f *Fleet) healthz(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	resp, err := f.opt.Client.Do(req)
	if err != nil {
		return fmt.Errorf("healthz: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %d", resp.StatusCode)
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Healthy reports whether the named instance answers /healthz now. Used by
// rolling restarts.
func (f *Fleet) Healthy(ctx context.Context, name string) error {
	v, ok := f.View(name)
	if !ok {
		return fmt.Errorf("no instance %q", name)
	}
	return f.healthz(ctx, v.DialAddr)
}

func (f *Fleet) adminRequest(ctx context.Context, method, addr, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+admin.Prefix+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return f.opt.Client.Do(req)
}

func (f *Fleet) getState(ctx context.Context, addr string) (*admin.State, int, error) {
	resp, err := f.adminRequest(ctx, http.MethodGet, addr, "state", nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var st admin.State
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("decode state: %w", err)
	}
	return &st, resp.StatusCode, nil
}

func (f *Fleet) scrapeCounters(ctx context.Context, addr string) (Counters, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/metrics", nil)
	resp, err := f.opt.Client.Do(req)
	if err != nil {
		return Counters{}, err
	}
	defer resp.Body.Close()
	return ParseCounters(resp.Body)
}

// record differences this poll's counters against the last poll's, per
// instance: each instance's bytes/s history (the card sparkline) and one
// fleet-wide chart point, and the per-instance Sample for observers. An
// instance with no scrape this poll (down, or /metrics failed) drops its
// history, so the next scrape starts afresh. Called with f.mu held.
func (f *Fleet) record(views []View, now time.Time) Sample {
	point := newPoint(now)
	sample := Sample{At: now, Instances: map[string]InstanceSample{}}
	rated := false
	seen := map[string]bool{}
	for i := range views {
		v := &views[i]
		cur := v.counters
		if cur == nil {
			continue
		}
		seen[v.Name] = true
		if prev, ok := f.prev[v.Name]; ok {
			if dt := cur.at.Sub(prev.at).Seconds(); dt > 0 {
				point.addRates(prev.c, cur.c, dt)
				rated = true
				sample.Instances[v.Name] = InstanceSample{Delta: counterDeltas(prev.c, cur.c), ActiveSessions: cur.c.ActiveSessions}
				h := append(f.hist[v.Name], CounterDelta(prev.c.Bytes, cur.c.Bytes)/dt)
				if len(h) > historyLen {
					h = h[len(h)-historyLen:]
				}
				f.hist[v.Name] = h
			}
		}
		f.prev[v.Name] = *cur
		v.BytesPS = slices.Clone(f.hist[v.Name])
	}
	for name := range f.prev {
		if !seen[name] {
			delete(f.prev, name)
			delete(f.hist, name)
		}
	}
	if rated {
		f.series = append(f.series, point)
	}
	cut := float64(now.Add(-SeriesWindow).UnixMilli()) / 1000
	i := 0
	for i < len(f.series) && f.series[i].T < cut {
		i++
	}
	f.series = f.series[i:]
	return sample
}

// Series returns the fleet-wide chart points for the last SeriesWindow,
// oldest first.
func (f *Fleet) Series() []RatePoint {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return slices.Clone(f.series)
}

// Drift lists the settings where the env file and the live config differ.
func Drift(d Desired, st admin.State) []string {
	var out []string
	for _, t := range fault.AllTypes {
		if d.Faults[t.String()] != st.Faults[t.String()] {
			out = append(out, "faults")
			break
		}
	}
	if !slices.Equal(d.ProfileRules, st.Profiles.Rules) {
		out = append(out, "profiles")
	}
	if d.ProfileSeed != st.Profiles.Seed {
		out = append(out, "profile seed")
	}
	if d.DelayMinMS != st.DelayMS.Min || d.DelayMaxMS != st.DelayMS.Max {
		out = append(out, "response delay")
	}
	if d.MaxSessions != st.MaxSessions {
		out = append(out, "session cap")
	}
	if d.ActiveDevices != st.ActiveDevices {
		out = append(out, "active devices")
	}
	return out
}

// SplitActive divides target across instances in proportion to weights (each
// instance's device count) by largest remainder, so the shares add up to
// target exactly and none exceeds its weight.
func SplitActive(target int, weights []int) ([]int, error) {
	total := 0
	for _, w := range weights {
		if w < 0 {
			return nil, errors.New("negative device count")
		}
		total += w
	}
	if target < 0 || target > total {
		return nil, fmt.Errorf("active devices %d out of range 0..%d", target, total)
	}
	out := make([]int, len(weights))
	if total == 0 {
		return out, nil
	}
	type rem struct {
		i int
		r int64
	}
	rems := make([]rem, len(weights))
	given := 0
	for i, w := range weights {
		p := int64(target) * int64(w)
		out[i] = int(p / int64(total))
		rems[i] = rem{i, p % int64(total)}
		given += out[i]
	}
	slices.SortStableFunc(rems, func(a, b rem) int {
		switch {
		case a.r > b.r:
			return -1
		case a.r < b.r:
			return 1
		}
		return 0
	})
	for k := 0; given < target; k++ {
		out[rems[k].i]++
		given++
	}
	return out, nil
}
