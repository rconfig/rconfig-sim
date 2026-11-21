package web

import (
	"cmp"
	"errors"
	"fmt"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
	ui "github.com/rcfg-sim/rcfg-sim/internal/ui/web/views/components"
)

// jobPoll is how often a job stream checks for new log lines.
const jobPoll = 200 * time.Millisecond

func buildInfo(s *jobs.Settings) *views.BuildInfo {
	if s == nil {
		return nil
	}
	b := &views.BuildInfo{Name: s.Name, Count: s.Count, Adopted: s.Adopted, Seed: "unknown", Distribution: "unknown",
		Created: s.Created.UTC().Format("2006-01-02 15:04 UTC")}
	if s.Seed != nil {
		b.Seed = strconv.FormatInt(*s.Seed, 10)
	}
	switch {
	case s.Scenario != "":
		b.Seed, b.Distribution = "none (installed, not generated)", "scenario "+s.Scenario
	case s.Distribution != "":
		b.Distribution = s.Distribution
	case len(s.Counts) > 0:
		b.Distribution = countsLabel(s.Counts)
	}
	return b
}

// countsLabel is per-model counts for display, largest first.
func countsLabel(counts map[string]int) string {
	type mc struct {
		m string
		n int
	}
	var all []mc
	for m, n := range counts {
		all = append(all, mc{m, n})
	}
	slices.SortFunc(all, func(a, b mc) int { return cmp.Or(b.n-a.n, strings.Compare(a.m, b.m)) })
	var parts []string
	for _, x := range all {
		parts = append(parts, fmt.Sprintf("%s %d", x.m, x.n))
	}
	return strings.Join(parts, ", ")
}

// countsOf is a build's per-model counts for the Build form: its counts
// if it recorded them, else its distribution spread over its count, else
// the default distribution over count (clamped to what fits).
func countsOf(cat *catalogue.Catalogue, live *jobs.Settings, count int) map[string]int {
	if len(live.Counts) > 0 {
		return live.Counts
	}
	if live.Distribution != "" {
		if w, err := cat.ParseDistribution(live.Distribution); err == nil {
			return cat.Counts(count, w)
		}
	}
	w, _ := cat.ParseDistribution(jobs.DefaultDistribution)
	return cat.Counts(count, w)
}

// countRows is the Build form's count rows, one group per family, the
// same component as the guided flow's device panels.
func countRows(cat *catalogue.Catalogue, counts map[string]int) []views.CountGroup {
	total := sumCounts(counts)
	var out []views.CountGroup
	for _, fam := range cat.Families {
		g := views.CountGroup{Name: fam.Name}
		for _, mod := range cat.ModelsIn(fam.ID) {
			n := counts[mod.ID]
			share := 0.0
			if total > 0 {
				share = 100 * float64(n) / float64(total)
			}
			d, _ := cat.Driver(mod.Driver)
			g.Rows = append(g.Rows, ui.Count{Name: "count_" + mod.ID, Model: mod.ID, Description: mod.Vendor + ". " + mod.Description,
				Protocol: strings.ToUpper(d.Protocol), AvgSize: jobs.HumanBytes(mod.AvgBytes), Value: n, Share: share})
		}
		if len(g.Rows) > 0 {
			out = append(out, g)
		}
	}
	return out
}

func sumCounts(c map[string]int) int {
	n := 0
	for _, v := range c {
		n += v
	}
	return n
}

func jobModel(j *jobs.Job) *views.JobModel {
	if j == nil {
		return nil
	}
	lines, done := j.Lines(0)
	m := &views.JobModel{ID: j.ID, Kind: j.Kind, Lines: lines, Finished: done, OfferRollback: j.OfferRollback()}
	if err := j.Err(); err != nil {
		m.Err = err.Error()
	}
	return m
}

func (s *Server) buildPage(w http.ResponseWriter, r *http.Request) {
	p := s.page("Build", "build")
	p.Scripts = []string{"/static/vendor/htmx-ext-sse-2.2.4.min.js"}
	m := views.BuildModel{DataDir: "/opt/rcfg-sim/data"}
	if s.opt.Jobs == nil {
		m.Err = "Build jobs are not configured."
		render(w, r, http.StatusOK, views.BuildPage(p, m))
		return
	}
	st := s.opt.Jobs.Status()
	cat, catErr := s.opt.Catalogue.Get()
	var na *jobs.NotAdoptedError
	switch {
	case catErr != nil:
		m.Err = catErr.Error()
	case errors.As(st.Err, &na):
		m.NotAdopted = na.Reason
	case st.Err != nil:
		m.Err = st.Err.Error()
	default:
		l := st.Layout
		m.DataDir = l.Root
		m.Live, m.Previous = buildInfo(st.Live), buildInfo(st.Previous)
		m.Instances, m.PerIP = l.IPCount, l.DevicesPerIP
		m.MinCount, m.MaxCount = l.CountBounds()
		m.Seed, m.Unknowns = jobs.DefaultSeed, st.Live.Seed == nil || (st.Live.Distribution == "" && len(st.Live.Counts) == 0)
		if st.Live.Seed != nil {
			m.Seed = *st.Live.Seed
		}
		counts := countsOf(cat, st.Live, min(max(st.Live.Count, m.MinCount), m.MaxCount))
		m.Groups, m.Count = countRows(cat, counts), sumCounts(counts)
	}
	m.Job = jobModel(s.opt.Jobs.Current())
	render(w, r, http.StatusOK, views.BuildPage(p, m))
}

// parseBuild reads the Build form: seed and a count_<model> per model.
// The fleet total is their sum.
func parseBuild(r *http.Request, cat *catalogue.Catalogue) (jobs.Request, error) {
	if err := r.ParseForm(); err != nil {
		return jobs.Request{}, err
	}
	var req jobs.Request
	var err error
	if req.Seed, err = strconv.ParseInt(strings.TrimSpace(r.Form.Get("seed")), 10, 64); err != nil {
		return req, errors.New("seed must be a whole number")
	}
	req.Counts = map[string]int{}
	for _, model := range cat.ModelIDs() {
		raw := strings.TrimSpace(r.Form.Get("count_" + model))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return req, fmt.Errorf("%s: devices must be a whole number, 0 or more", model)
		}
		if n > 0 {
			req.Counts[model] = n
		}
	}
	if sumCounts(req.Counts) == 0 {
		return req, errors.New("set at least one device")
	}
	return req, nil
}

// estimateModel is the stacked bar of counts by model, in catalogue order.
func estimateModel(cat *catalogue.Catalogue, counts map[string]int) views.EstimateModel {
	m := views.EstimateModel{Total: sumCounts(counts)}
	if m.Total == 0 {
		return m
	}
	x := 0
	for i, model := range cat.ModelIDs() {
		n := counts[model]
		if n <= 0 {
			continue
		}
		w := n * 100 / m.Total
		width := max(1, min(w, 100-x))
		m.Bars = append(m.Bars, views.Bar{Model: model, Count: n, Pct: w, X: strconv.Itoa(x), W: strconv.Itoa(width),
			Class: views.ModelFills[i%len(views.ModelFills)]})
		x += width
	}
	return m
}

func (s *Server) buildEstimate(w http.ResponseWriter, r *http.Request) {
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		render(w, r, http.StatusOK, views.EstimatePanel(views.EstimateModel{Err: err.Error()}))
		return
	}
	req, err := parseBuild(r, cat)
	m := estimateModel(cat, req.Counts)
	if err != nil {
		m.Err = err.Error()
		render(w, r, http.StatusOK, views.EstimatePanel(m))
		return
	}
	if s.opt.Jobs == nil {
		m.Err = "Build jobs are not configured."
		render(w, r, http.StatusOK, views.EstimatePanel(m))
		return
	}
	plan, err := s.opt.Jobs.Plan(req)
	if plan != nil {
		e := plan.Estimate
		m.Disk = jobs.HumanBytes(e.DiskBytes) + " (refused unless " + jobs.HumanBytes(e.NeedFree) + " is free)"
		m.RAM = jobs.HumanBytes(e.ConfigBytes)
		m.Free = jobs.HumanBytes(plan.Free)
		m.Fits = plan.Free >= e.NeedFree
		m.Generate = "about " + e.Generate.Round(time.Second).String()
		m.Restart = fmt.Sprintf("%d instances, at least %s of pauses plus each start", plan.Instances, plan.Restart)
		m.Unknowns = plan.Unknowns
	}
	if err != nil {
		m.Err = err.Error()
		var ce *jobs.CountError
		if errors.As(err, &ce) {
			m.Steps = ce.Steps
		}
	} else {
		m.Ready = true
	}
	render(w, r, http.StatusOK, views.EstimatePanel(m))
}

// buildBusy refuses a job while a Fleet-screen rolling restart runs: the
// two would restart the same units.
func (s *Server) buildBusy() bool {
	s.rollMu.Lock()
	defer s.rollMu.Unlock()
	return s.rolling.Running
}

func (s *Server) buildRun(w http.ResponseWriter, r *http.Request) {
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	req, err := parseBuild(r, cat)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	if s.opt.Jobs == nil || s.buildBusy() {
		s.fail(w, r, "A rolling restart is running; build when it has finished.")
		return
	}
	j, err := s.opt.Jobs.StartBuild(req)
	if err != nil {
		s.fail(w, r, "Build refused: "+err.Error())
		return
	}
	s.opt.Logger.Info("build started", "job", j.ID, "counts", countsLabel(req.Counts), "seed", req.Seed)
	render(w, r, http.StatusOK, views.JobPanel(*jobModel(j)))
}

func (s *Server) buildRollback(w http.ResponseWriter, r *http.Request) {
	if s.opt.Jobs == nil || s.buildBusy() {
		s.fail(w, r, "A rolling restart is running; roll back when it has finished.")
		return
	}
	j, err := s.opt.Jobs.StartRollback()
	if err != nil {
		s.fail(w, r, "Rollback refused: "+err.Error())
		return
	}
	s.opt.Logger.Info("rollback started", "job", j.ID)
	render(w, r, http.StatusOK, views.JobPanel(*jobModel(j)))
}

// buildJobStream streams a job's log lines (event "line", HTML-escaped,
// swapped onto the end of the log), then its outcome ("state") and "done",
// which closes the browser's EventSource.
func (s *Server) buildJobStream(w http.ResponseWriter, r *http.Request) {
	if s.opt.Jobs == nil {
		http.NotFound(w, r)
		return
	}
	j, ok := s.opt.Jobs.Job(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := sseWriter{w: w, rc: http.NewResponseController(w)}
	if _, err := w.Write([]byte(": connected\n\n")); err != nil || sw.rc.Flush() != nil {
		return
	}
	next := 0
	tick := time.NewTicker(jobPoll)
	defer tick.Stop()
	for {
		lines, done := j.Lines(next)
		if len(lines) > 0 {
			var b strings.Builder
			for _, l := range lines {
				b.WriteString(html.EscapeString(l))
				b.WriteString("&#10;") // a newline that survives the SSE data framing
			}
			if sw.send("line", b.String()) != nil {
				return
			}
			next += len(lines)
		}
		if done {
			var b strings.Builder
			_ = views.JobState(*jobModel(j)).Render(r.Context(), &b)
			if sw.send("state", b.String()) != nil || sw.send("done", "") != nil {
				return
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}
