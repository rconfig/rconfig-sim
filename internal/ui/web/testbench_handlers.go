package web

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/testbench"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// maxImport bounds an uploaded testbench file.
const maxImport = 1 << 20

// benchPill is the header pill's model.
func (s *Server) benchPill() views.BenchPill {
	if s.opt.Bench == nil {
		return views.BenchPill{}
	}
	a := s.opt.Bench.Active()
	b := views.BenchPill{State: a.State, Name: a.Name, Modified: a.Modified, Error: a.Error, RunID: a.RunID}
	if a.State != testbench.Idle {
		b.Elapsed = a.Elapsed(time.Now()).String()
	}
	if !a.StopAt.IsZero() && a.State == testbench.Running {
		b.StopIn = max(time.Until(a.StopAt), 0).Round(time.Second).String()
	}
	return b
}

func (s *Server) benchPillView(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.BenchPillView(s.benchPill()))
}

// noteChange records a setting change made on Fleet or Failures in the
// active run and marks its testbench modified.
func (s *Server) noteChange(text string) {
	if s.opt.Bench != nil {
		s.opt.Bench.Changed(text)
	}
}

// benchBusy refuses a lifecycle action while a Fleet-screen rolling
// restart runs, as builds do.
func (s *Server) benchBusy(w http.ResponseWriter, r *http.Request) bool {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return true
	}
	if s.buildBusy() {
		s.fail(w, r, "A rolling restart is running; try again when it has finished.")
		return true
	}
	return false
}

func parseOverride(faults, seed, rules, delay, maxSessions, active string) (testbench.Override, error) {
	var o testbench.Override
	if faults != "" {
		set, err := fault.ParseRates(faults)
		if err != nil {
			return o, err
		}
		o.Faults = set.Rates()
		for k, v := range o.Faults {
			if v == 0 {
				delete(o.Faults, k)
			}
		}
	}
	if seed != "" || rules != "" {
		p := testbench.Profiles{Rules: []string{}}
		if seed != "" {
			n, err := strconv.ParseInt(seed, 10, 64)
			if err != nil {
				return o, fmt.Errorf("profile seed must be a whole number")
			}
			p.Seed = n
		}
		for _, it := range strings.Split(rules, ",") {
			if it = strings.TrimSpace(it); it != "" {
				p.Rules = append(p.Rules, it)
			}
		}
		o.Profiles = &p
	}
	if delay != "" {
		lo, hi, ok := strings.Cut(delay, "-")
		a, err1 := strconv.ParseInt(strings.TrimSpace(lo), 10, 64)
		b, err2 := strconv.ParseInt(strings.TrimSpace(hi), 10, 64)
		if !ok || err1 != nil || err2 != nil {
			return o, fmt.Errorf("delay %q: want min-max in ms, e.g. 50-500", delay)
		}
		o.DelayMS = &testbench.DelayMS{Min: a, Max: b}
	}
	if maxSessions != "" {
		n, err := strconv.ParseInt(maxSessions, 10, 64)
		if err != nil {
			return o, fmt.Errorf("session cap must be a whole number")
		}
		o.MaxSessions = &n
	}
	if active != "" {
		a, err := testbench.ParseActive(active)
		if err != nil {
			return o, err
		}
		o.ActiveDevices = &a
	}
	return o, nil
}

func (s *Server) testbenchDuplicate(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	tb, err := s.opt.Testbenches.Get(r.PathValue("slug"))
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	tb.Name, tb.Version = s.opt.Testbenches.DuplicateName(tb.Name), 0
	saved, err := s.opt.Testbenches.Create(tb)
	if err != nil {
		s.fail(w, r, "Not duplicated: "+err.Error())
		return
	}
	w.Header().Set("HX-Redirect", "/testbenches/"+saved.Slug)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) testbenchDelete(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	slug := r.PathValue("slug")
	if a := s.opt.Bench.Active(); a.State != testbench.Idle && a.Slug == slug {
		s.fail(w, r, "This testbench is active; stop it first.")
		return
	}
	if err := s.opt.Testbenches.Delete(slug); err != nil {
		s.fail(w, r, "Not deleted: "+err.Error())
		return
	}
	w.Header().Set("HX-Redirect", "/testbenches")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) testbenchExport(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	tb, err := s.opt.Testbenches.Get(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "testbench-"+tb.Slug+".json"))
	_, _ = w.Write(testbench.Encode(tb))
}

func (s *Server) testbenchImport(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImport+4096)
	file, _, err := r.FormFile("file")
	if err != nil {
		s.fail(w, r, "Import: choose a testbench JSON file.")
		return
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxImport+1))
	if err != nil || len(b) > maxImport {
		s.fail(w, r, "Import: the file is unreadable or larger than 1 MiB.")
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	tb, err := testbench.Decode(b, cat)
	if err != nil {
		s.fail(w, r, "Not imported: "+err.Error())
		return
	}
	saved, err := s.opt.Testbenches.Create(tb)
	if err != nil {
		s.fail(w, r, "Not imported: "+err.Error()+". Rename it in the file, or delete the existing one.")
		return
	}
	s.opt.Logger.Info("testbench imported", "testbench", saved.Name)
	w.Header().Set("HX-Redirect", "/testbenches/"+saved.Slug)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) testbenchStart(w http.ResponseWriter, r *http.Request) {
	if s.benchBusy(w, r) {
		return
	}
	slug := r.PathValue("slug")
	j, err := s.opt.Bench.Start(slug, r.URL.Query().Get("replace") != "")
	if errors.Is(err, testbench.ErrActive) {
		tb, _ := s.opt.Testbenches.Get(slug)
		render(w, r, http.StatusOK, views.ReplaceConfirm(s.opt.Bench.Active().Name, slug, tb.Name))
		return
	}
	if err != nil {
		s.fail(w, r, "Not started: "+err.Error())
		return
	}
	s.opt.Logger.Info("testbench start", "testbench", slug, "job", j.ID)
	w.Header().Set("HX-Trigger", "bench-changed")
	render(w, r, http.StatusOK, views.JobPanel(*jobModel(j)))
}

// benchAction runs a lifecycle action on the active testbench and answers
// with a flash pointing at the job.
func (s *Server) benchAction(w http.ResponseWriter, r *http.Request) {
	if s.benchBusy(w, r) {
		return
	}
	verb := r.PathValue("verb")
	var err error
	switch verb {
	case "stop":
		_, err = s.opt.Bench.Stop()
	case "restart":
		_, err = s.opt.Bench.Restart()
	case "revert":
		_, err = s.opt.Bench.Revert()
	case "dismiss":
		err = s.opt.Bench.Dismiss()
	case "save":
		var tb testbench.Testbench
		if tb, err = s.opt.Bench.SaveToTestbench(); err == nil {
			w.Header().Set("HX-Trigger", "bench-changed")
			s.flash(w, r, false, views.Flash{Kind: "ok", Text: fmt.Sprintf("Saved to testbench %s, version %d.", tb.Name, tb.Version)})
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, strings.ToUpper(verb[:1])+verb[1:]+" refused: "+err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "bench-changed")
	text := map[string]string{"stop": "Stopping the testbench.", "restart": "Restarting every instance with the same testbench.",
		"revert": "Re-applying the testbench's settings.", "dismiss": "Dismissed."}[verb]
	s.flash(w, r, verb != "dismiss", views.Flash{Kind: "ok", Text: text + " Progress is on the Testbenches screen."})
}

// ratesText and capsText (like rulesText) describe a change for the run timeline.
func ratesText(rates map[string]float64) string {
	set, err := fault.NewSetFromRates(rates)
	if err != nil || set.String() == "" {
		return "none"
	}
	return set.String()
}

func capsText(caps map[string]int64) string {
	var parts []string
	for _, n := range slices.Sorted(maps.Keys(caps)) {
		parts = append(parts, fmt.Sprintf("%s=%d", n, caps[n]))
	}
	return strings.Join(parts, ", ")
}
