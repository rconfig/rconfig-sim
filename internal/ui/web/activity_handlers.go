package web

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// Stream timings.
const (
	streamTick      = 250 * time.Millisecond // how often a stream checks for a new frame or chart point
	droppedEvery    = time.Second            // how often a stream reports drops
	recentPerDevice = 20                     // events in the device panel
)

func (s *Server) activityPage(w http.ResponseWriter, r *http.Request) {
	p := s.page("Activity", "activity")
	p.Scripts = []string{"/static/vendor/htmx-ext-sse-2.2.4.min.js", "/static/vendor/uplot-1.6.32.iife.min.js", "/static/activity.js"}
	p.Styles = []string{"/static/vendor/uplot-1.6.32.min.css"}
	render(w, r, http.StatusOK, views.ActivityPage(p))
}

// sseWriter writes server-sent events and flushes each one.
type sseWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (sw sseWriter) send(event, data string) error {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	for _, line := range strings.Split(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	if _, err := io.WriteString(sw.w, b.String()); err != nil {
		return err
	}
	return sw.rc.Flush()
}

func (sw sseWriter) json(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return sw.send(event, string(b))
}

// activityStream is the Activity screen's one event stream. Messages:
//
//	history  every chart point of the window, once, on connect
//	rates    each new chart point
//	frame    each new fleet frame (Frame JSON)
//	summary  the summary strip, as HTML (swapped in by htmx-ext-sse)
//	event    one session event (activity.Event JSON)
//	dropped  {"n": events this browser missed, "upstream": events the instances dropped}
//
// The stream holds the hub open: when the last browser goes, the hub closes
// its upstream connections.
func (s *Server) activityStream(w http.ResponseWriter, r *http.Request) {
	sub, leave := s.hub.Join()
	defer leave()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := sseWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil || sw.rc.Flush() != nil {
		return
	}

	series := s.opt.Fleet.Series()
	if sw.json("history", series) != nil {
		return
	}
	var lastPoint float64
	if len(series) > 0 {
		lastPoint = series[len(series)-1].T
	}
	var lastFrame int64
	lastUpstream := s.hub.UpstreamDropped()

	tick := time.NewTicker(streamTick)
	defer tick.Stop()
	nextDropped := time.Now().Add(droppedEvery)
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev := <-sub.C:
			err = sw.json("event", ev)
		case now := <-tick.C:
			if f := s.hub.Frame(); f != nil && f.At != lastFrame {
				lastFrame = f.At
				if err = sw.json("frame", f); err == nil {
					err = sw.send("summary", s.summaryHTML(r, f))
				}
			}
			if pts := s.opt.Fleet.Series(); err == nil && len(pts) > 0 && pts[len(pts)-1].T != lastPoint {
				p := pts[len(pts)-1]
				lastPoint = p.T
				err = sw.json("rates", p)
			}
			if err == nil && now.After(nextDropped) {
				nextDropped = now.Add(droppedEvery)
				up := s.hub.UpstreamDropped()
				n, upDelta := sub.TakeDropped(), up-lastUpstream
				lastUpstream = up
				if n > 0 || upDelta > 0 {
					err = sw.json("dropped", map[string]uint64{"n": n, "upstream": upDelta})
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) summaryModel(f *Frame) views.SummaryModel {
	m := views.SummaryModel{Instances: len(f.Instances), Devices: f.Total, Upstream: s.hub.UpstreamDropped()}
	for _, in := range f.Instances {
		if in.Reachable {
			m.Reachable++
		}
	}
	for _, name := range LegendOrder {
		if n := f.Counts[name]; n > 0 || name != "unreachable" {
			m.Legend = append(m.Legend, views.LegendItem{Name: name, Class: views.StateClass(name), N: n})
		}
	}
	return m
}

func (s *Server) summaryHTML(r *http.Request, f *Frame) string {
	var b bytes.Buffer
	_ = views.ActivitySummary(s.summaryModel(f)).Render(r.Context(), &b)
	return b.String()
}

// activityHosts serves every instance's hostnames by port offset, for the
// heatmap's hover. Gzipped when the browser accepts it: 50k names are about
// a megabyte raw.
func (s *Server) activityHosts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	var out io.Writer = w
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		out = gz
	}
	_ = json.NewEncoder(out).Encode(s.opt.Fleet.Hostnames())
}

// activityDevice renders the side panel for one heatmap cell.
func (s *Server) activityDevice(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("instance")
	index, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil {
		render(w, r, http.StatusOK, views.DevicePanel(views.DeviceModel{Err: "bad device index"}))
		return
	}
	d, err := s.opt.Fleet.Device(name, index)
	m := views.DeviceModel{Instance: d.Instance, Host: d.Host, IP: d.IP, Port: d.Port, Profile: d.Profile, Pinned: d.Pinned,
		State: "unknown", StateClass: views.StateClass(""), LastSeen: "unknown"}
	if err != nil {
		m.Err = err.Error()
	}
	if f := s.hub.Frame(); f != nil {
		m.State, m.LastSeen = cellState(f, name, index)
		m.StateClass = views.StateClass(m.State)
	}
	if d.Host != "" {
		for _, ev := range s.hub.Recent(d.Host, recentPerDevice) {
			m.Events = append(m.Events, eventLine(ev))
		}
	}
	render(w, r, http.StatusOK, views.DevicePanel(m))
}

// cellState reads one device's state name and last-seen age from a frame.
func cellState(f *Frame, name string, index int) (state, lastSeen string) {
	for _, in := range f.Instances {
		if in.Name != name || index < 0 || index >= in.Count {
			continue
		}
		i := in.Offset + index
		states, err1 := base64.StdEncoding.DecodeString(f.State)
		seen, err2 := base64.StdEncoding.DecodeString(f.LastSeen)
		if err1 != nil || err2 != nil || i >= len(states) || 4*i+4 > len(seen) {
			break
		}
		state = stateNames[states[i]]
		if state == "" {
			state = "never"
		}
		ts := int64(binary.LittleEndian.Uint32(seen[4*i:]))
		switch {
		case !in.Reachable:
			lastSeen = "unknown (instance not reporting)"
		case ts == 0:
			lastSeen = "never"
		default:
			lastSeen = fmt.Sprintf("%s (%ds ago)", time.Unix(ts, 0).UTC().Format("15:04:05 UTC"), max(0, f.Now-ts))
		}
		return state, lastSeen
	}
	return "unknown", "unknown"
}

func eventLine(ev activity.Event) string {
	t := time.UnixMilli(ev.T).UTC().Format("15:04:05.000")
	parts := []string{t, ev.Kind}
	if ev.Outcome != "" {
		parts = append(parts, ev.Outcome)
	}
	if ev.Fault != "" {
		parts = append(parts, "fault "+ev.Fault)
	}
	if ev.Kind == activity.KindSessionEnd {
		parts = append(parts, fmt.Sprintf("%d B in %d ms", ev.Bytes, ev.MS))
	}
	return strings.Join(parts, " · ")
}
