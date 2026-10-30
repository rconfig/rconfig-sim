// Package admin serves the rcfg-sim admin API, /admin/v1/*, on the metrics
// listener when --admin is set. It reads and changes the instance's live
// runtime config and serves its activity data (device states and a sampled
// event stream). See docs/UI-ARCHITECTURE.md section 6 for the contract.
//
// Every change goes through runtime.Holder.Update, so it applies to new
// sessions only and concurrent changes to different settings all survive.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/runtime"
)

// Prefix is where the API is mounted.
const Prefix = "/admin/v1/"

// Limits on PUT input.
const (
	maxBodyBytes = 64 << 10
	maxDelayMS   = 3_600_000 // one hour; keeps the delay well inside time.Duration
)

// Event stream settings.
const (
	defaultSample         = 0.01
	defaultDroppedEvery   = time.Second
	subscriberBufferDepth = 256
)

// Instance is what the API needs from a running simulator. *sshsrv.Server
// implements it.
type Instance interface {
	Runtime() *runtime.Holder
	ResolveProfiles(rules []profile.Rule, seed int64) (map[int]*profile.Profile, error)
	ActiveSessions() int64
	DeviceTable() *activity.DeviceTable
	Bus() *activity.Bus
	StartedAt() time.Time
	Done() <-chan struct{}
}

// Options describe the instance and configure the API.
type Options struct {
	Version   string
	ListenIP  string
	PortStart int
	PortCount int
	Devices   int // devices loaded from the manifest
	// Token, when set, must be sent as "Authorization: Bearer <token>".
	Token  string
	Logger *slog.Logger
	// DroppedEvery is how often an event stream reports dropped events.
	// Zero means one second.
	DroppedEvery time.Duration
}

// State is the body of GET /admin/v1/state and of every successful PUT.
type State struct {
	Version        string             `json:"version"`
	ListenIP       string             `json:"listen_ip"`
	PortStart      int                `json:"port_start"`
	PortCount      int                `json:"port_count"`
	Devices        int                `json:"devices"`
	StartedAt      time.Time          `json:"started_at"`
	Generation     uint64             `json:"generation"`
	Faults         map[string]float64 `json:"faults"`
	Profiles       Profiles           `json:"profiles"`
	DelayMS        DelayMS            `json:"delay_ms"`
	MaxSessions    int64              `json:"max_sessions"`
	ActiveDevices  int                `json:"active_devices"`
	ActiveSessions int64              `json:"active_sessions"`
}

// Profiles is the device profile config: the --profile-seed and the
// --device-profiles items.
type Profiles struct {
	Seed  int64    `json:"seed"`
	Rules []string `json:"rules"`
}

// DelayMS is the per-command response delay window in milliseconds.
type DelayMS struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
}

// Devices is the body of GET /admin/v1/devices.
type Devices struct {
	PortStart int    `json:"port_start"`
	Now       int64  `json:"now"`       // unix seconds
	State     string `json:"state"`     // base64, one state byte per device
	LastSeen  string `json:"last_seen"` // base64, little-endian uint32 unix seconds per device
}

type handler struct {
	inst Instance
	opt  Options
	mux  *http.ServeMux
}

// Handler returns the API handler. Mount it at Prefix.
func Handler(inst Instance, opt Options) http.Handler {
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if opt.DroppedEvery <= 0 {
		opt.DroppedEvery = defaultDroppedEvery
	}
	h := &handler{inst: inst, opt: opt, mux: http.NewServeMux()}
	h.mux.HandleFunc(Prefix+"state", h.method(http.MethodGet, h.getState))
	h.mux.HandleFunc(Prefix+"faults", h.method(http.MethodPut, h.putFaults))
	h.mux.HandleFunc(Prefix+"profiles", h.method(http.MethodPut, h.putProfiles))
	h.mux.HandleFunc(Prefix+"behaviour", h.method(http.MethodPut, h.putBehaviour))
	h.mux.HandleFunc(Prefix+"active", h.method(http.MethodPut, h.putActive))
	h.mux.HandleFunc(Prefix+"devices", h.method(http.MethodGet, h.getDevices))
	h.mux.HandleFunc(Prefix+"events", h.method(http.MethodGet, h.getEvents))
	h.mux.HandleFunc(Prefix, func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint: "+r.URL.Path)
	})
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.opt.Token != "" && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="rcfg-sim"`)
		writeError(w, http.StatusUnauthorized, "missing or wrong bearer token")
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *handler) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(h.opt.Token)) == 1
}

// method wraps fn so any other HTTP method gets a JSON 405.
func (h *handler) method(m string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != m {
			w.Header().Set("Allow", m)
			writeError(w, http.StatusMethodNotAllowed, r.Method+" not allowed; use "+m)
			return
		}
		fn(w, r)
	}
}

func (h *handler) state(live *runtime.Live) State {
	rules := live.ProfileRules
	if rules == nil {
		rules = []string{}
	}
	return State{
		Version:        h.opt.Version,
		ListenIP:       h.opt.ListenIP,
		PortStart:      h.opt.PortStart,
		PortCount:      h.opt.PortCount,
		Devices:        h.opt.Devices,
		StartedAt:      h.inst.StartedAt().UTC().Truncate(time.Second),
		Generation:     live.Generation,
		Faults:         live.Faults.Rates(),
		Profiles:       Profiles{Seed: live.ProfileSeed, Rules: rules},
		DelayMS:        DelayMS{Min: live.DelayMin.Milliseconds(), Max: live.DelayMax.Milliseconds()},
		MaxSessions:    live.MaxSessions,
		ActiveDevices:  live.ActiveDevices,
		ActiveSessions: h.inst.ActiveSessions(),
	}
}

func (h *handler) getState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.state(h.inst.Runtime().Load()))
}

// update applies fn through Holder.Update and answers with the new state, or
// a 400 carrying fn's error.
func (h *handler) update(w http.ResponseWriter, what string, fn func(*runtime.Live) error) {
	live, err := h.inst.Runtime().Update(fn)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.opt.Logger.Info("admin change applied", "change", what, "generation", live.Generation)
	writeJSON(w, http.StatusOK, h.state(live))
}

func (h *handler) putFaults(w http.ResponseWriter, r *http.Request) {
	var body map[string]float64
	if !decode(w, r, &body) {
		return
	}
	set, err := fault.NewSetFromRates(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.update(w, "faults", func(l *runtime.Live) error {
		l.Faults = set
		return nil
	})
}

func (h *handler) putProfiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seed  *int64    `json:"seed"`
		Rules *[]string `json:"rules"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Rules == nil {
		writeError(w, http.StatusBadRequest, `"rules" is required; use [] for none`)
		return
	}
	given := make([]string, 0, len(*body.Rules))
	for _, item := range *body.Rules {
		item = strings.TrimSpace(item)
		if item == "" || strings.Contains(item, ",") {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("rule %q: want one profile@percent item", item))
			return
		}
		given = append(given, item)
	}
	rules, err := profile.Parse(strings.Join(given, ","))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Seed defaults to the current one. Read it outside Update so the slow
	// rebuild runs off the store lock; the Update below re-checks it.
	seed := h.inst.Runtime().Load().ProfileSeed
	if body.Seed != nil {
		seed = *body.Seed
	}
	profiles, err := h.inst.ResolveProfiles(rules, seed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.update(w, "profiles", func(l *runtime.Live) error {
		if body.Seed == nil && l.ProfileSeed != seed {
			return errors.New("profile seed changed concurrently; retry")
		}
		l.Profiles = profiles
		l.ProfileRules = given
		l.ProfileSeed = seed
		return nil
	})
}

func (h *handler) putBehaviour(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DelayMS *struct {
			Min *int64 `json:"min"`
			Max *int64 `json:"max"`
		} `json:"delay_ms"`
		MaxSessions *int64 `json:"max_sessions"`
	}
	if !decode(w, r, &body) {
		return
	}
	if d := body.DelayMS; d != nil {
		switch {
		case d.Min == nil || d.Max == nil:
			writeError(w, http.StatusBadRequest, "delay_ms needs both min and max")
			return
		case *d.Min < 0 || *d.Max > maxDelayMS || *d.Min > *d.Max:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("delay_ms: want 0 <= min <= max <= %d", maxDelayMS))
			return
		}
	}
	if body.MaxSessions != nil && *body.MaxSessions < 1 {
		writeError(w, http.StatusBadRequest, "max_sessions must be >= 1")
		return
	}
	h.update(w, "behaviour", func(l *runtime.Live) error {
		if d := body.DelayMS; d != nil {
			l.DelayMin = time.Duration(*d.Min) * time.Millisecond
			l.DelayMax = time.Duration(*d.Max) * time.Millisecond
		}
		if body.MaxSessions != nil {
			l.MaxSessions = *body.MaxSessions
		}
		return nil
	})
}

func (h *handler) putActive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ActiveDevices *int `json:"active_devices"`
	}
	if !decode(w, r, &body) {
		return
	}
	n := body.ActiveDevices
	if n == nil || *n < 0 || *n > h.opt.PortCount {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("active_devices is required, 0..%d (port_count)", h.opt.PortCount))
		return
	}
	h.update(w, "active", func(l *runtime.Live) error {
		l.ActiveDevices = *n
		return nil
	})
}

func (h *handler) getDevices(w http.ResponseWriter, _ *http.Request) {
	live := h.inst.Runtime().Load()
	t := h.inst.DeviceTable()
	writeJSON(w, http.StatusOK, Devices{
		PortStart: h.opt.PortStart,
		Now:       time.Now().Unix(),
		State:     t.EncodeStates(live.ActiveDevices),
		LastSeen:  t.EncodeLastSeen(),
	})
}

// getEvents streams sampled session events as server-sent events until the
// client goes away or the server shuts down. Each "data:" line is one JSON
// activity.Event, or an activity.Dropped report every DroppedEvery when
// events were lost to the rate cap or a slow client.
func (h *handler) getEvents(w http.ResponseWriter, r *http.Request) {
	sample := defaultSample
	if v := r.URL.Query().Get("sample"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || !(f > 0 && f <= 1) {
			writeError(w, http.StatusBadRequest, "sample must be a number in (0,1]")
			return
		}
		sample = f
	}
	rc := http.NewResponseController(w)

	bus := h.inst.Bus()
	sub := bus.Subscribe(sample, subscriberBufferDepth)
	defer bus.Unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// A comment line so the client sees the stream open at once.
	if _, err := io.WriteString(w, ": subscribed\n\n"); err != nil || rc.Flush() != nil {
		return
	}

	tick := time.NewTicker(h.opt.DroppedEvery)
	defer tick.Stop()
	for {
		var msg any
		select {
		case <-r.Context().Done():
			return
		case <-h.inst.Done():
			return
		case ev := <-sub.C:
			msg = ev
		case <-tick.C:
			n := sub.TakeDropped()
			if n == 0 {
				continue
			}
			msg = activity.Dropped{Kind: activity.KindDropped, N: n}
		}
		b, err := json.Marshal(msg)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// decode reads a JSON body into v, rejecting unknown fields and trailing
// data. On failure it writes a 400 and returns false.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		writeError(w, http.StatusBadRequest, "bad JSON body: trailing data")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
