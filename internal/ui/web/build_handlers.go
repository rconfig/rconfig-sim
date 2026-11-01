package web

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
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
	if s.Distribution != "" {
		b.Distribution = s.Distribution
	}
	return b
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
	var na *jobs.NotAdoptedError
	switch {
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
		m.Count = min(max(st.Live.Count, m.MinCount), m.MaxCount)
		m.Seed, m.Unknowns = jobs.DefaultSeed, st.Live.Seed == nil || st.Live.Distribution == ""
		if st.Live.Seed != nil {
			m.Seed = *st.Live.Seed
		}
		dist := st.Live.Distribution
		if dist == "" {
			dist = jobs.DefaultDistribution
		}
		weights, err := configs.ParseDistribution(dist)
		if err != nil {
			weights, _ = configs.ParseDistribution(jobs.DefaultDistribution)
		}
		for _, model := range configs.Models() {
			m.Weights = append(m.Weights, views.WeightRow{Model: model, Pct: weights[model]})
		}
	}
	m.Job = jobModel(s.opt.Jobs.Current())
	render(w, r, http.StatusOK, views.BuildPage(p, m))
}

// parseBuild reads the Build form: count, seed and w_<model> percentages,
// turned into a --distribution string.
func parseBuild(r *http.Request) (jobs.Request, map[string]int, error) {
	if err := r.ParseForm(); err != nil {
		return jobs.Request{}, nil, err
	}
	var req jobs.Request
	var err error
	if req.Count, err = strconv.Atoi(strings.TrimSpace(r.Form.Get("count"))); err != nil {
		return req, nil, errors.New("devices must be a whole number")
	}
	if req.Seed, err = strconv.ParseInt(strings.TrimSpace(r.Form.Get("seed")), 10, 64); err != nil {
		return req, nil, errors.New("seed must be a whole number")
	}
	weights := map[string]int{}
	var parts []string
	for _, model := range configs.Models() {
		raw := strings.TrimSpace(r.Form.Get("w_" + model))
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 100 {
			return req, nil, fmt.Errorf("%s: weight must be 0 to 100", model)
		}
		weights[model] = n
		if n > 0 {
			parts = append(parts, model+":"+strconv.Itoa(n))
		}
	}
	req.Distribution = strings.Join(parts, ",")
	return req, weights, nil
}

func estimateModel(req jobs.Request, weights map[string]int) views.EstimateModel {
	m := views.EstimateModel{}
	x := 0
	for i, model := range configs.Models() {
		w := weights[model]
		m.Total += w
		if w <= 0 {
			continue
		}
		width := min(w, max(0, 100-x))
		m.Bars = append(m.Bars, views.Bar{Model: model, Pct: w, X: strconv.Itoa(x), W: strconv.Itoa(width),
			Class: views.ModelFills[i%len(views.ModelFills)]})
		x += width
	}
	return m
}

func (s *Server) buildEstimate(w http.ResponseWriter, r *http.Request) {
	req, weights, err := parseBuild(r)
	m := estimateModel(req, weights)
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
	req, _, err := parseBuild(r)
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
	s.opt.Logger.Info("build started", "job", j.ID, "count", req.Count, "seed", req.Seed, "distribution", req.Distribution)
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
