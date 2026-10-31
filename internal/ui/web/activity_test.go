package web

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func instancesConnected(insts []*uitest.Instance) int {
	n := 0
	for _, in := range insts {
		if in.Events.Active() {
			n++
		}
	}
	return n
}

func recv(t *testing.T, sub *activity.Subscriber) activity.Event {
	t.Helper()
	select {
	case ev := <-sub.C:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no event")
		return activity.Event{}
	}
}

// TestHubIdleHoldsNothing: with no browser connected, no upstream event
// connection and no poller.
func TestHubIdleHoldsNothing(t *testing.T) {
	e := newEnv(t, 2, "")
	e.fleet.Poll(context.Background())
	time.Sleep(300 * time.Millisecond)
	if e.srv.Hub().UpstreamConns() != 0 || instancesConnected(e.insts) != 0 || e.srv.Hub().Frame() != nil {
		t.Fatalf("idle hub holds connections: hub %d, instances %d", e.srv.Hub().UpstreamConns(), instancesConnected(e.insts))
	}
}

// TestHubFanOutAndTeardown: one upstream per instance however many
// browsers; every browser gets every event; the last leave closes every
// upstream connection.
func TestHubFanOutAndTeardown(t *testing.T) {
	e := newEnv(t, 2, "")
	hub := e.srv.Hub()

	sub1, leave1 := hub.Join()
	sub2, leave2 := hub.Join()
	eventually(t, "two upstreams", func() bool { return hub.UpstreamConns() == 2 && instancesConnected(e.insts) == 2 })

	e.insts[0].Events.Publish(activity.Event{T: 1, Host: "rtr-a", Port: 10001, Kind: activity.KindSessionEnd, Outcome: "ok"})
	e.insts[1].Events.Publish(activity.Event{T: 2, Host: "rtr-b", Port: 10002, Kind: activity.KindFault, Fault: "auth_fail"})
	for _, sub := range []*activity.Subscriber{sub1, sub2} {
		got := map[string]bool{}
		got[recv(t, sub).Host] = true
		got[recv(t, sub).Host] = true
		if !got["rtr-a"] || !got["rtr-b"] {
			t.Fatalf("a browser missed an event: %v", got)
		}
	}
	if hub.UpstreamConns() != 2 {
		t.Fatalf("two browsers must share one upstream per instance, got %d", hub.UpstreamConns())
	}
	if r := hub.Recent("rtr-a", 10); len(r) != 1 || r[0].Outcome != "ok" {
		t.Fatalf("recent events: %+v", r)
	}

	leave1()
	if hub.Viewers() != 1 || hub.UpstreamConns() != 2 {
		t.Fatalf("one browser left: viewers %d, upstreams %d", hub.Viewers(), hub.UpstreamConns())
	}
	leave1() // idempotent
	if hub.Viewers() != 1 {
		t.Fatal("a second leave must not count")
	}
	leave2()
	if hub.Viewers() != 0 || hub.UpstreamConns() != 0 {
		t.Fatalf("after the last leave: viewers %d, upstreams %d", hub.Viewers(), hub.UpstreamConns())
	}
	eventually(t, "instances to see the streams close", func() bool { return instancesConnected(e.insts) == 0 })

	// And it starts again for the next browser.
	_, leave3 := hub.Join()
	eventually(t, "upstreams again", func() bool { return hub.UpstreamConns() == 2 })
	leave3()
	if hub.UpstreamConns() != 0 {
		t.Fatal("upstreams left open")
	}
}

// TestHubFollowsInstanceChanges: an instance that goes down loses its
// upstream; one that comes back gets a new one.
func TestHubFollowsInstanceChanges(t *testing.T) {
	e := newEnv(t, 2, "")
	hub := e.srv.Hub()
	_, leave := hub.Join()
	defer leave()
	eventually(t, "two upstreams", func() bool { return hub.UpstreamConns() == 2 })

	e.insts[1].Down.Store(true)
	e.fleet.Poll(context.Background())
	eventually(t, "the down instance's upstream to close", func() bool { return hub.UpstreamConns() == 1 })

	e.insts[1].Down.Store(false)
	e.fleet.Poll(context.Background())
	eventually(t, "a new upstream", func() bool { return hub.UpstreamConns() == 2 })
}

func TestHubCountsUpstreamDrops(t *testing.T) {
	dir := t.TempDir()
	in := uitest.Start(t, dir, "127.0.0.1", uitest.Options{EventRate: 10, DroppedEvery: 50 * time.Millisecond})
	f := fleet.New(fleet.Options{EnvDir: dir})
	f.Poll(context.Background())
	hub := NewHub(f, nil, 50*time.Millisecond)
	_, leave := hub.Join()
	defer leave()
	eventually(t, "upstream", func() bool { return in.Events.Active() })
	for i := 0; i < 200; i++ {
		in.Events.Publish(activity.Event{T: int64(i), Host: "h", Kind: activity.KindSessionEnd})
	}
	eventually(t, "upstream drops reported", func() bool { return hub.UpstreamDropped() > 0 })
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func TestAssembleFrame(t *testing.T) {
	views := []fleet.View{
		{Instance: fleet.Instance{Name: "10.0.0.1", ListenIP: "10.0.0.1", PortStart: 10000, PortCount: 3}, Status: fleet.StatusUp},
		{Instance: fleet.Instance{Name: "10.0.0.2", ListenIP: "10.0.0.2", PortStart: 10000, PortCount: 2}, Status: fleet.StatusDown},
		{Instance: fleet.Instance{Name: "bad", Err: errors.New("bad env")}, Status: fleet.StatusBadEnv},
		{Instance: fleet.Instance{Name: "10.0.0.4", ListenIP: "10.0.0.4", PortStart: 20000, PortCount: 2}, Status: fleet.StatusUp},
	}
	seen := make([]byte, 12)
	seen[4] = 7 // device 1 of instance 1 last seen at unix 7
	devs := map[string]*deviceResult{
		"10.0.0.1": {states: []byte{1, 7, 6}, lastSeen: seen},
		"10.0.0.4": {states: []byte{1}, lastSeen: make([]byte, 4)}, // wrong length: unreachable
	}
	f := AssembleFrame(views, devs, time.Unix(100, 0))
	if f.Total != 7 || len(f.Instances) != 3 {
		t.Fatalf("frame: total %d, instances %+v", f.Total, f.Instances)
	}
	if f.Instances[1].Offset != 3 || f.Instances[2].Offset != 5 || !f.Instances[0].Reachable || f.Instances[1].Reachable || f.Instances[2].Reachable {
		t.Fatalf("instances: %+v", f.Instances)
	}
	states, _ := base64.StdEncoding.DecodeString(f.State)
	if string(states) != string([]byte{1, 7, 6, 255, 255, 255, 255}) {
		t.Fatalf("states: %v", states)
	}
	ls, _ := base64.StdEncoding.DecodeString(f.LastSeen)
	if len(ls) != 28 || ls[4] != 7 {
		t.Fatalf("last seen: %v", ls)
	}
	if f.Counts["ok"] != 1 || f.Counts["active"] != 1 || f.Counts["inactive"] != 1 || f.Counts["unreachable"] != 4 || f.Now != 100 {
		t.Fatalf("counts: %+v", f.Counts)
	}
	if st, last := cellState(f, "10.0.0.1", 1); st != "active" || !strings.Contains(last, "93s ago") {
		t.Fatalf("cellState: %q %q", st, last)
	}
	if st, last := cellState(f, "10.0.0.2", 0); st != "unreachable" || !strings.Contains(last, "not reporting") {
		t.Fatalf("cellState unreachable: %q %q", st, last)
	}
}

// TestAssembleFrameFleetScale: 50,000 cells across 20 instances.
func TestAssembleFrameFleetScale(t *testing.T) {
	var views []fleet.View
	devs := map[string]*deviceResult{}
	for i := 0; i < 20; i++ {
		name := "10.50.0." + string(rune('a'+i))
		views = append(views, fleet.View{Instance: fleet.Instance{Name: name, PortStart: 10000, PortCount: 2500}, Status: fleet.StatusUp})
		devs[name] = &deviceResult{states: make([]byte, 2500), lastSeen: make([]byte, 10000)}
	}
	start := time.Now()
	f := AssembleFrame(views, devs, time.Now())
	if f.Total != 50000 || f.Counts["never"] != 50000 {
		t.Fatalf("frame: %d cells, counts %v", f.Total, f.Counts)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("assembling 50k cells took %v", d)
	}
}

func TestHubFrameFromInstances(t *testing.T) {
	e := newEnv(t, 2, "")
	e.insts[0].Table.Start(2)                                   // device 2 active
	e.insts[1].Table.End(0, activity.StateAuthFail, time.Now()) // device 0 auth_fail
	_, leave := e.srv.Hub().Join()
	defer leave()
	eventually(t, "a frame", func() bool { return e.srv.Hub().Frame() != nil })
	f := e.srv.Hub().Frame()
	states, _ := base64.StdEncoding.DecodeString(f.State)
	if f.Total != 40 || states[2] != byte(activity.StateActive) || states[20] != byte(activity.StateAuthFail) {
		t.Fatalf("frame: total %d states %v", f.Total, states)
	}

	// A device state change shows within a frame interval or two.
	e.insts[0].Table.End(2, activity.StateOK, time.Now())
	eventually(t, "the change in a frame", func() bool {
		s, _ := base64.StdEncoding.DecodeString(e.srv.Hub().Frame().State)
		return s[2] == byte(activity.StateOK)
	})
}

// sseEvent is one parsed server-sent event.
type sseEvent struct{ name, data string }

func readSSE(r io.Reader, out chan<- sseEvent) {
	defer close(out)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var ev sseEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.name != "" || len(data) > 0 {
				ev.data = strings.Join(data, "\n")
				out <- ev
			}
			ev, data = sseEvent{}, nil
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
}

func waitFor(t *testing.T, ch <-chan sseEvent, name string) sseEvent {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before %q", name)
			}
			if ev.name == name {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %q message", name)
		}
	}
}

// TestActivityStream: the browser stream carries history, frames, the
// summary HTML, events and rates; closing it tears the hub down.
func TestActivityStream(t *testing.T) {
	e := newEnv(t, 2, "")
	e.insts[0].Sessions.Store(10)
	e.fleet.Poll(context.Background()) // a chart point exists before connecting

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.http.URL+"/activity/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	ch := make(chan sseEvent, 256)
	go readSSE(resp.Body, ch)

	var hist []fleet.RatePoint
	if err := json.Unmarshal([]byte(waitFor(t, ch, "history").data), &hist); err != nil || len(hist) != 1 {
		t.Fatalf("history: %v %+v", err, hist)
	}
	var fr Frame
	if err := json.Unmarshal([]byte(waitFor(t, ch, "frame").data), &fr); err != nil || fr.Total != 40 || len(fr.Instances) != 2 {
		t.Fatalf("frame: %v %+v", err, fr)
	}
	summary := waitFor(t, ch, "summary").data
	if !strings.Contains(summary, "instances reporting") || !strings.Contains(summary, "bg-st-active") {
		t.Fatalf("summary: %s", summary)
	}

	eventually(t, "upstreams", func() bool { return e.srv.Hub().UpstreamConns() == 2 })
	e.insts[1].Events.Publish(activity.Event{T: 5, Host: "rtr-127.0.0.2-3", Port: 10003, Kind: activity.KindSessionEnd, Outcome: "disconnect"})
	var ev activity.Event
	if err := json.Unmarshal([]byte(waitFor(t, ch, "event").data), &ev); err != nil || ev.Outcome != "disconnect" {
		t.Fatalf("event: %v %+v", err, ev)
	}

	e.insts[0].Sessions.Store(30)
	e.fleet.Poll(context.Background())
	var p fleet.RatePoint
	if err := json.Unmarshal([]byte(waitFor(t, ch, "rates").data), &p); err != nil || p.Sessions["ok"] <= 0 {
		t.Fatalf("rates: %v %+v", err, p)
	}

	// The tab closes: the stream ends and the hub lets go of the instances.
	cancel()
	resp.Body.Close()
	eventually(t, "the hub to empty", func() bool { return e.srv.Hub().Viewers() == 0 && e.srv.Hub().UpstreamConns() == 0 })
	eventually(t, "the instances to see it", func() bool { return instancesConnected(e.insts) == 0 })
}

func TestActivityStreamEndsOnServerClose(t *testing.T) {
	e := newEnv(t, 1, "")
	resp, err := http.Get(e.http.URL + "/activity/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ch := make(chan sseEvent, 64)
	go readSSE(resp.Body, ch)
	waitFor(t, ch, "history")
	e.srv.Close()
	timeout := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				eventually(t, "upstreams closed", func() bool { return e.srv.Hub().UpstreamConns() == 0 })
				return
			}
		case <-timeout:
			t.Fatal("stream still open after Close")
		}
	}
}

func TestActivityHostsAndDevice(t *testing.T) {
	e := newEnv(t, 2, "")
	req, _ := http.NewRequest("GET", e.http.URL+"/activity/hosts", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("hosts not gzipped: %v", resp.Header)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var hosts map[string][]string
	if err := json.NewDecoder(gz).Decode(&hosts); err != nil || len(hosts["127.0.0.2"]) != 20 || hosts["127.0.0.2"][5] != "rtr-127.0.0.2-5" {
		t.Fatalf("hosts: %v %v", err, hosts)
	}

	_, leave := e.srv.Hub().Join()
	defer leave()
	eventually(t, "upstreams", func() bool { return e.srv.Hub().UpstreamConns() == 2 })
	e.insts[1].Events.Publish(activity.Event{T: time.Now().UnixMilli(), Host: "rtr-127.0.0.2-5", Port: 10005, Kind: activity.KindSessionEnd, Outcome: "auth_fail", Fault: "auth_fail"})
	eventually(t, "the event in the ring", func() bool { return len(e.srv.Hub().Recent("rtr-127.0.0.2-5", 5)) == 1 })
	eventually(t, "a frame", func() bool { return e.srv.Hub().Frame() != nil })

	body := e.get("/activity/device?instance=127.0.0.2&index=5")
	for _, want := range []string{"rtr-127.0.0.2-5", "127.0.0.2:10005", "instance 127.0.0.2", "never", "session_end · auth_fail · fault auth_fail"} {
		if !strings.Contains(body, want) {
			t.Errorf("device panel missing %q:\n%s", want, body)
		}
	}
	if body := e.get("/activity/device?instance=127.0.0.2&index=99"); !strings.Contains(body, "out of range") {
		t.Errorf("bad index: %s", body)
	}
}

func TestActivityPageRender(t *testing.T) {
	e := newEnv(t, 1, "")
	body := e.get("/activity")
	for _, want := range []string{
		`hx-ext="sse"`, `sse-connect="/activity/stream"`, `sse-swap="summary"`,
		`<canvas id="heatmap"`, `id="device-panel"`, `id="chart-active"`, `id="chart-faults"`,
		`id="tail-pause"`, `id="tail-host"`, `id="tail-outcome"`, `id="tail-fault"`,
		`href="/static/vendor/uplot-1.6.32.min.css"`, `aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("activity page missing %q", want)
		}
	}
	// Scripts load after htmx, in dependency order.
	htmxAt := strings.Index(body, "htmx-2.0.11.min.js")
	sseAt := strings.Index(body, "htmx-ext-sse-2.2.4.min.js")
	uplotAt := strings.Index(body, "uplot-1.6.32.iife.min.js")
	actAt := strings.Index(body, "/static/activity.js")
	if !(htmxAt >= 0 && htmxAt < sseAt && sseAt < uplotAt && uplotAt < actAt) {
		t.Fatalf("script order: htmx %d, sse %d, uplot %d, activity %d", htmxAt, sseAt, uplotAt, actAt)
	}
	for _, asset := range []string{"/static/vendor/htmx-ext-sse-2.2.4.min.js", "/static/vendor/uplot-1.6.32.iife.min.js",
		"/static/vendor/uplot-1.6.32.min.css", "/static/activity.js"} {
		if resp, _ := e.do(req{method: "GET", path: asset}); resp.StatusCode != 200 {
			t.Errorf("%s: %d", asset, resp.StatusCode)
		}
	}
	// Other pages do not load the Activity scripts.
	if strings.Contains(e.get("/fleet"), "activity.js") {
		t.Error("fleet page loads activity.js")
	}
}

func TestSummaryRender(t *testing.T) {
	out := renderString(t, views.ActivitySummary(views.SummaryModel{Instances: 3, Reachable: 2, Devices: 50,
		Legend: []views.LegendItem{{Name: "ok", Class: "bg-st-ok", N: 40}}, Upstream: 9}))
	for _, want := range []string{"2</span> / <span class=\"num\">3", "bg-st-ok", ">ok<", ">40<", "dropped <span class=\"num\">9"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}
