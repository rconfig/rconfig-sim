package web

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/runs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

const timeFmt = "2006-01-02 15:04:05"

func runRow(r *runs.Run) views.RunRow {
	row := views.RunRow{ID: r.ID, Name: r.Name, Version: r.Version, Started: r.Started.Local().Format(timeFmt),
		Duration: r.Duration(time.Now()).String(), Outcome: r.Outcome,
		Sessions: fmtNum(r.Totals.SessionsTotal()), Bytes: jobs.HumanBytes(int64(r.Totals.Bytes)), Tags: r.Tags}
	if !r.Ended.IsZero() {
		row.Ended = r.Ended.Local().Format(timeFmt)
	}
	var faults float64
	for _, v := range r.Totals.Faults {
		faults += v
	}
	row.Faults = fmtNum(faults)
	return row
}

func fmtNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func (s *Server) runsPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.Runs == nil {
		http.NotFound(w, r)
		return
	}
	list, err := s.opt.Runs.List()
	m := views.RunsModel{Tag: r.URL.Query().Get("tag")}
	if err != nil {
		m.Err = err.Error()
	}
	for _, run := range list {
		if m.Tag != "" && !run.HasTag(m.Tag) {
			continue
		}
		m.Rows = append(m.Rows, runRow(run))
	}
	render(w, r, http.StatusOK, views.RunsPage(s.page("Runs", "runs"), m))
}

// getRun reads a run; the active one comes from memory, so its totals are
// current.
func (s *Server) getRun(id string) (*runs.Run, error) {
	if s.opt.Bench != nil {
		if run := s.opt.Bench.Run(); run != nil && run.ID == id {
			return run, nil
		}
	}
	return s.opt.Runs.Get(id)
}

func totalsColumns() []string {
	cols := []string{}
	for _, o := range fleet.Outcomes {
		cols = append(cols, "sessions "+o)
	}
	for _, f := range fault.AllTypes {
		cols = append(cols, f.String())
	}
	return append(cols, "sent", "peak active")
}

func totalsRow(name string, t *runs.Totals) views.RunTotalsRow {
	row := views.RunTotalsRow{Name: name}
	for _, o := range fleet.Outcomes {
		row.Values = append(row.Values, fmtNum(t.Sessions[o]))
	}
	for _, f := range fault.AllTypes {
		row.Values = append(row.Values, fmtNum(t.Faults[f.String()]))
	}
	row.Values = append(row.Values, jobs.HumanBytes(int64(t.Bytes)), fmtNum(t.PeakActive))
	return row
}

func (s *Server) runPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.Runs == nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.getRun(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m := views.RunModel{Row: runRow(run), Reason: run.Reason, Snapshot: string(run.Testbench), Columns: totalsColumns(),
		Build: run.Build, Notes: run.Notes, Tags: strings.Join(run.Tags, ", "), Incomplete: run.Incomplete()}
	m.Totals = []views.RunTotalsRow{totalsRow("fleet", run.Totals)}
	for _, n := range slices.Sorted(maps.Keys(run.Instances)) {
		m.Instances = append(m.Instances, totalsRow(n, run.Instances[n]))
	}
	for _, e := range run.Events {
		m.Events = append(m.Events, views.RunEvent{At: e.At.Local().Format(timeFmt), Kind: e.Kind, Text: e.Text})
	}
	render(w, r, http.StatusOK, views.RunPage(s.page("Run "+run.ID, "runs"), m))
}

func (s *Server) runExport(w http.ResponseWriter, r *http.Request) {
	if s.opt.Runs == nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.getRun(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Query().Get("format") {
	case "json", "":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "run-"+run.ID+".json"))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(run)
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "run-"+run.ID+".csv"))
		_ = runs.WriteCSV(w, run)
	default:
		http.Error(w, "format must be json or csv", http.StatusBadRequest)
	}
}

// runNotes saves a run's notes and tags; the active run's go through the
// testbench manager, so its own saves keep them.
func (s *Server) runNotes(w http.ResponseWriter, r *http.Request) {
	if s.opt.Runs == nil {
		http.NotFound(w, r)
		return
	}
	id, notes, tags := r.PathValue("id"), r.FormValue("notes"), runs.SplitTags(r.FormValue("tags"))
	done := false
	var err error
	if s.opt.Bench != nil {
		done, err = s.opt.Bench.Annotate(id, notes, tags)
	}
	if !done {
		_, err = s.opt.Runs.Update(id, func(run *runs.Run) error { return run.SetNotes(notes, tags) })
	}
	if err != nil {
		s.fail(w, r, "Not saved: "+err.Error())
		return
	}
	s.flash(w, r, false, views.Flash{Kind: "ok", Text: "Notes and tags saved."})
}

func (s *Server) runsCompare(w http.ResponseWriter, r *http.Request) {
	if s.opt.Runs == nil {
		http.NotFound(w, r)
		return
	}
	ids := r.URL.Query()["id"]
	if len(ids) != 2 {
		http.Error(w, "tick exactly two runs to compare", http.StatusBadRequest)
		return
	}
	a, errA := s.getRun(ids[0])
	b, errB := s.getRun(ids[1])
	if errA != nil || errB != nil {
		http.NotFound(w, r)
		return
	}
	c := runs.Compare(a, b)
	m := views.CompareModel{A: runRow(a), B: runRow(b)}
	for _, row := range c.Notes {
		m.Notes = append(m.Notes, [3]string{row.Label, row.A, row.B})
	}
	for _, row := range c.Totals {
		m.Totals = append(m.Totals, [3]string{row.Label, row.A, row.B})
	}
	for _, row := range c.Settings {
		m.Settings = append(m.Settings, [3]string{row.Label, row.A, row.B})
	}
	render(w, r, http.StatusOK, views.ComparePage(s.page("Compare runs", "runs"), m))
}
