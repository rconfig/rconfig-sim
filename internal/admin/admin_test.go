package admin

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

// fakeInstance is an Instance with three devices on ports 10000..10002,
// hostnames rtr-0..rtr-2.
type fakeInstance struct {
	holder  *runtime.Holder
	table   *activity.DeviceTable
	bus     *activity.Bus
	started time.Time
	done    chan struct{}
}

func newFake(t *testing.T) *fakeInstance {
	t.Helper()
	faults, _ := fault.ParseRates("auth_fail=0.02")
	return &fakeInstance{
		holder: runtime.NewHolder(&runtime.Live{
			Faults:        faults,
			DelayMin:      50 * time.Millisecond,
			DelayMax:      500 * time.Millisecond,
			MaxSessions:   5000,
			ActiveDevices: 3,
			ProfileSeed:   42,
		}),
		table:   activity.NewDeviceTable(3),
		bus:     activity.NewBus(activity.DefaultRateLimit, 0),
		started: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
		done:    make(chan struct{}),
	}
}

func (f *fakeInstance) Runtime() *runtime.Holder { return f.holder }
func (f *fakeInstance) ResolveProfiles(rules []profile.Rule, seed int64) (map[int]*profile.Profile, error) {
	out := map[int]*profile.Profile{}
	for i := 0; i < 3; i++ {
		if p := profile.Assign(fmt.Sprintf("rtr-%d", i), seed, rules); p != nil {
			out[10000+i] = p
		}
	}
	return out, nil
}
func (f *fakeInstance) ActiveSessions() int64              { return 7 }
func (f *fakeInstance) DeviceTable() *activity.DeviceTable { return f.table }
func (f *fakeInstance) Bus() *activity.Bus                 { return f.bus }
func (f *fakeInstance) StartedAt() time.Time               { return f.started }
func (f *fakeInstance) Done() <-chan struct{}              { return f.done }

func serve(t *testing.T, f *fakeInstance, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(Prefix, Handler(f, Options{
		Version: "0.0.6", ListenIP: "10.50.0.1", PortStart: 10000, PortCount: 3, Devices: 3,
		Token: token, DroppedEvery: 20 * time.Millisecond,
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&buf)
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type %q", method, path, ct)
	}
	return resp.StatusCode, []byte(buf.String())
}

func decodeState(t *testing.T, b []byte) State {
	t.Helper()
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("decode state %s: %v", b, err)
	}
	return st
}

func wantError(t *testing.T, code int, b []byte, wantCode int) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("status: want %d, got %d (%s)", wantCode, code, b)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &e); err != nil || e.Error == "" {
		t.Fatalf("want {\"error\": ...}, got %s", b)
	}
}

func TestGetState(t *testing.T) {
	srv := serve(t, newFake(t), "")
	code, b := do(t, srv, "GET", "/admin/v1/state", "", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	// Pin the wire shape: every key in section 6, faults with all four types.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"version", "listen_ip", "port_start", "port_count", "devices", "started_at",
		"generation", "faults", "profiles", "delay_ms", "max_sessions", "active_devices", "active_sessions"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("state missing key %q", k)
		}
	}
	st := decodeState(t, b)
	want := State{
		Version: "0.0.6", ListenIP: "10.50.0.1", PortStart: 10000, PortCount: 3, Devices: 3,
		StartedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), Generation: 1,
		Profiles: Profiles{Seed: 42, Rules: []string{}}, DelayMS: DelayMS{Min: 50, Max: 500},
		MaxSessions: 5000, ActiveDevices: 3, ActiveSessions: 7,
	}
	gotFaults := st.Faults
	st.Faults = nil
	if fmt.Sprint(st) != fmt.Sprint(want) {
		t.Errorf("state:\n got %+v\nwant %+v", st, want)
	}
	if len(gotFaults) != 4 || gotFaults["auth_fail"] != 0.02 || gotFaults["malformed"] != 0 {
		t.Errorf("faults: %v", gotFaults)
	}
	if !strings.Contains(string(raw["started_at"]), "2026-10-30T09:00:00Z") || string(raw["profiles"]) != `{"seed":42,"rules":[]}` {
		t.Errorf("started_at %s, profiles %s", raw["started_at"], raw["profiles"])
	}
}

func TestPutFaults(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")

	code, b := do(t, srv, "PUT", "/admin/v1/faults", `{"disconnect_mid": 0.005, "slow_response": 1}`, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	st := decodeState(t, b)
	if st.Generation != 2 || st.Faults["auth_fail"] != 0 || st.Faults["disconnect_mid"] != 0.005 || st.Faults["slow_response"] != 1 {
		t.Fatalf("missing types must mean 0: %+v", st)
	}
	if live := f.holder.Load(); live.Faults.Rate(fault.TypeSlowResponse) != 1 || live.Faults.Enabled(fault.TypeAuthFail) {
		t.Fatalf("live faults not applied: %v", live.Faults)
	}

	for _, body := range []string{`{"auth_fail": 1.5}`, `{"auth_fail": -0.1}`, `{"bogus": 0.1}`, `{"auth_fail": "x"}`, `[1]`, `{`, `{} {}`} {
		code, b := do(t, srv, "PUT", "/admin/v1/faults", body, "")
		wantError(t, code, b, 400)
	}
	if g := f.holder.Load().Generation; g != 2 {
		t.Fatalf("rejected PUTs must not bump generation: %d", g)
	}
}

func TestPutProfiles(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")

	code, b := do(t, srv, "PUT", "/admin/v1/profiles", `{"seed": 7, "rules": ["refuse@100%"]}`, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	st := decodeState(t, b)
	if st.Profiles.Seed != 7 || len(st.Profiles.Rules) != 1 || st.Profiles.Rules[0] != "refuse@100%" {
		t.Fatalf("profiles: %+v", st.Profiles)
	}
	live := f.holder.Load()
	if len(live.Profiles) != 3 || !live.Profiles[10001].Is(profile.KindRefuse) {
		t.Fatalf("profile map not rebuilt: %v", live.Profiles)
	}

	// Seed omitted keeps the current seed; [] clears.
	code, b = do(t, srv, "PUT", "/admin/v1/profiles", `{"rules": []}`, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	if st := decodeState(t, b); st.Profiles.Seed != 7 || len(st.Profiles.Rules) != 0 || len(f.holder.Load().Profiles) != 0 {
		t.Fatalf("clear: %+v", st.Profiles)
	}

	for _, body := range []string{
		`{"seed": 1}`,                                 // rules required
		`{"rules": ["hang:3600s"]}`,                   // missing @percent
		`{"rules": ["hang:3600s@3%,refuse@1%"]}`,      // two items in one
		`{"rules": ["refuse@60%", "disconnect@50%"]}`, // over 100%
		`{"rules": ["bogus@1%"]}`,
		`{"rules": [""]}`,
	} {
		code, b := do(t, srv, "PUT", "/admin/v1/profiles", body, "")
		wantError(t, code, b, 400)
	}
}

func TestPutBehaviour(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")

	code, b := do(t, srv, "PUT", "/admin/v1/behaviour", `{"delay_ms": {"min": 0, "max": 10}}`, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	if st := decodeState(t, b); st.DelayMS != (DelayMS{0, 10}) || st.MaxSessions != 5000 {
		t.Fatalf("delay only: %+v", st)
	}
	code, b = do(t, srv, "PUT", "/admin/v1/behaviour", `{"max_sessions": 12}`, "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	if st := decodeState(t, b); st.DelayMS != (DelayMS{0, 10}) || st.MaxSessions != 12 {
		t.Fatalf("cap only: %+v", st)
	}
	if live := f.holder.Load(); live.DelayMax != 10*time.Millisecond || live.MaxSessions != 12 {
		t.Fatalf("live: %+v", live)
	}

	for _, body := range []string{
		`{"delay_ms": {"min": 10, "max": 5}}`,
		`{"delay_ms": {"min": -1, "max": 5}}`,
		`{"delay_ms": {"min": 0}}`,
		`{"delay_ms": {"min": 0, "max": 3600001}}`,
		`{"max_sessions": 0}`,
		`{"max_sessions": 5, "extra": 1}`,
	} {
		code, b := do(t, srv, "PUT", "/admin/v1/behaviour", body, "")
		wantError(t, code, b, 400)
	}
}

func TestPutActive(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")
	for _, n := range []int{0, 2, 3} {
		code, b := do(t, srv, "PUT", "/admin/v1/active", fmt.Sprintf(`{"active_devices": %d}`, n), "")
		if code != 200 || decodeState(t, b).ActiveDevices != n || f.holder.Load().ActiveDevices != n {
			t.Fatalf("active %d: status %d %s", n, code, b)
		}
	}
	for _, body := range []string{`{"active_devices": -1}`, `{"active_devices": 4}`, `{}`} {
		code, b := do(t, srv, "PUT", "/admin/v1/active", body, "")
		wantError(t, code, b, 400)
	}
}

func TestGetDevices(t *testing.T) {
	f := newFake(t)
	now := time.Unix(1761814800, 0)
	f.table.End(0, activity.StateOK, now)
	f.table.Start(1)
	_, _ = f.holder.Update(func(l *runtime.Live) error { l.ActiveDevices = 2; return nil })
	srv := serve(t, f, "")

	code, b := do(t, srv, "GET", "/admin/v1/devices", "", "")
	if code != 200 {
		t.Fatalf("status %d: %s", code, b)
	}
	var d Devices
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	states, _ := base64.StdEncoding.DecodeString(d.State)
	seen, _ := base64.StdEncoding.DecodeString(d.LastSeen)
	if d.PortStart != 10000 || d.Now < now.Unix() ||
		string(states) != string([]byte{byte(activity.StateOK), byte(activity.StateActive), byte(activity.StateInactive)}) ||
		len(seen) != 12 {
		t.Fatalf("devices: %+v states %v seen %d bytes", d, states, len(seen))
	}
}

func TestMethodsAndUnknownPaths(t *testing.T) {
	srv := serve(t, newFake(t), "")
	code, b := do(t, srv, "POST", "/admin/v1/state", "", "")
	wantError(t, code, b, 405)
	code, b = do(t, srv, "GET", "/admin/v1/faults", "", "")
	wantError(t, code, b, 405)
	code, b = do(t, srv, "GET", "/admin/v1/nope", "", "")
	wantError(t, code, b, 404)
}

func TestToken(t *testing.T) {
	srv := serve(t, newFake(t), "s3cret")
	for _, tok := range []string{"", "wrong", "s3cret2"} {
		code, b := do(t, srv, "GET", "/admin/v1/state", "", tok)
		wantError(t, code, b, 401)
	}
	if code, b := do(t, srv, "GET", "/admin/v1/state", "", "s3cret"); code != 200 {
		t.Fatalf("right token: %d %s", code, b)
	}
	// The events stream is covered too.
	resp, err := srv.Client().Get(srv.URL + "/admin/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("events without token: %d", resp.StatusCode)
	}
}

// TestConcurrentPutsDifferentEndpoints is the lost-update case: PUTs to
// different endpoints at once must all survive. Run under -race.
func TestConcurrentPutsDifferentEndpoints(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")
	const n = 50
	var wg sync.WaitGroup
	put := func(path string, body func(i int) string) {
		defer wg.Done()
		for i := 1; i <= n; i++ {
			if code, b := do(t, srv, "PUT", path, body(i), ""); code != 200 {
				t.Errorf("%s: %d %s", path, code, b)
				return
			}
		}
	}
	wg.Add(4)
	go put("/admin/v1/faults", func(i int) string { return fmt.Sprintf(`{"malformed": %g}`, float64(i)/100) })
	go put("/admin/v1/behaviour", func(i int) string { return fmt.Sprintf(`{"max_sessions": %d}`, i) })
	go put("/admin/v1/active", func(i int) string { return fmt.Sprintf(`{"active_devices": %d}`, i%2+2) })
	go put("/admin/v1/profiles", func(i int) string { return `{"seed": 9, "rules": ["disconnect@50%"]}` })
	wg.Wait()

	live := f.holder.Load()
	if live.Faults.Rate(fault.TypeMalformed) != float64(n)/100 || live.MaxSessions != n ||
		live.ActiveDevices != n%2+2 || live.ProfileSeed != 9 || len(live.ProfileRules) != 1 {
		t.Fatalf("a concurrent change was lost: %+v", live)
	}
	if live.Generation != 1+4*n {
		t.Fatalf("generation: want %d, got %d", 1+4*n, live.Generation)
	}
}

// sseReader reads "data:" payloads from an event stream.
func sseReader(t *testing.T, ctx context.Context, url string) (<-chan string, func()) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	out := make(chan string, 4096)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				out <- d
			}
		}
	}()
	return out, func() { resp.Body.Close() }
}

func waitSubscribed(t *testing.T, b *activity.Bus) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !b.Active(); {
		if time.Now().After(deadline) {
			t.Fatal("subscriber never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestEventsUnderLoad publishes a burst far past the rate cap: the client
// gets events, at most about the cap, and dropped reports covering the rest.
func TestEventsUnderLoad(t *testing.T) {
	f := newFake(t)
	f.bus = activity.NewBus(50, 0)
	srv := serve(t, f, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data, closeFn := sseReader(t, ctx, srv.URL+"/admin/v1/events?sample=1")
	defer closeFn()
	waitSubscribed(t, f.bus)

	const total = 5000
	for i := 0; i < total; i++ {
		f.bus.Publish(activity.Event{T: int64(i), Host: "rtr-0", Port: 10000, Kind: activity.KindSessionEnd, Outcome: "ok"})
	}

	var events int
	var dropped uint64
	timeout := time.After(3 * time.Second)
	for uint64(events)+dropped < total {
		select {
		case d := <-data:
			var m map[string]any
			if err := json.Unmarshal([]byte(d), &m); err != nil {
				t.Fatalf("bad event %q: %v", d, err)
			}
			switch m["kind"] {
			case activity.KindDropped:
				dropped += uint64(m["n"].(float64))
			case activity.KindSessionEnd:
				if m["host"] != "rtr-0" || m["outcome"] != "ok" {
					t.Fatalf("event fields: %v", m)
				}
				events++
			default:
				t.Fatalf("unexpected event %v", m)
			}
		case <-timeout:
			t.Fatalf("got %d events + %d dropped of %d", events, dropped, total)
		}
	}
	if events == 0 || events > 60 || dropped == 0 {
		t.Fatalf("rate cap 50/s: %d events, %d dropped", events, dropped)
	}
}

func TestEventsSampleValidation(t *testing.T) {
	srv := serve(t, newFake(t), "")
	for _, q := range []string{"0", "1.5", "-1", "x"} {
		code, b := do(t, srv, "GET", "/admin/v1/events?sample="+q, "", "")
		wantError(t, code, b, 400)
	}
}

// TestEventsEndOnShutdown checks the stream ends when the instance shuts
// down, so it cannot hold up Shutdown, and unsubscribes.
func TestEventsEndOnShutdown(t *testing.T) {
	f := newFake(t)
	srv := serve(t, f, "")
	data, closeFn := sseReader(t, context.Background(), srv.URL+"/admin/v1/events")
	defer closeFn()
	waitSubscribed(t, f.bus)
	close(f.done)
	select {
	case _, ok := <-data:
		if ok {
			t.Fatal("unexpected data")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end on shutdown")
	}
	for deadline := time.Now().Add(time.Second); f.bus.Active(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("stream did not unsubscribe")
		}
	}
}
