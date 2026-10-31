package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/systemd"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// maxDelayMS matches the admin API's limit on PUT /admin/v1/behaviour.
const maxDelayMS = 3_600_000

func (s *Server) fleetModel() views.FleetModel {
	m := views.FleetModel{
		Views:   s.opt.Fleet.Views(),
		Poll:    strconv.FormatInt(s.opt.Poll.Milliseconds(), 10) + "ms",
		Rolling: s.rollingView(),
	}
	for _, v := range m.Views {
		if v.Status != fleet.StatusUp {
			continue
		}
		if m.Ref == nil {
			m.Ref = v.State
		}
		m.CapTotal += v.State.MaxSessions
	}
	// Active devices count down instances too (section 5.4).
	m.Devices, m.Active = s.opt.Fleet.ActiveDevicesTotal()
	return m
}

func (s *Server) fleetPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.FleetPage(s.page("Fleet", "fleet"), s.fleetModel()))
}

func (s *Server) fleetCards(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.FleetCards(s.fleetModel(), true))
}

// resultsFlash summarises a fan-out: instances that took the change now,
// those saved for their next start, partial applies, skips and failures.
func resultsFlash(what string, rs []fleet.Result) views.Flash {
	live, later := 0, 0
	var lines []string
	for _, r := range rs {
		switch {
		case r.OK() && r.NextStart:
			later++
			lines = append(lines, r.Instance+": down; saved to its env file, applies on next start")
		case r.OK():
			live++
		case r.Skipped:
			lines = append(lines, fmt.Sprintf("%s: %v; nothing written", r.Instance, r.Err))
		case r.Err != nil && len(r.Applied) > 0:
			lines = append(lines, fmt.Sprintf("%s: partly applied (%s saved); not applied: %v",
				r.Instance, strings.Join(r.Applied, ", "), r.Err))
		case r.Err != nil:
			lines = append(lines, fmt.Sprintf("%s: not applied: %v", r.Instance, r.Err))
		default:
			lines = append(lines, fmt.Sprintf("%s: applied, but the env file was not saved (%v); it will show as drift", r.Instance, r.EnvErr))
		}
	}
	text := fmt.Sprintf("%s applied to %d of %d instances.", what, live, len(rs))
	if later > 0 {
		text = fmt.Sprintf("%s applied to %d of %d instances; %d more on next start.", what, live, len(rs), later)
	}
	f := views.Flash{Kind: "ok", Text: text, Lines: lines}
	switch {
	case len(rs) == 0:
		f.Kind, f.Text = "error", what+": no instances to apply to."
	case live+later == 0:
		f.Kind = "error"
	case live+later < len(rs):
		f.Kind = "warn"
	}
	return f
}

func (s *Server) knownInstance(name string) bool {
	_, ok := s.opt.Fleet.View(name)
	return ok
}

func (s *Server) unitAction(w http.ResponseWriter, r *http.Request) {
	name, verb := r.PathValue("name"), r.PathValue("verb")
	if !s.knownInstance(name) {
		s.fail(w, r, "No instance "+name+".")
		return
	}
	if err := s.opt.Units.Do(r.Context(), verb, name); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	s.opt.Logger.Info("unit action", "verb", verb, "instance", name)
	s.flash(w, r, true, views.Flash{Kind: "ok", Text: fmt.Sprintf("%s: %s done.", name, verb)})
}

func (s *Server) unitActionAll(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")
	if verb != systemd.Start && verb != systemd.Stop && verb != systemd.Restart {
		s.fail(w, r, "Unsupported action "+verb+".")
		return
	}
	vs := s.opt.Fleet.Views()
	errs := make([]error, len(vs))
	var wg sync.WaitGroup
	for i, v := range vs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.opt.Units.Do(r.Context(), verb, v.Name)
		}()
	}
	wg.Wait()
	rs := make([]fleet.Result, len(vs))
	for i, v := range vs {
		rs[i] = fleet.Result{Instance: v.Name, Err: errs[i]}
	}
	s.opt.Logger.Info("unit action, all instances", "verb", verb)
	s.flash(w, r, true, resultsFlash(strings.ToUpper(verb[:1])+verb[1:], rs))
}

func (s *Server) rollingView() views.RollingView {
	s.rollMu.Lock()
	defer s.rollMu.Unlock()
	rv := s.rolling
	rv.Steps = slices.Clone(rv.Steps)
	return rv
}

func (s *Server) rollingRestart(w http.ResponseWriter, r *http.Request) {
	pause, err := strconv.Atoi(strings.TrimSpace(r.FormValue("pause")))
	if err != nil || pause < 0 || pause > 600 {
		s.fail(w, r, "Pause must be 0 to 600 seconds.")
		return
	}
	var names []string
	for _, v := range s.opt.Fleet.Views() {
		if v.Err == nil {
			names = append(names, v.Name)
		}
	}
	if len(names) == 0 {
		s.fail(w, r, "No instances to restart.")
		return
	}
	s.rollMu.Lock()
	if s.rolling.Running {
		s.rollMu.Unlock()
		s.fail(w, r, "A rolling restart is already running.")
		return
	}
	s.rolling = views.RollingView{Running: true}
	s.rollMu.Unlock()

	go s.runRolling(names, time.Duration(pause)*time.Second)
	s.flash(w, r, true, views.Flash{Kind: "ok", Text: fmt.Sprintf("Rolling restart of %d instances started.", len(names))})
}

func (s *Server) runRolling(names []string, pause time.Duration) {
	progress := func(st systemd.RollingStep) {
		line := st.Instance + ": " + st.Phase
		s.rollMu.Lock()
		s.rolling.Steps = append(s.rolling.Steps, line)
		s.rollMu.Unlock()
	}
	err := s.opt.Units.RollingRestart(s.ctx, names, s.opt.RollingWait, pause,
		func(ctx context.Context, inst string) error { return s.opt.Fleet.Healthy(ctx, inst) }, progress)
	s.rollMu.Lock()
	s.rolling.Running, s.rolling.Done = false, true
	if err != nil {
		s.rolling.Err = err.Error()
	}
	s.rollMu.Unlock()
	s.opt.Logger.Info("rolling restart finished", "instances", len(names), "err", err)
	s.opt.Fleet.Poll(s.ctx)
}

func (s *Server) reapply(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res := s.opt.Fleet.Reapply(r.Context(), name)
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash("Env file settings for "+name, []fleet.Result{res}))
}

// --- Active devices ---

func parseActive(r *http.Request, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue("active")))
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("active devices must be 0 to %d", max)
	}
	return n, nil
}

func (s *Server) applyActive(w http.ResponseWriter, r *http.Request) {
	m := s.fleetModel()
	n, err := parseActive(r, m.Devices)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	rs, err := s.opt.Fleet.ApplyActive(r.Context(), n)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash(fmt.Sprintf("Active devices %d", n), rs))
}

func (s *Server) pendingActive(w http.ResponseWriter, r *http.Request) {
	m := s.fleetModel()
	d := views.Diff{ApplyURL: "/fleet/active", Form: "form-active"}
	n, err := parseActive(r, m.Devices)
	if err == nil && n != m.Active {
		d.Items = append(d.Items, views.DiffItem{What: "Active devices", From: strconv.Itoa(m.Active), To: strconv.Itoa(n)})
		if split, err := s.opt.Fleet.ActiveSplit(n); err == nil {
			var parts []string
			for _, v := range m.Views {
				if share, ok := split[v.Name]; ok {
					parts = append(parts, fmt.Sprintf("%s %d", v.Name, share))
				}
			}
			d.Note = "Split: " + strings.Join(parts, ", ")
		}
	}
	render(w, r, http.StatusOK, views.PendingBar(d))
}

// --- Response delay ---

func parseDelay(r *http.Request) (lo, hi int64, err error) {
	lo, err1 := strconv.ParseInt(strings.TrimSpace(r.FormValue("delay_min")), 10, 64)
	hi, err2 := strconv.ParseInt(strings.TrimSpace(r.FormValue("delay_max")), 10, 64)
	if err1 != nil || err2 != nil || lo < 0 || lo > hi || hi > maxDelayMS {
		return 0, 0, fmt.Errorf("response delay: want 0 <= min <= max <= %d ms", maxDelayMS)
	}
	return lo, hi, nil
}

func (s *Server) applyDelay(w http.ResponseWriter, r *http.Request) {
	lo, hi, err := parseDelay(r)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	rs := s.opt.Fleet.ApplyDelay(r.Context(), lo, hi)
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash(fmt.Sprintf("Response delay %d-%d ms", lo, hi), rs))
}

func (s *Server) pendingDelay(w http.ResponseWriter, r *http.Request) {
	m := s.fleetModel()
	d := views.Diff{ApplyURL: "/fleet/delay", Form: "form-delay"}
	if lo, hi, err := parseDelay(r); err == nil && m.Ref != nil && (lo != m.Ref.DelayMS.Min || hi != m.Ref.DelayMS.Max) {
		d.Items = append(d.Items, views.DiffItem{
			What: "Response delay",
			From: fmt.Sprintf("%d–%d ms", m.Ref.DelayMS.Min, m.Ref.DelayMS.Max),
			To:   fmt.Sprintf("%d–%d ms", lo, hi),
		})
	}
	render(w, r, http.StatusOK, views.PendingBar(d))
}

// --- Session caps ---

func (s *Server) parseCaps(r *http.Request) (map[string]int64, error) {
	caps := map[string]int64{}
	for _, v := range s.opt.Fleet.Views() {
		raw, ok := r.Form["cap_"+v.Name]
		if !ok || len(raw) == 0 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(raw[0]), 10, 64)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("session cap for %s must be a whole number >= 1", v.Name)
		}
		caps[v.Name] = n
	}
	return caps, nil
}

func (s *Server) applyCaps(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	caps, err := s.parseCaps(r)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	// Only send what changes.
	for _, v := range s.opt.Fleet.Views() {
		if v.State != nil && caps[v.Name] == v.State.MaxSessions {
			delete(caps, v.Name)
		}
	}
	if len(caps) == 0 {
		s.flash(w, r, false, views.Flash{Kind: "ok", Text: "Session caps unchanged."})
		return
	}
	rs := s.opt.Fleet.ApplySessionCaps(r.Context(), caps)
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash("Session caps", rs))
}

func (s *Server) pendingCaps(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	d := views.Diff{ApplyURL: "/fleet/caps", Form: "form-caps"}
	if caps, err := s.parseCaps(r); err == nil {
		for _, v := range s.opt.Fleet.Views() {
			if n, ok := caps[v.Name]; ok && v.State != nil && n != v.State.MaxSessions {
				d.Items = append(d.Items, views.DiffItem{What: "Session cap " + v.Name, From: strconv.FormatInt(v.State.MaxSessions, 10), To: strconv.FormatInt(n, 10)})
			}
		}
	}
	render(w, r, http.StatusOK, views.PendingBar(d))
}
