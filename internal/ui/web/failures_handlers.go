package web

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
)

// maxAffectedShown caps the hostnames the affected panel lists and copies.
const maxAffectedShown = 10_000

// failures is a parsed Failures form: what Apply would send.
type failures struct {
	rates map[string]float64
	seed  int64
	rules []string
}

// refState is the first up instance's state, and whether the up instances
// disagree on faults or profiles.
func (s *Server) refState() (ref *admin.State, mixed bool, devices int) {
	for _, v := range s.opt.Fleet.Views() {
		if v.Status != fleet.StatusUp {
			continue
		}
		devices += min(v.State.Devices, v.State.PortCount)
		if ref == nil {
			ref = v.State
			continue
		}
		if !sameFailures(ref, v.State) {
			mixed = true
		}
	}
	return ref, mixed, devices
}

func sameFailures(a, b *admin.State) bool {
	for k, v := range a.Faults {
		if b.Faults[k] != v {
			return false
		}
	}
	return a.Profiles.Seed == b.Profiles.Seed && slices.Equal(a.Profiles.Rules, b.Profiles.Rules)
}

// ruleRow splits "hang:3600s@3%" into the editor's fields.
func ruleRow(item string) views.RuleRow {
	spec, pct, _ := strings.Cut(item, "@")
	kind, param, _ := strings.Cut(strings.TrimSpace(spec), ":")
	return views.RuleRow{Kind: kind, Param: param, Pct: strings.TrimSuffix(strings.TrimSpace(pct), "%")}
}

func formModel(faults map[string]float64, rules []string) ([]views.FaultRow, []views.RuleRow) {
	rows := slices.Clone(views.FaultTypes)
	for i := range rows {
		rows[i].Pct = math.Round(faults[rows[i].Type]*100*1e4) / 1e4
	}
	var rr []views.RuleRow
	for _, it := range rules {
		rr = append(rr, ruleRow(it))
	}
	return rows, rr
}

func (s *Server) failuresPage(w http.ResponseWriter, r *http.Request) {
	ref, mixed, devices := s.refState()
	all, err := s.opt.Presets.List()
	if err != nil {
		s.opt.Logger.Warn("presets", "err", err)
	}
	m := views.FailuresModel{Devices: devices, Presets: all, Mixed: mixed, NoneUp: ref == nil, Seed: 42}
	var faults map[string]float64
	var rules []string
	if ref != nil {
		faults, rules, m.Seed = ref.Faults, ref.Profiles.Rules, ref.Profiles.Seed
	}
	if name := r.URL.Query().Get("preset"); name != "" {
		p, ok := s.opt.Presets.Get(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		faults, rules, m.Seed, m.Loaded = p.Faults, p.Profiles.Rules, p.Profiles.Seed, p.Name
	}
	m.Faults, m.Rules = formModel(faults, rules)
	render(w, r, http.StatusOK, views.FailuresPage(s.page("Failures", "failures"), m))
}

func (s *Server) ruleRow(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.RuleRowView(views.RuleRow{Kind: "hang", Param: "3600s", Pct: "1"}))
}

func (s *Server) presetList(w http.ResponseWriter, r *http.Request) {
	all, _ := s.opt.Presets.List()
	render(w, r, http.StatusOK, views.PresetList(all))
}

// parseFailures reads the Failures form: pct_<type> percentages, the rule
// rows (rule_kind, rule_param, rule_pct) and the seed. Validates with the
// same parsers rcfg-sim uses.
func parseFailures(r *http.Request) (failures, error) {
	if err := r.ParseForm(); err != nil {
		return failures{}, err
	}
	f := failures{rates: map[string]float64{}}
	for _, ft := range views.FaultTypes {
		raw := strings.TrimSpace(r.Form.Get("pct_" + ft.Type))
		if raw == "" {
			continue
		}
		pct, err := strconv.ParseFloat(raw, 64)
		if err != nil || pct < 0 || pct > 100 || math.IsNaN(pct) {
			return f, fmt.Errorf("%s must be 0 to 100%%", ft.Label)
		}
		// Rates to 1e-6, so 3.5% is stored as 0.035, not 0.035000000000000003.
		f.rates[ft.Type] = math.Round(pct/100*1e6) / 1e6
	}

	kinds, params, pcts := r.Form["rule_kind"], r.Form["rule_param"], r.Form["rule_pct"]
	if len(kinds) != len(params) || len(kinds) != len(pcts) {
		return f, fmt.Errorf("profile rules are incomplete")
	}
	f.rules = []string{}
	for i, kind := range kinds {
		param, pct := strings.TrimSpace(params[i]), strings.TrimSuffix(strings.TrimSpace(pcts[i]), "%")
		if !slices.Contains(views.RuleKinds, kind) {
			return f, fmt.Errorf("unknown profile %q", kind)
		}
		item := kind
		if param != "" {
			item += ":" + param
		}
		item += "@" + pct + "%"
		if strings.ContainsAny(item, ", ") {
			return f, fmt.Errorf("rule %q: no commas or spaces", item)
		}
		f.rules = append(f.rules, item)
	}
	if _, err := profile.Parse(strings.Join(f.rules, ",")); err != nil {
		return f, err
	}

	seed, err := strconv.ParseInt(strings.TrimSpace(r.Form.Get("seed")), 10, 64)
	if err != nil {
		return f, fmt.Errorf("seed must be a whole number")
	}
	f.seed = seed
	return f, nil
}

func (s *Server) applyFailures(w http.ResponseWriter, r *http.Request) {
	f, err := parseFailures(r)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	rs := s.opt.Fleet.ApplyFailures(r.Context(), f.rates, f.seed, f.rules)
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash("Faults and profiles", rs))
}

func (s *Server) affected(w http.ResponseWriter, r *http.Request) {
	f, err := parseFailures(r)
	if err != nil {
		render(w, r, http.StatusOK, views.AffectedList(views.AffectedModel{Err: err.Error()}))
		return
	}
	items, err := s.opt.Fleet.AffectedBy(f.seed, f.rules)
	m := views.AffectedModel{Items: items, Shown: min(len(items), maxAffectedShown)}
	if err != nil {
		m.Err = err.Error()
	}
	idx := map[string]int{}
	for _, a := range items {
		if i, ok := idx[a.Profile]; ok {
			m.ByProfile[i].N++
		} else {
			idx[a.Profile] = len(m.ByProfile)
			m.ByProfile = append(m.ByProfile, views.ProfileCount{Profile: a.Profile, N: 1})
		}
	}
	render(w, r, http.StatusOK, views.AffectedList(m))
}

func pct(rate float64) string {
	return strconv.FormatFloat(math.Round(rate*100*1e4)/1e4, 'f', -1, 64) + "%"
}

func rulesText(rules []string) string {
	if len(rules) == 0 {
		return "none"
	}
	return strings.Join(rules, ", ")
}

func (s *Server) pendingFailures(w http.ResponseWriter, r *http.Request) {
	d := views.Diff{ApplyURL: "/failures/apply", Form: "failures-form"}
	ref, mixed, _ := s.refState()
	f, err := parseFailures(r)
	if err != nil || ref == nil {
		render(w, r, http.StatusOK, views.PendingBar(d))
		return
	}
	for _, ft := range views.FaultTypes {
		if from, to := ref.Faults[ft.Type], f.rates[ft.Type]; from != to {
			d.Items = append(d.Items, views.DiffItem{What: ft.Type, From: pct(from), To: pct(to)})
		}
	}
	if !slices.Equal(ref.Profiles.Rules, f.rules) {
		d.Items = append(d.Items, views.DiffItem{What: "Profiles", From: rulesText(ref.Profiles.Rules), To: rulesText(f.rules)})
	}
	if ref.Profiles.Seed != f.seed {
		d.Items = append(d.Items, views.DiffItem{What: "Seed", From: strconv.FormatInt(ref.Profiles.Seed, 10), To: strconv.FormatInt(f.seed, 10)})
	}
	if mixed && len(d.Items) > 0 {
		d.Note = "Instances differ today; Apply sets them all to this."
	}
	render(w, r, http.StatusOK, views.PendingBar(d))
}

func (s *Server) savePreset(w http.ResponseWriter, r *http.Request) {
	f, err := parseFailures(r)
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	p := presets.Preset{Name: r.Form.Get("preset_name"), Faults: f.rates, Profiles: presets.Profiles{Seed: f.seed, Rules: f.rules}}
	if err := s.opt.Presets.Save(p); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "presets-changed")
	render(w, r, http.StatusOK, views.Flashes([]views.Flash{{Kind: "ok", Text: "Preset " + strings.TrimSpace(p.Name) + " saved."}}))
}

func (s *Server) applyPreset(w http.ResponseWriter, r *http.Request) {
	p, ok := s.opt.Presets.Get(r.FormValue("name"))
	if !ok {
		s.fail(w, r, "No preset "+r.FormValue("name")+".")
		return
	}
	rs := s.opt.Fleet.ApplyFailures(r.Context(), p.Faults, p.Profiles.Seed, p.Profiles.Rules)
	s.opt.Fleet.Poll(r.Context())
	s.flash(w, r, true, resultsFlash("Preset "+p.Name, rs))
}

func (s *Server) deletePreset(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	if err := s.opt.Presets.Delete(name); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	w.Header().Set("HX-Trigger", "presets-changed")
	render(w, r, http.StatusOK, views.Flashes([]views.Flash{{Kind: "ok", Text: "Preset " + name + " deleted."}}))
}
