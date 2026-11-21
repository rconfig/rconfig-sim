package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/testbench"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
	ui "github.com/rcfg-sim/rcfg-sim/internal/ui/web/views/components"
)

// Testbench kinds on the list and in the guided flow (10.5). A kind is
// which families have devices; "shipped" is the flow's "start from a
// shipped testbench" and "scenario" is Phase 9.
const (
	kindIP       = "ip"
	kindOptical  = "optical"
	kindMix      = "mix"
	kindScenario = "scenario"
	kindShipped  = "shipped"
)

var kindTitles = map[string]string{
	kindIP: "Enterprise IP", kindOptical: "Optical transport", kindMix: "Service provider mix", kindScenario: "Topology scenarios",
}

// familyNames are the catalogue's family names for a testbench's badges.
func familyNames(cat *catalogue.Catalogue, ids []string) []string {
	var out []string
	for _, id := range ids {
		if f, ok := cat.Family(id); ok {
			out = append(out, shortFamily(f))
		}
	}
	return out
}

// shortFamily is a family's badge text: its name up to any bracket
// ("Optical transport (TL1)" is "Optical").
func shortFamily(f catalogue.Family) string {
	name, _, _ := strings.Cut(f.Name, " (")
	if f.ID == "ip" {
		return "IP"
	}
	first, _, _ := strings.Cut(name, " ")
	return first
}

func (s *Server) testbenchesPage(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	m := views.TestbenchesModel{Active: s.benchPill()}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		m.Err = err.Error()
		render(w, r, http.StatusOK, views.TestbenchesPage(s.page("Testbenches", "testbenches"), m))
		return
	}
	all, err := s.opt.Testbenches.List()
	if err != nil {
		m.Err = err.Error()
	}
	last := map[string]views.RunRow{}
	if rs, _ := s.opt.Runs.List(); rs != nil {
		for _, run := range slices.Backward(rs) { // oldest first, so the newest wins
			last[run.TestbenchSlug] = runRow(run)
		}
	}
	st := s.opt.Jobs.Status()
	active := s.opt.Bench.Active()
	// Shipped and your own testbenches together, grouped by kind; one
	// that fails validation goes in Needs attention with the reason.
	m.Show = r.URL.Query().Get("show")
	if m.Show != "shipped" && m.Show != "yours" {
		m.Show = ""
	}
	groups := map[string]*views.BenchGroup{}
	order := []string{kindIP, kindOptical, kindMix, kindScenario, "attention"}
	for _, k := range order {
		groups[k] = &views.BenchGroup{Title: kindTitles[k]}
	}
	groups["attention"].Title, groups["attention"].Help = "Needs attention", "These no longer validate against this host's generator. Edit and save one to fix it; it cannot start until then."
	groups[kindIP].Help = "Cisco IOS fleets, from TEST-SCENARIOS.md scenarios 3 to 9."
	groups[kindOptical].Help = "TL1 optical networks: gateways and their remote nodes."
	groups[kindMix].Help = "IP and optical together, as a service provider runs both."
	groups[kindScenario].Help = "Curated topologies with matching neighbours across vendors."
	for _, tb := range all {
		if (m.Show == "shipped" && !tb.Shipped) || (m.Show == "yours" && tb.Shipped) {
			continue
		}
		row := views.TestbenchRow{Slug: tb.Slug, Name: tb.Name, Description: tb.Description, Version: tb.Version,
			Devices: tb.Devices.Total(), Duration: tb.Duration, Shipped: tb.Shipped, Problem: tb.Problem,
			Active:   active.State != testbench.Idle && active.Slug == tb.Slug,
			Families: familyNames(cat, tb.Devices.FamilyIDs())}
		if tb.Devices.Source == testbench.SourceScenario {
			row.Families = []string{"Topology"}
		}
		if tb.Problem == "" {
			row.Fit = testbench.Fit(tb, st.Layout, st.Err)
		}
		if lr, ok := last[tb.Slug]; ok {
			row.LastRun, row.LastRunID = lr.Started, lr.ID
		}
		g := tb.Devices.Kind()
		if groups[g] == nil || tb.Problem != "" {
			g = "attention"
		}
		groups[g].Rows = append(groups[g].Rows, row)
	}
	for _, k := range order {
		if len(groups[k].Rows) > 0 {
			m.Groups = append(m.Groups, *groups[k])
		}
	}
	if j := s.opt.Jobs.Current(); j != nil && (strings.HasPrefix(j.Kind, "testbench") || strings.HasPrefix(j.Kind, "build (kept")) && !j.Finished() {
		m.Job = jobModel(j)
	}
	p := s.page("Testbenches", "testbenches")
	p.Scripts = []string{"/static/vendor/htmx-ext-sse-2.2.4.min.js"}
	render(w, r, http.StatusOK, views.TestbenchesPage(p, m))
}

// behaviourModel fills the behaviour step from a testbench.
func (s *Server) behaviourModel(tb testbench.Testbench) views.TestbenchEditModel {
	m := views.TestbenchEditModel{ProfileSeed: tb.Runtime.Profiles.Seed, Rules: strings.Join(tb.Runtime.Profiles.Rules, "\n"),
		DelayMin: tb.Runtime.DelayMS.Min, DelayMax: tb.Runtime.DelayMS.Max, MaxSessions: tb.Runtime.MaxSessions,
		Active: tb.Runtime.ActiveDevices.String(), Duration: tb.Duration}
	m.Faults, _ = formModel(tb.Runtime.Faults, nil)
	for _, ip := range slices.Sorted(maps.Keys(tb.Overrides)) {
		o := tb.Overrides[ip]
		row := views.OverrideRow{IP: ip}
		if o.Faults != nil {
			set, _ := fault.NewSetFromRates(o.Faults)
			row.Faults = set.String()
		}
		if o.Profiles != nil {
			row.ProfileSeed, row.Rules = i64s(o.Profiles.Seed), strings.Join(o.Profiles.Rules, ",")
		}
		if o.DelayMS != nil {
			row.DelayMin, row.DelayMax = i64s(o.DelayMS.Min), i64s(o.DelayMS.Max)
		}
		if o.MaxSessions != nil {
			row.MaxSessions = i64s(*o.MaxSessions)
		}
		if o.ActiveDevices != nil {
			row.Active = o.ActiveDevices.String()
		}
		m.Overrides = append(m.Overrides, row)
	}
	m.Instances = s.opt.Jobs.Status().Layout.Instances
	if ps, _ := s.opt.Presets.List(); ps != nil {
		for _, p := range ps {
			m.Presets = append(m.Presets, p.Name)
		}
	}
	return m
}

func i64s(n int64) string { return strconv.FormatInt(n, 10) }

// parseBehaviour reads the behaviour step's fields into tb.
func parseBehaviour(f url.Values, tb *testbench.Testbench) error {
	var errs []error
	num := func(name string) int64 {
		v, err := strconv.ParseInt(strings.TrimSpace(f.Get(name)), 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s must be a whole number", strings.ReplaceAll(name, "_", " ")))
		}
		return v
	}
	tb.Duration = strings.TrimSpace(f.Get("duration"))
	rt := testbench.Runtime{Faults: map[string]float64{}, MaxSessions: num("max_sessions"),
		DelayMS:  testbench.DelayMS{Min: num("delay_min"), Max: num("delay_max")},
		Profiles: testbench.Profiles{Seed: num("profile_seed"), Rules: []string{}}}
	for _, ft := range views.FaultTypes {
		raw := strings.TrimSpace(f.Get("f_" + ft.Type))
		if raw == "" {
			continue
		}
		pct, err := strconv.ParseFloat(raw, 64)
		if err != nil || pct < 0 || pct > 100 || math.IsNaN(pct) {
			errs = append(errs, fmt.Errorf("%s must be 0 to 100%%", ft.Label))
			continue
		}
		if pct > 0 {
			rt.Faults[ft.Type] = math.Round(pct/100*1e6) / 1e6
		}
	}
	for _, line := range strings.Split(f.Get("rules"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rt.Profiles.Rules = append(rt.Profiles.Rules, line)
		}
	}
	var err error
	if rt.ActiveDevices, err = testbench.ParseActive(f.Get("active")); err != nil {
		errs = append(errs, err)
	}
	tb.Runtime = rt

	tb.Overrides = nil
	col := func(name string, i int) string {
		if vs := f[name]; i < len(vs) {
			return strings.TrimSpace(vs[i])
		}
		return ""
	}
	for i, ip := range f["ov_ip"] {
		if ip = strings.TrimSpace(ip); ip == "" {
			continue // "remove"
		}
		if _, err := netip.ParseAddr(ip); err != nil {
			errs = append(errs, fmt.Errorf("override: %q is not an IP", ip))
			continue
		}
		if tb.Overrides == nil {
			tb.Overrides = map[string]testbench.Override{}
		}
		if _, dup := tb.Overrides[ip]; dup {
			errs = append(errs, fmt.Errorf("override: %s is listed twice", ip))
			continue
		}
		o, err := parseOverride(col("ov_faults", i), col("ov_profile_seed", i), col("ov_rules", i), col("ov_delay", i), col("ov_max_sessions", i), col("ov_active", i))
		if err != nil {
			errs = append(errs, fmt.Errorf("override %s: %w", ip, err))
			continue
		}
		tb.Overrides[ip] = o
	}
	return errors.Join(errs...)
}

// draft is the guided flow's state, carried between steps in the form.
type draft struct {
	Kind    string          `json:"kind"`
	From    string          `json:"from,omitempty"` // shipped testbench to start from
	Slug    string          `json:"slug,omitempty"` // editing this saved testbench
	Shipped bool            `json:"shipped,omitempty"`
	TB      json.RawMessage `json:"tb"`
}

// newTestbench is a fresh testbench of a kind, with the defaults the
// guided flow starts from.
func newTestbench(kind string, cat *catalogue.Catalogue, perInstance int, instances int) testbench.Testbench {
	tb := testbench.Testbench{
		Devices: testbench.Devices{Source: testbench.SourceGenerated, Generated: &testbench.Generated{Seed: jobs.DefaultSeed, Families: map[string]*testbench.FamilyDevices{}}},
		Runtime: testbench.Runtime{Faults: map[string]float64{}, Profiles: testbench.Profiles{Seed: 42, Rules: []string{}},
			DelayMS: testbench.DelayMS{Min: 50, Max: 500}, MaxSessions: 5000, ActiveDevices: testbench.Active{All: true}},
	}
	total := max(perInstance*instances, 1)
	fams := tb.Devices.Generated.Families
	if kind == kindIP || kind == kindMix {
		ipTotal := total
		if kind == kindMix {
			ipTotal = total * 9 / 10
		}
		w, _ := cat.ParseDistribution(jobs.DefaultDistribution)
		fams["ip"] = &testbench.FamilyDevices{Models: cat.Counts(ipTotal, w)}
	}
	if kind == kindOptical || kind == kindMix {
		optTotal := total
		if kind == kindMix {
			optTotal = total - total*9/10
		}
		models := cat.ModelsIn("optical")
		f := &testbench.FamilyDevices{Models: map[string]int{}, SSHAuth: testbench.SSHAuthDriver}
		if len(models) > 0 {
			f.Models[models[0].ID] = optTotal
		}
		fams["optical"] = f
	}
	return tb
}

// kindOf is the flow kind of a testbench's devices.
func kindOf(tb testbench.Testbench) string {
	if k := tb.Devices.Kind(); k != "" {
		return k
	}
	return kindIP
}

// loadDraft reads the draft from the form; a missing or broken one starts
// a new IP testbench.
func (s *Server) loadDraft(f url.Values, cat *catalogue.Catalogue) (draft, testbench.Testbench) {
	var d draft
	var tb testbench.Testbench
	if json.Unmarshal([]byte(f.Get("draft")), &d) == nil && len(d.TB) > 0 && json.Unmarshal(d.TB, &tb) == nil &&
		(tb.Devices.Generated != nil || tb.Devices.Scenario != nil) {
		_ = s.opt.Testbenches.Check(&tb) // the review step shows any problem
		return d, tb
	}
	l := s.opt.Jobs.Status().Layout
	d.Kind = kindIP
	return d, newTestbench(kindIP, cat, l.DevicesPerIP, l.IPCount)
}

func saveDraft(d draft, tb testbench.Testbench) string {
	d.TB = testbench.Encode(tb)
	b, _ := json.Marshal(d)
	return string(b)
}

// parseStep applies the posted step's visible fields to the draft.
func (s *Server) parseStep(at int, f url.Values, d *draft, tb *testbench.Testbench, cat *catalogue.Catalogue) error {
	switch at {
	case 0:
		if d.Slug != "" {
			return nil // editing: the kind is fixed
		}
		kind := f.Get("kind")
		if kind == "" {
			kind = kindIP
		}
		switch kind {
		case kindIP, kindOptical, kindMix:
			if kind != d.Kind {
				l := s.opt.Jobs.Status().Layout
				*tb = newTestbench(kind, cat, l.DevicesPerIP, l.IPCount)
			}
			d.From = ""
		case kindShipped:
			d.From = f.Get("from")
		case kindScenario:
			if kind != d.Kind {
				sc, ok := s.firstScenario()
				if !ok {
					return errors.New("no scenario package can be used on this host; see the Scenarios screen")
				}
				*tb = scenarioTestbench(sc)
			}
			d.From = ""
		default:
			return fmt.Errorf("unknown kind %q", kind)
		}
		d.Kind = kind
	case 1:
		if tb.Devices.Source == testbench.SourceScenario {
			return s.parseScenario(f.Get("scenario"), d, tb)
		}
		g := tb.Devices.Generated
		seed, err := strconv.ParseInt(strings.TrimSpace(f.Get("seed")), 10, 64)
		if err != nil {
			return errors.New("seed must be a whole number")
		}
		g.Seed = seed
		for _, fid := range []string{"ip", "optical"} {
			fam := g.Families[fid]
			if fam == nil {
				continue
			}
			fam.Models = map[string]int{}
			for _, m := range cat.ModelsIn(fid) {
				raw := strings.TrimSpace(f.Get("count_" + m.ID))
				if raw == "" {
					continue
				}
				n, err := strconv.Atoi(raw)
				if err != nil || n < 0 {
					return fmt.Errorf("%s: the count must be a whole number, 0 or more", m.ID)
				}
				if n > 0 {
					fam.Models[m.ID] = n
				}
			}
			if fid == "optical" {
				fam.SSHAuth = f.Get("ssh_auth")
				fam.Options = map[string]int{}
				for _, o := range cat.FamilyOptions(fid) {
					n, err := strconv.Atoi(strings.TrimSpace(f.Get("opt_" + o.Name)))
					if err != nil {
						return fmt.Errorf("%s must be a whole number", o.Name)
					}
					if n != o.Default {
						fam.Options[o.Name] = n
					}
				}
				if len(fam.Options) == 0 {
					fam.Options = nil
				}
			}
		}
		if p := f.Get("preset"); p != "" && g.Families["ip"] != nil {
			total := familySum(g.Families["ip"].Models)
			if total == 0 {
				total = 1000
			}
			dist := ipPresets[p]
			if dist == "" {
				return fmt.Errorf("unknown preset %q", p)
			}
			w, err := cat.ParseDistribution(dist)
			if err != nil {
				return err
			}
			g.Families["ip"].Models = cat.Counts(total, w)
		}
	case 2:
		return parseBehaviour(f, tb)
	case 3:
		tb.Name, tb.Description = strings.TrimSpace(f.Get("name")), strings.TrimSpace(f.Get("description"))
	}
	return nil
}

// scenarioTestbench is a new testbench from a scenario package, with the
// package's suggested behaviour.
func scenarioTestbench(sc scenarios.Scenario) testbench.Testbench {
	tb := testbench.Testbench{
		Devices: testbench.Devices{Source: testbench.SourceScenario, Scenario: &testbench.ScenarioRef{ID: sc.ID, Version: sc.Version,
			Name: sc.Name, Devices: len(sc.Devices), SSHAuth: sc.SSHAuthMode()}},
		Runtime: testbench.Runtime{Faults: map[string]float64{}, Profiles: testbench.Profiles{Seed: 42, Rules: []string{}},
			DelayMS: testbench.DelayMS{Min: 50, Max: 500}, MaxSessions: 100, ActiveDevices: testbench.Active{All: true}},
	}
	if r := sc.SuggestedRuntime; r != nil {
		if r.Faults != nil {
			tb.Runtime.Faults = maps.Clone(r.Faults)
		}
		if r.DelayMS != nil {
			tb.Runtime.DelayMS = testbench.DelayMS{Min: r.DelayMS.Min, Max: r.DelayMS.Max}
		}
	}
	return tb
}

// scenarioList is the library, or nothing when there is none.
func (s *Server) scenarioList() []scenarios.Scenario {
	lib := s.opt.Testbenches.Scenarios()
	if lib == nil {
		return nil
	}
	all, err := lib.List()
	if err != nil {
		s.opt.Logger.Warn("scenario library", "err", err)
	}
	return all
}

// firstScenario is the first package that can be used, for a new
// scenario testbench.
func (s *Server) firstScenario() (scenarios.Scenario, bool) {
	for _, sc := range s.scenarioList() {
		if sc.Problem() == "" {
			return sc, true
		}
	}
	return scenarios.Scenario{}, false
}

// parseScenario applies the devices step's scenario choice. A new
// testbench takes the package's suggested behaviour when the choice
// changes; an edited one keeps its own.
func (s *Server) parseScenario(id string, d *draft, tb *testbench.Testbench) error {
	lib := s.opt.Testbenches.Scenarios()
	if lib == nil {
		return errors.New("no scenario library")
	}
	sc, err := lib.Get(id)
	if err != nil {
		return errors.New("pick a scenario")
	}
	if p := sc.Problem(); p != "" {
		return fmt.Errorf("%s cannot be used: %s", sc.Name, p)
	}
	if r := tb.Devices.Scenario; r != nil && r.ID == sc.ID && r.Version == sc.Version {
		return nil
	}
	if d.Slug == "" {
		name, desc := tb.Name, tb.Description
		*tb = scenarioTestbench(sc)
		tb.Name, tb.Description = name, desc
		return nil
	}
	tb.Devices.Scenario = &testbench.ScenarioRef{ID: sc.ID, Version: sc.Version, Name: sc.Name, Devices: len(sc.Devices), SSHAuth: sc.SSHAuthMode()}
	return nil
}

// ipPresets are the IP panel's quick mixes, as distributions of the
// panel's current total.
var ipPresets = map[string]string{
	"typical": "sm:40,md:40,lg:15,xl:5",
	"heavy":   "sm:10,md:30,lg:20,xl:40",
}

func familySum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// flowModel builds the step's view.
func (s *Server) flowModel(step int, d draft, tb testbench.Testbench, cat *catalogue.Catalogue) views.FlowModel {
	m := views.FlowModel{Step: step, Draft: saveDraft(d, tb), Editing: d.Slug != "", Shipped: d.Shipped, Slug: d.Slug,
		Version: tb.Version, Kind: d.Kind, From: d.From, Name: tb.Name, Description: tb.Description}
	if d.Shipped {
		m.Editing = true
	}
	labels := []string{"What are you testing", "Devices", "Behaviour", "Review and start"}
	for i, l := range labels {
		st := ui.Step{Label: l}
		if i < step {
			st.Goto = strconv.Itoa(i) // completed steps post the form back
		}
		m.Steps = append(m.Steps, st)
	}
	m.KindLabel = kindTitles[kindOf(tb)]
	switch step {
	case 0:
		why := ""
		if _, ok := s.firstScenario(); !ok {
			why = "no usable scenario"
		}
		m.KindChoices = kindChoices(cat, why)
		if d.Kind == kindShipped {
			for _, sh := range s.shippedTestbenches() {
				m.ShippedChoices = append(m.ShippedChoices, ui.Choice{Value: sh.Slug, Title: sh.Name, Description: sh.Description,
					Badges:   append(benchBadges(cat, sh), ui.BadgeData{Kind: ui.BadgeMuted, Text: views.Thousands(sh.Devices.Total()) + " devices"}),
					Disabled: attention(sh)})
			}
		}
	case 1:
		if tb.Devices.Source == testbench.SourceScenario {
			s.scenarioStep(&m, tb)
			break
		}
		s.devicesStep(&m, tb, cat)
	case 2:
		m.Behaviour = s.behaviourModel(tb)
	case 3:
		s.reviewStep(&m, d, tb, cat)
	}
	return m
}

// shippedTestbenches are the shipped testbenches with their scenario
// details filled in.
func (s *Server) shippedTestbenches() []testbench.Testbench {
	all, _ := s.opt.Testbenches.List()
	var out []testbench.Testbench
	for _, tb := range all {
		if tb.Shipped {
			out = append(out, tb)
		}
	}
	return out
}

// attention is a choice card's disabled reason for a testbench that needs
// attention, else "".
func attention(tb testbench.Testbench) string {
	if tb.Problem != "" {
		return "needs attention"
	}
	return ""
}

// benchBadges are a testbench's family badges; a scenario's is Topology.
func benchBadges(cat *catalogue.Catalogue, tb testbench.Testbench) []ui.BadgeData {
	if tb.Devices.Source == testbench.SourceScenario {
		return []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "Topology"}}
	}
	return familyBadges(cat, tb.Devices.FamilyIDs())
}

func familyBadges(cat *catalogue.Catalogue, ids []string) []ui.BadgeData {
	var out []ui.BadgeData
	for _, n := range familyNames(cat, ids) {
		out = append(out, ui.BadgeData{Kind: ui.BadgeFamily, Text: n})
	}
	return out
}

// kindChoices are step 1's cards, described from the catalogue's families.
// noScenario, when set, is why the Topology card cannot be picked.
func kindChoices(cat *catalogue.Catalogue, noScenario string) []ui.Choice {
	ip, _ := cat.Family("ip")
	opt, _ := cat.Family("optical")
	return []ui.Choice{
		{Value: kindIP, Title: "Enterprise IP fleet", Description: ip.Description, Icon: "IP", Badges: []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "IP"}, {Kind: ui.BadgeProtocol, Text: "CLI"}}},
		{Value: kindOptical, Title: "Optical transport (TL1)", Description: opt.Description, Icon: "TL1", Badges: []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "Optical"}, {Kind: ui.BadgeProtocol, Text: "TL1"}}},
		{Value: kindMix, Title: "Service provider mix", Description: "IP and optical devices in one fleet, each with its own panel, as a service provider runs both.", Icon: "SP",
			Badges: []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "IP"}, {Kind: ui.BadgeFamily, Text: "Optical"}}},
		{Value: kindScenario, Title: "Topology scenario", Description: "A curated package with matching neighbours across vendors, such as mesh-campus.", Icon: "◇",
			Badges: []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "Topology"}}, Disabled: noScenario},
		{Value: kindShipped, Title: "Start from a shipped testbench", Description: "Copy one of the built-in testbenches and change what you need.", Icon: "⧉"},
	}
}

// scenarioStep is the devices step of a scenario testbench: one choice
// card per package (section 10.8); one that cannot be used is shown with
// why.
func (s *Server) scenarioStep(m *views.FlowModel, tb testbench.Testbench) {
	m.ShowScenario = true
	st := s.opt.Jobs.Status()
	if r := tb.Devices.Scenario; r != nil {
		m.Scenario = r.ID
	}
	for _, sc := range s.scenarioList() {
		m.ScenarioChoices = append(m.ScenarioChoices, scenarioChoice(sc, st.Layout, st.Err, "cannot be used (see Scenarios)"))
	}
	m.Total = tb.Devices.Total()
	m.Fit = testbench.Fit(tb, st.Layout, st.Err)
	m.EstimateLine = fmt.Sprintf("%d devices from the package; nothing is generated.", m.Total)
}

// scenarioChoice is a scenario's choice card: purpose, device count,
// drivers, whether it fits this host, and broken, the badge of one that
// cannot be used.
func scenarioChoice(sc scenarios.Scenario, l jobs.Layout, layoutErr error, broken string) ui.Choice {
	c := ui.Choice{Value: sc.ID, Title: sc.Name, Description: sc.Description, Icon: "◇",
		Badges: []ui.BadgeData{{Kind: ui.BadgeFamily, Text: "Topology"}, {Kind: ui.BadgeMuted, Text: fmt.Sprintf("%d devices", len(sc.Devices))}}}
	for _, d := range sc.DeviceDrivers() {
		c.Badges = append(c.Badges, ui.BadgeData{Kind: ui.BadgeProtocol, Text: d})
	}
	switch {
	case sc.Problem() != "":
		c.Disabled = broken
	case layoutErr != nil:
	case scenarios.Fit(sc, l) != "":
		c.Badges = append(c.Badges, ui.BadgeData{Kind: ui.BadgeError, Text: "doesn't fit this host"})
	default:
		c.Badges = append(c.Badges, ui.BadgeData{Kind: ui.BadgeOK, Text: "fits"})
	}
	return c
}

func (s *Server) devicesStep(m *views.FlowModel, tb testbench.Testbench, cat *catalogue.Catalogue) {
	g := tb.Devices.Generated
	m.Seed = g.Seed
	ip, opt := g.Families["ip"], g.Families["optical"]
	m.ShowIP, m.ShowOptical = ip != nil, opt != nil
	m.Total = tb.Devices.Total()
	rows := func(fid string, fam *testbench.FamilyDevices) ([]ui.Count, int) {
		var out []ui.Count
		total := familySum(fam.Models)
		for _, mod := range cat.ModelsIn(fid) {
			n := fam.Models[mod.ID]
			share := 0.0
			if total > 0 {
				share = 100 * float64(n) / float64(total)
			}
			d, _ := cat.Driver(mod.Driver)
			out = append(out, ui.Count{Name: "count_" + mod.ID, Model: mod.ID, Description: mod.Vendor + ". " + mod.Description,
				Protocol: strings.ToUpper(d.Protocol), AvgSize: jobs.HumanBytes(mod.AvgBytes), Value: n, Share: share})
		}
		return out, total
	}
	if ip != nil {
		m.IPRows, m.IPTotal = rows("ip", ip)
		m.IPPresets = []ui.Option{{Value: "typical", Label: "Typical enterprise 40/40/15/5", Help: "sm 40%, md 40%, lg 15%, xl 5% of the IP subtotal"},
			{Value: "heavy", Label: "Heavy configs", Help: "sm 10%, md 30%, lg 20%, xl 40% of the IP subtotal"}}
	}
	if opt != nil {
		m.OpticalRows, m.OpticalTotal = rows("optical", opt)
		m.SSHAuth = opt.SSHAuth
		if m.SSHAuth == "" {
			m.SSHAuth = testbench.SSHAuthPassword
		}
		m.SSHChoices = []ui.Option{
			{Value: testbench.SSHAuthDriver, Label: "Login inside TL1 only, like real 6500s", Help: "No SSH password for TL1 devices; ACT-USER is the only login. IP devices still ask for their SSH password."},
			{Value: testbench.SSHAuthPassword, Label: "SSH password first, then TL1 login", Help: "Every device asks for an SSH password, then TL1 devices want ACT-USER as well."},
		}
		if ip == nil {
			m.SSHChoices = append(m.SSHChoices, ui.Option{Value: testbench.SSHAuthNone, Label: "No SSH password at all", Help: "Any SSH client gets in without a password; ACT-USER is the only login. Optical-only fleets."})
		}
		for _, o := range cat.FamilyOptions("optical") {
			v := o.Default
			if x, ok := opt.Options[o.Name]; ok {
				v = x
			}
			label := strings.ReplaceAll(o.Name, "_", " ")
			if o.Name == "rne_dual_home_pct" {
				label, m.DualHome = "Dual-homed remote NEs", true
			}
			m.Options = append(m.Options, views.OptionField{Name: o.Name, Label: label, Help: o.Help, Flag: o.Flag,
				Value: v, Min: o.Min, Max: o.Max, Default: o.Default, Models: strings.Join(cat.OptionModels(o.Name), ", ")})
		}
	}
	st := s.opt.Jobs.Status()
	m.Fit = testbench.Fit(tb, st.Layout, st.Err)
	e := jobs.EstimateBuild(tb.Devices.Counts(), cat)
	m.EstimateLine = fmt.Sprintf("%s devices: about %s on disk and %s to generate.", views.Thousands(m.Total),
		jobs.HumanBytes(e.DiskBytes), e.Generate.Round(time.Second))
}

// reviewStep is step 4: the summary sentence, the mix, what will happen.
func (s *Server) reviewStep(m *views.FlowModel, d draft, tb testbench.Testbench, cat *catalogue.Catalogue) {
	if tb.Devices.Source == testbench.SourceScenario {
		s.reviewScenario(m, tb, cat)
		return
	}
	counts := tb.Devices.Counts()
	total := tb.Devices.Total()
	x := 0.0
	for i, id := range cat.ModelIDs() {
		n := counts[id]
		if n == 0 {
			continue
		}
		pct := 100 * float64(n) / float64(max(total, 1))
		m.Mix = append(m.Mix, views.MixSegment{Label: id, N: n, Pct: pct, X: strconv.FormatFloat(x, 'f', 2, 64),
			W: strconv.FormatFloat(pct, 'f', 2, 64), Class: views.ModelFills[i%len(views.ModelFills)]})
		x += pct
	}
	st := s.opt.Jobs.Status()
	check := tb
	if check.Name == "" {
		check.Name = "draft"
	}
	if err := testbench.Validate(check, cat); err != nil {
		for _, e := range strings.Split(err.Error(), "\n") {
			m.Problems = append(m.Problems, e)
		}
	}
	m.Problems = append(m.Problems, testbench.Fit(tb, st.Layout, st.Err)...)
	if st.Err != nil {
		m.Summary = fmt.Sprintf("Saves a testbench of %s devices. This host has no usable instance layout, so it cannot start here.", views.Thousands(total))
		return
	}
	req := tb.Devices.Request(cat)
	_, _, live, kept := s.opt.Jobs.KeptBuild(req)
	e := jobs.EstimateBuild(req.Counts, cat)
	build := ""
	switch {
	case live:
		build = "uses the live build (no generation)"
		m.Happen = append(m.Happen, "The live build already has these devices: nothing is generated.")
	case kept:
		build = "switches to a kept build (no generation)"
		m.Happen = append(m.Happen, "A kept build has these devices: Start switches to it, with no generation, and restarts every instance.")
	default:
		build = fmt.Sprintf("builds them in about %s", e.Generate.Round(time.Second))
		m.Happen = append(m.Happen, fmt.Sprintf("Generates a new build: about %s on disk, %s to generate, then restarts every instance.",
			jobs.HumanBytes(e.DiskBytes), e.Generate.Round(time.Second)))
	}
	pending, _ := s.opt.Fleet.SSHAuthPending(st.Layout.Instances, tb.Devices.SSHAuth())
	if len(pending) > 0 {
		m.Happen = append(m.Happen, fmt.Sprintf("Sets the SSH login to %q on %d instances; that needs a restart, which Start does.", tb.Devices.SSHAuth(), len(pending)))
	}
	if st.Live != nil && st.Live.Scenario != "" {
		m.Happen = append(m.Happen, "The live build is the "+st.Live.Scenario+" scenario: Start puts back the command files and passwords each instance had before it (COMMANDS_ROOT, PASSWORD, ENABLE_PASSWORD); that needs a restart, which Start does.")
	}
	if unused := s.unusedNow(); len(unused) > 0 {
		m.Happen = append(m.Happen, fmt.Sprintf("Starts the %d instances the scenario did not use: %s.", len(unused), strings.Join(unused, ", ")))
	}
	m.Happen = append(m.Happen, "Applies the behaviour settings to every instance and its env file, then waits until each one matches.")
	if tb.Duration != "" {
		m.Happen = append(m.Happen, "Stops itself after "+tb.Duration+".")
	}
	m.Summary = fmt.Sprintf("%s devices on %d instances: %s, then starts.", views.Thousands(total), st.Layout.IPCount, build)
	if len(pending) > 0 && build != "" && (live) {
		m.Summary = fmt.Sprintf("%s devices on %d instances: %s, restarts for the SSH login change, then starts.", views.Thousands(total), st.Layout.IPCount, build)
	}
}

// reviewScenario is the review step of a scenario testbench: the device
// mix by driver, the build it installs or reuses, the instances it leaves
// unused, and the restarts its env keys need.
func (s *Server) reviewScenario(m *views.FlowModel, tb testbench.Testbench, cat *catalogue.Catalogue) {
	check := tb
	if check.Name == "" {
		check.Name = "draft"
	}
	if err := testbench.Validate(check, cat); err != nil {
		m.Problems = append(m.Problems, strings.Split(err.Error(), "\n")...)
	}
	if err := s.opt.Testbenches.Check(&check); err != nil {
		m.Problems = append(m.Problems, err.Error())
	}
	st := s.opt.Jobs.Status()
	m.Problems = append(m.Problems, testbench.Fit(tb, st.Layout, st.Err)...)
	sc, err := s.opt.Testbenches.Scenario(tb)
	if err != nil {
		m.Summary = "Saves a scenario testbench whose package is not in the library."
		return
	}
	byDriver := map[string]int{}
	for _, dv := range sc.Devices {
		byDriver[dv.Driver]++
	}
	x := 0.0
	for i, drv := range slices.Sorted(maps.Keys(byDriver)) {
		pct := 100 * float64(byDriver[drv]) / float64(len(sc.Devices))
		m.Mix = append(m.Mix, views.MixSegment{Label: drv, N: byDriver[drv], Pct: pct, X: strconv.FormatFloat(x, 'f', 2, 64),
			W: strconv.FormatFloat(pct, 'f', 2, 64), Class: views.ModelFills[i%len(views.ModelFills)]})
		x += pct
	}
	if st.Err != nil {
		m.Summary = fmt.Sprintf("Saves a testbench of the %s scenario. This host has no usable instance layout, so it cannot start here.", sc.Name)
		return
	}
	l := st.Layout
	used := scenarios.Instances(sc, l)
	build := "installs the package as a build"
	if hash, err := scenarios.Hash(sc, l); err == nil && st.Live != nil && st.Live.SettingsHash == hash {
		build = "uses the live build"
		m.Happen = append(m.Happen, "The live build is this scenario already: nothing is installed.")
	} else {
		m.Happen = append(m.Happen, fmt.Sprintf("Installs the package as a build: its %d devices go on the first instance first, ports from %d, hostnames kept.", len(sc.Devices), l.PortStart))
	}
	if used < l.IPCount && used > 0 {
		m.Happen = append(m.Happen, fmt.Sprintf("Uses %d of %d instances. The other %d are not used by this testbench and are stopped: %s.",
			used, l.IPCount, l.IPCount-used, strings.Join(l.Instances[used:], ", ")))
	}
	keys := testbench.ScenarioKeys(sc, l)
	if pending, _ := s.opt.Fleet.KeysPending(l.Instances[:min(used, len(l.Instances))], keys); len(pending) > 0 {
		what := "turns on the package's command files"
		if len(sc.Commands) == 0 {
			what = "turns command files off"
		}
		if sc.Auth.Mode == scenarios.AuthAnyPassword {
			what += " and accepts any password"
		}
		m.Happen = append(m.Happen, fmt.Sprintf("Sets the SSH login to %q, %s on %d instances; that needs a restart, which Start does.", sc.SSHAuthMode(), what, len(pending)))
	}
	m.Happen = append(m.Happen, "Applies the behaviour settings to the instances it uses and their env files, then waits until each one matches.")
	if tb.Duration != "" {
		m.Happen = append(m.Happen, "Stops itself after "+tb.Duration+".")
	}
	m.Summary = fmt.Sprintf("%d devices of the %s scenario on %d of %d instances: %s, then starts.", len(sc.Devices), sc.Name, used, l.IPCount, build)
}

// unusedNow lists the instances the live build does not use.
func (s *Server) unusedNow() []string {
	var out []string
	for _, v := range s.opt.Fleet.Views() {
		if v.Status == fleet.StatusUnused {
			out = append(out, v.Name)
		}
	}
	return out
}

// testbenchNew opens the guided flow for a new testbench.
func (s *Server) testbenchNew(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.failPage(w, r, err)
		return
	}
	// "Create testbench from this scenario" opens the devices step with the
	// package chosen.
	if id := r.URL.Query().Get("scenario"); id != "" {
		if lib := s.opt.Testbenches.Scenarios(); lib != nil {
			if sc, err := lib.Get(id); err == nil && sc.Problem() == "" {
				tb := scenarioTestbench(sc)
				render(w, r, http.StatusOK, views.FlowPage(s.page("New testbench", "testbenches"), s.flowModel(1, draft{Kind: kindScenario}, tb, cat)))
				return
			}
		}
	}
	l := s.opt.Jobs.Status().Layout
	tb := newTestbench(kindIP, cat, l.DevicesPerIP, l.IPCount)
	render(w, r, http.StatusOK, views.FlowPage(s.page("New testbench", "testbenches"), s.flowModel(0, draft{Kind: kindIP}, tb, cat)))
}

// testbenchEdit opens the flow on a saved (or shipped) testbench, kind
// fixed, at the devices step.
func (s *Server) testbenchEdit(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.failPage(w, r, err)
		return
	}
	tb, err := s.opt.Testbenches.Get(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d := draft{Kind: kindOf(tb), Slug: tb.Slug, Shipped: tb.Shipped}
	render(w, r, http.StatusOK, views.FlowPage(s.page(tb.Name, "testbenches"), s.flowModel(1, d, tb, cat)))
}

// failPage answers a page request the UI cannot serve without the
// catalogue.
func (s *Server) failPage(w http.ResponseWriter, r *http.Request, err error) {
	http.Error(w, err.Error(), http.StatusServiceUnavailable)
}

// testbenchFlow moves the guided flow between steps: it applies the posted
// step's fields to the draft, then shows the requested step, or the same
// one with the reason it cannot move on.
func (s *Server) testbenchFlow(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	d, tb := s.loadDraft(r.Form, cat)
	at, _ := strconv.Atoi(r.Form.Get("at"))
	next, err := strconv.Atoi(r.Form.Get("goto"))
	if err != nil {
		next = at // a preset button: apply it and stay
	}
	next = min(max(next, 0), 3)
	if perr := s.parseStep(at, r.Form, &d, &tb, cat); perr != nil {
		m := s.flowModel(at, d, tb, cat)
		m.Error = perr.Error()
		render(w, r, http.StatusOK, views.Flow(m))
		return
	}
	// Leaving step 1 with "start from shipped": copy the chosen one.
	if at == 0 && next > 0 && d.Kind == kindShipped {
		var sh testbench.Testbench
		found := false
		for _, x := range s.shippedTestbenches() {
			if x.Slug == d.From && x.Problem == "" {
				sh, found = x, true
			}
		}
		if !found {
			m := s.flowModel(0, d, tb, cat)
			m.Error = "Pick a shipped testbench to start from."
			render(w, r, http.StatusOK, views.Flow(m))
			return
		}
		tb = sh
		tb.Name, tb.Version, tb.Slug, tb.Shipped = s.opt.Testbenches.DuplicateName(sh.Name), 0, "", false
		d.Kind, d.From = kindOf(tb), ""
	}
	// Moving forward from the devices step needs devices.
	if at == 1 && next > 1 && tb.Devices.Total() < 1 {
		m := s.flowModel(1, d, tb, cat)
		m.Error = "Add at least one device."
		render(w, r, http.StatusOK, views.Flow(m))
		return
	}
	render(w, r, http.StatusOK, views.Flow(s.flowModel(next, d, tb, cat)))
}

// testbenchSave saves the flow's draft: Save replaces the testbench being
// edited (its version goes up); ?as=1 saves a new one; ?start=1 then starts
// it.
func (s *Server) testbenchSave(w http.ResponseWriter, r *http.Request) {
	if s.opt.Bench == nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	cat, err := s.opt.Catalogue.Get()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	d, tb := s.loadDraft(r.Form, cat)
	at, _ := strconv.Atoi(r.Form.Get("at"))
	if err := s.parseStep(at, r.Form, &d, &tb, cat); err != nil {
		s.fail(w, r, "Not saved: "+err.Error())
		return
	}
	var saved testbench.Testbench
	if r.URL.Query().Get("as") != "" || d.Slug == "" || d.Shipped {
		saved, err = s.opt.Testbenches.Create(tb)
	} else {
		saved, err = s.opt.Testbenches.Save(d.Slug, tb)
	}
	if err != nil {
		s.fail(w, r, "Not saved: "+err.Error())
		return
	}
	s.opt.Logger.Info("testbench saved", "testbench", saved.Name, "version", saved.Version)
	if r.URL.Query().Get("start") != "" {
		if s.buildBusy() {
			s.fail(w, r, "Saved, but a rolling restart is running; start it from the list when that has finished.")
			return
		}
		if _, err := s.opt.Bench.Start(saved.Slug, false); err != nil {
			if errors.Is(err, testbench.ErrActive) {
				render(w, r, http.StatusOK, views.ReplaceConfirm(s.opt.Bench.Active().Name, saved.Slug, saved.Name))
				return
			}
			s.fail(w, r, "Saved, but not started: "+err.Error())
			return
		}
		w.Header().Set("HX-Redirect", "/testbenches")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("HX-Redirect", "/testbenches/"+saved.Slug)
	w.WriteHeader(http.StatusOK)
}

// testbenchPreset loads a preset into the behaviour step's faults and
// profiles.
func (s *Server) testbenchPreset(w http.ResponseWriter, r *http.Request) {
	p, ok := s.opt.Presets.Get(r.URL.Query().Get("preset"))
	if !ok {
		s.fail(w, r, "No such preset.")
		return
	}
	m := views.TestbenchEditModel{ProfileSeed: p.Profiles.Seed, Rules: strings.Join(p.Profiles.Rules, "\n")}
	m.Faults, _ = formModel(p.Faults, nil)
	render(w, r, http.StatusOK, views.BenchFailures(m))
}

func (s *Server) overrideRow(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, views.OverrideRowView(views.OverrideRow{}, s.opt.Jobs.Status().Layout.Instances))
}
