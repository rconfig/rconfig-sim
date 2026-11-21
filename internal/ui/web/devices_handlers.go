package web

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/inventory"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/testbench"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// deviceStatesTTL is how long one fetch of every instance's device table
// serves the Devices screen.
const deviceStatesTTL = 2 * time.Second

// scope is the build a Devices view shows: the live one, a testbench's or a
// run's.
type scope struct {
	label    string // heading
	name     string // for export file names: build or testbench name
	query    string // testbench=... or run=...; "" for the live build
	build    string
	manifest string
	live     bool
	missing  string // the build is not kept (or none is set up)
	offer    string // a testbench slug to offer "Build it" for
}

func (s *Server) scopeOf(r *http.Request) scope {
	q := r.URL.Query()
	switch {
	case q.Get("run") != "":
		id := q.Get("run")
		sc := scope{query: "run=" + url.QueryEscape(id)}
		run, err := s.getRun(id)
		if err != nil {
			sc.missing = "No run " + id + "."
			return sc
		}
		sc.label, sc.name = "run "+run.ID+" ("+run.Name+")", run.Name
		if run.Build == "" {
			sc.missing = "This run did not record its build (it started before builds were recorded, or failed before one was ready)."
			return sc
		}
		dir, live, ok := s.buildDir(run.Build)
		if !ok {
			sc.missing = "The build this run used, " + run.Build + ", is no longer kept (retention removed it), so its devices cannot be listed."
			return sc
		}
		sc.build, sc.manifest, sc.live = run.Build, filepath.Join(dir, "manifest.csv"), live
		return sc
	case q.Get("testbench") != "":
		slug := q.Get("testbench")
		sc := scope{query: "testbench=" + url.QueryEscape(slug)}
		if s.opt.Testbenches == nil || s.opt.Jobs == nil {
			sc.missing = "Testbenches are not enabled."
			return sc
		}
		tb, err := s.opt.Testbenches.Get(slug)
		if err != nil {
			sc.missing = err.Error()
			return sc
		}
		sc.label, sc.name = "testbench "+tb.Name, tb.Name
		cat, err := s.opt.Catalogue.Get()
		if err != nil {
			sc.missing = err.Error()
			return sc
		}
		name, dir, live, ok := s.opt.Jobs.KeptBuild(tb.Devices.Request(cat))
		if !ok {
			sc.missing = "No kept build has this testbench's build settings yet. Build it to list its devices, or start the testbench."
			st := s.opt.Jobs.Status()
			if len(testbench.Fit(tb, st.Layout, st.Err)) == 0 {
				sc.offer = slug
			}
			return sc
		}
		sc.build, sc.manifest, sc.live = name, filepath.Join(dir, "manifest.csv"), live
		return sc
	}
	return s.liveScope()
}

// liveScope is the build the instances are serving.
func (s *Server) liveScope() scope {
	sc := scope{live: true}
	if s.opt.Jobs != nil {
		if st := s.opt.Jobs.Status(); st.Err == nil && st.Live != nil {
			sc.build, sc.manifest = st.Live.Name, st.Layout.Manifest
			sc.label, sc.name = "live build "+st.Live.Name, st.Live.Name
			return sc
		}
	}
	// Not on the data/ layout: the instances' shared manifest.
	for _, v := range s.opt.Fleet.Views() {
		if v.Err == nil && v.Manifest != "" {
			sc.build, sc.manifest = "manifest", v.Manifest
			sc.label, sc.name = v.Manifest, "manifest"
			return sc
		}
	}
	sc.missing = "No instances, so no manifest to list."
	return sc
}

func (s *Server) buildDir(name string) (string, bool, bool) {
	if s.opt.Jobs == nil {
		return "", false, false
	}
	return s.opt.Jobs.BuildDir(name)
}

// deviceStates returns every up instance's /admin/v1/devices answer,
// cached for deviceStatesTTL.
func (s *Server) deviceStates(ctx context.Context) map[string]*admin.Devices {
	s.devMu.Lock()
	defer s.devMu.Unlock()
	if s.devCache != nil && time.Since(s.devAt) < deviceStatesTTL {
		return s.devCache
	}
	out := map[string]*admin.Devices{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, v := range s.opt.Fleet.Views() {
		if v.Status != fleet.StatusUp {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := s.opt.Fleet.FetchDevices(ctx, v.Name); err == nil {
				mu.Lock()
				out[v.Name] = d
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	s.devCache, s.devAt = out, time.Now()
	return out
}

// rows is the scope's devices with their status.
func (s *Server) rows(ctx context.Context, sc scope) ([]inventory.Row, *inventory.Index, error) {
	x, err := s.inv.Load(sc.build, sc.manifest)
	if err != nil {
		return nil, nil, err
	}
	live := inventory.Live{Views: s.opt.Fleet.Views(), LiveBuild: sc.live}
	if cat, err := s.opt.Catalogue.Get(); err == nil {
		live.FamilyOf = func(model, template string) string {
			if f := cat.FamilyOfModel(model); f != "" {
				return f
			}
			return cat.FamilyOfTemplate(template)
		}
	}
	if sc.live {
		live.Devices = s.deviceStates(ctx)
	}
	if s.opt.Bench != nil {
		live.Benches, live.BenchRunning = true, s.opt.Bench.Active().State == testbench.Running
	}
	return inventory.Join(x, live), x, nil
}

func deviceRow(r *inventory.Row, sc scope, cat *catalogue.Catalogue) views.DeviceRow {
	d := views.DeviceRow{Hostname: r.Hostname, IP: r.IP, Port: r.Port, Instance: r.Instance, Model: r.Model, Family: familyLabel(cat, r.Family),
		Username: r.Username, Password: r.Password, Profile: r.Profile, Status: r.Status, Reason: r.Reason,
		LastOutcome: r.LastOutcome, Href: "/device/" + url.PathEscape(r.Hostname)}
	if sc.query != "" {
		d.Href += "?" + sc.query
	}
	if !r.LastSeen.IsZero() {
		d.LastSeen = r.LastSeen.Local().Format(timeFmt)
	}
	return d
}

// filterHref is the Devices URL with one filter set, keeping the scope.
func filterHref(sc scope, key, val string) string {
	v := url.Values{key: {val}}
	if sc.query != "" {
		extra, _ := url.ParseQuery(sc.query)
		for k, vs := range extra {
			v[k] = vs
		}
	}
	return "/devices?" + v.Encode()
}

var deviceColumns = []struct{ label, sort string }{
	{"Hostname", "hostname"}, {"IP", "ip"}, {"Port", "port"}, {"Model", "model"}, {"Login", ""},
	{"Profile", ""}, {"Status", "status"}, {"Last outcome", "outcome"}, {"Last seen", "last_seen"},
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request) {
	sc := s.scopeOf(r)
	q := inventory.ParseQuery(r.URL.Query())
	m := views.DevicesModel{Scope: sc.label, ScopeQuery: sc.query, Build: sc.build, Missing: sc.missing, BuildSlug: sc.offer,
		NotLive: sc.missing == "" && !sc.live, Open: r.URL.Query().Get("open"),
		Filters: views.DeviceFilters{Status: q.Status, Reason: q.Reason, Family: q.Family, Instance: q.Instance, Model: q.Model,
			Profile: q.Profile, Outcome: q.Outcome, Q: q.Q}}
	full := q.Values()
	if sc.query != "" {
		extra, _ := url.ParseQuery(sc.query)
		maps.Copy(full, extra)
	}
	m.Query = full.Encode()
	var sorts []views.SortLink
	exportV := q.Filters().Values()
	if sc.query != "" {
		extra, _ := url.ParseQuery(sc.query)
		maps.Copy(exportV, extra)
	}
	if sc.missing == "" {
		all, _, err := s.rows(r.Context(), sc)
		if err != nil {
			m.Missing = "Could not read the manifest: " + err.Error()
		} else {
			sum := inventory.Summarise(all)
			m.Summary = views.DeviceSummary{Total: sum.Total, Active: sum.Active, Unknown: sum.Unknown}
			for _, reason := range slices.Sorted(maps.Keys(sum.Inactive)) {
				m.Summary.Inactive = append(m.Summary.Inactive, views.KVCount{Label: "inactive: " + reason, Count: sum.Inactive[reason], Href: filterHref(sc, "reason", reason)})
			}
			cat, _ := s.opt.Catalogue.Get()
			for _, fid := range slices.Sorted(maps.Keys(sum.Families)) {
				m.Summary.Families = append(m.Summary.Families, views.KVCount{Label: familyLabel(cat, fid), Count: sum.Families[fid], Href: filterHref(sc, "family", fid)})
			}
			if cat != nil {
				for _, f := range cat.Families {
					m.Families = append(m.Families, f.ID)
				}
			}
			for _, model := range slices.Sorted(maps.Keys(sum.Models)) {
				m.Summary.Models = append(m.Summary.Models, views.KVCount{Label: model, Count: sum.Models[model], Href: filterHref(sc, "model", model)})
				m.Models = append(m.Models, model)
			}
			insts := map[string]bool{}
			for i := range all {
				if all[i].Instance != "" {
					insts[all[i].Instance] = true
				}
			}
			m.Instances = slices.Sorted(maps.Keys(insts))
			matched := inventory.Filter(all, q)
			m.Matched = len(matched)
			page, pages := inventory.Page(matched, q)
			m.Page, m.Pages = min(max(q.Page, 1), pages), pages
			for i := range page {
				m.Rows = append(m.Rows, deviceRow(&page[i], sc, cat))
			}
		}
	}
	for _, c := range deviceColumns {
		l := views.SortLink{Label: c.label}
		if c.sort != "" {
			v, _ := url.ParseQuery(m.Query)
			v.Del("page")
			cur := q.Sort
			if cur == "" {
				cur = "hostname"
			}
			l.Active, l.Desc = cur == c.sort, q.Desc
			v.Set("sort", c.sort)
			v.Del("desc")
			if l.Active && !q.Desc {
				v.Set("desc", "1")
			}
			l.Href = "/devices?" + v.Encode()
		}
		sorts = append(sorts, l)
	}
	exportV.Set("format", "csv")
	csvHref := "/devices/export?" + exportV.Encode()
	exportV.Set("format", "json")
	jsonHref := "/devices/export?" + exportV.Encode()
	render(w, r, http.StatusOK, views.DevicesPage(s.page("Devices", "devices"), m, sorts, csvHref, jsonHref))
}

// devicesFind is the header search: an exact hostname opens its drawer,
// anything else searches.
func (s *Server) devicesFind(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sc := s.scopeOf(r)
	if q != "" && sc.missing == "" {
		if x, err := s.inv.Load(sc.build, sc.manifest); err == nil {
			if _, ok := x.Lookup(q); ok {
				http.Redirect(w, r, "/devices?"+url.Values{"q": {q}, "open": {q}}.Encode(), http.StatusSeeOther)
				return
			}
		}
	}
	http.Redirect(w, r, "/devices?"+url.Values{"q": {q}}.Encode(), http.StatusSeeOther)
}

// findRow is one device of the scope, with its status.
func (s *Server) findRow(r *http.Request, sc scope, host string) (*inventory.Row, error) {
	if sc.missing != "" {
		return nil, fmt.Errorf("%s", sc.missing)
	}
	all, _, err := s.rows(r.Context(), sc)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Hostname == host {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("no device %q in %s", host, sc.label)
}

func (s *Server) deviceDrawer(w http.ResponseWriter, r *http.Request) {
	sc := s.scopeOf(r)
	host := r.PathValue("host")
	row, err := s.findRow(r, sc, host)
	if err != nil {
		render(w, r, http.StatusOK, views.DeviceDrawer(views.DeviceDrawerModel{Err: err.Error()}))
		return
	}
	if r.Header.Get("HX-Request") != "true" {
		// A plain link (no htmx): the page with the drawer open.
		v := url.Values{"q": {host}, "open": {host}}
		if sc.query != "" {
			extra, _ := url.ParseQuery(sc.query)
			maps.Copy(v, extra)
		}
		http.Redirect(w, r, "/devices?"+v.Encode(), http.StatusSeeOther)
		return
	}
	cat, _ := s.opt.Catalogue.Get()
	m := views.DeviceDrawerModel{Row: deviceRow(row, sc, cat), EnablePassword: row.EnablePassword, Template: row.Template,
		ProfilePinned: row.ProfilePin, TestCommand: inventory.TestCommand(row.Template),
		SSH: fmt.Sprintf("ssh -p %d %s@%s", row.Port, row.Username, row.IP)}
	if sc.live && row.Instance != "" {
		m.TestURL = "/device/" + url.PathEscape(host) + "/test-login"
	}
	for _, ev := range s.hub.Recent(host, recentPerDevice) {
		m.Events = append(m.Events, eventLine(ev))
	}
	render(w, r, http.StatusOK, views.DeviceDrawer(m))
}

// deviceTestLogin runs Test login on a live-build device, one at a time per
// browser session.
func (s *Server) deviceTestLogin(w http.ResponseWriter, r *http.Request) {
	sid := sessionID(r)
	s.testMu.Lock()
	if s.testing[sid] {
		s.testMu.Unlock()
		render(w, r, http.StatusOK, views.LoginTestResult(views.LoginTestModel{Stage: "start", Err: "a Test login is already running in this browser; wait for it"}))
		return
	}
	s.testing[sid] = true
	s.testMu.Unlock()
	defer func() { s.testMu.Lock(); delete(s.testing, sid); s.testMu.Unlock() }()

	row, err := s.findRow(r, s.liveScope(), r.PathValue("host"))
	if err != nil {
		render(w, r, http.StatusOK, views.LoginTestResult(views.LoginTestModel{Stage: "start", Err: err.Error()}))
		return
	}
	addr := net.JoinHostPort(row.IP, strconv.Itoa(row.Port))
	res := s.testLogin(r.Context(), addr, row.Device, inventory.TestTimeout)
	var faults map[string]float64
	if v, ok := s.opt.Fleet.View(row.Instance); ok && v.State != nil {
		faults = v.State.Faults
	}
	m := views.LoginTestModel{OK: res.OK, Stage: res.Stage, Command: res.Command, Output: res.Output, Err: res.Err,
		LoginTime: res.LoginTime.Round(time.Millisecond).String(), Total: res.Total.Round(time.Millisecond).String(),
		Hint: inventory.Explain(res, faults, row.Profile)}
	s.opt.Logger.Info("test login", "host", row.Hostname, "ok", res.OK, "stage", res.Stage, "err", res.Err)
	render(w, r, http.StatusOK, views.LoginTestResult(m))
}

// devicesExport streams exactly the rows the filters show, as CSV or JSON
// (Phase 7 item 4). Also reachable with HTTP Basic, for scripts.
func (s *Server) devicesExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "json" {
		http.Error(w, "format must be csv or json", http.StatusBadRequest)
		return
	}
	sc := s.scopeOf(r)
	if sc.missing != "" {
		http.Error(w, sc.missing, http.StatusNotFound)
		return
	}
	all, _, err := s.rows(r.Context(), sc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	q := inventory.ParseQuery(r.URL.Query()).Filters()
	rows := inventory.Filter(all, q)
	name := inventory.FileName(sc.name, q.Status, time.Now(), format)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	rc := http.NewResponseController(w)
	flush := func() { _ = rc.Flush() }
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		err = inventory.WriteCSV(w, rows, flush)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		err = inventory.WriteJSON(w, rows, flush)
	}
	if err != nil {
		s.opt.Logger.Warn("device export cut short", "err", err)
		return
	}
	s.markExported()
	s.opt.Logger.Info("devices exported", "scope", sc.label, "format", format, "rows", len(rows))
}

func (s *Server) testbenchKeepBuild(w http.ResponseWriter, r *http.Request) {
	if s.opt.Testbenches == nil || s.opt.Jobs == nil {
		http.NotFound(w, r)
		return
	}
	if s.buildBusy() {
		s.fail(w, r, "A rolling restart is running; build when it has finished.")
		return
	}
	tb, err := s.opt.Testbenches.Get(r.PathValue("slug"))
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	j, err := s.opt.Jobs.StartKeptBuild(tb.Devices.Request(cat))
	if err != nil {
		s.fail(w, r, "Build refused: "+err.Error())
		return
	}
	render(w, r, http.StatusOK, views.JobPanel(*jobModel(j)))
}

// --- First-login checklist (Phase 7 item 7) ---

type checklistState struct {
	Dismissed bool `json:"dismissed"`
	Exported  bool `json:"exported"`
}

func (s *Server) checklistPath() string {
	if s.opt.DataDir == "" {
		return ""
	}
	return filepath.Join(s.opt.DataDir, "checklist.json")
}

func (s *Server) readChecklist() checklistState {
	var st checklistState
	if p := s.checklistPath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}
	return st
}

func (s *Server) writeChecklist(st checklistState) {
	p := s.checklistPath()
	if p == "" {
		return
	}
	b, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err == nil {
		tmp := p + ".tmp"
		if os.WriteFile(tmp, b, 0o640) == nil {
			_ = os.Rename(tmp, p)
		}
	}
}

func (s *Server) markExported() {
	s.clMu.Lock()
	defer s.clMu.Unlock()
	if st := s.readChecklist(); !st.Exported {
		st.Exported = true
		s.writeChecklist(st)
	}
}

// checklist is the model, or nil once dismissed.
func (s *Server) checklist() *views.ChecklistModel {
	st := s.readChecklist()
	if st.Dismissed {
		return nil
	}
	vs := s.opt.Fleet.Views()
	up, admin := len(vs) > 0, len(vs) > 0
	for _, v := range vs {
		if v.Status != fleet.StatusUp {
			up = false
		}
		if v.Status == fleet.StatusAdminOff || v.Status == fleet.StatusError {
			admin = false
		}
	}
	started := false
	if s.opt.Runs != nil {
		if rs, _ := s.opt.Runs.List(); len(rs) > 0 {
			started = true
		}
	}
	m := &views.ChecklistModel{Items: []views.ChecklistItem{
		{Text: "Every instance is up", Href: "/fleet", Done: up},
		{Text: "The admin API answers on every instance", Href: "/fleet", Done: admin},
		{Text: "Start a testbench", Href: "/testbenches", Done: started},
		{Text: "Export the devices (CSV or JSON)", Href: "/devices", Done: st.Exported},
	}}
	for _, it := range m.Items {
		if it.Done {
			m.Done++
		}
	}
	return m
}

func (s *Server) checklistView(w http.ResponseWriter, r *http.Request) {
	m := s.checklist()
	if m == nil {
		w.WriteHeader(http.StatusOK) // dismissed: the section empties
		return
	}
	render(w, r, http.StatusOK, views.Checklist(*m))
}

func (s *Server) checklistDismiss(w http.ResponseWriter, r *http.Request) {
	s.clMu.Lock()
	st := s.readChecklist()
	st.Dismissed = true
	s.writeChecklist(st)
	s.clMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// familyLabel is a family's badge text from the catalogue ("IP",
// "Optical"); the id when the catalogue is not available.
func familyLabel(cat *catalogue.Catalogue, id string) string {
	if id == "" || cat == nil {
		return id
	}
	if f, ok := cat.Family(id); ok {
		return shortFamily(f)
	}
	return id
}
