package web

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/scenarios"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/web/views"
	ui "github.com/rcfg-sim/rcfg-sim/internal/ui/web/views/components"
)

// scenariosPage is the scenario library (section 10.8): a choice card per
// package, the chosen one's detail below, and the packages that cannot be
// used with their reasons. /scenarios/{id} and /scenarios?id= open one.
func (s *Server) scenariosPage(w http.ResponseWriter, r *http.Request) {
	var lib *scenarios.Library
	if s.opt.Testbenches != nil {
		lib = s.opt.Testbenches.Scenarios()
	}
	m := views.ScenariosModel{}
	if lib == nil {
		m.Err = "The scenario library is not configured."
		render(w, r, http.StatusOK, views.ScenariosPage(s.page("Scenarios", "scenarios"), m))
		return
	}
	m.Dir = lib.Dir()
	all, err := lib.List()
	if err != nil {
		m.Err = err.Error()
	}
	st := s.opt.Jobs.Status()
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	found := false
	for _, sc := range all {
		m.Cards = append(m.Cards, scenarioChoice(sc, st.Layout, st.Err, "cannot be used"))
		if p := sc.Problem(); p != "" {
			m.Attention = append(m.Attention, views.ScenarioProblem{Name: sc.Name, Folder: sc.Folder, Shipped: sc.Shipped, Problems: sc.Problems})
		}
		if sc.ID == id && !found {
			found = true
			m.Selected = id
			m.Detail = scenarioDetail(sc, st.Layout, st.Err)
		}
	}
	status := http.StatusOK
	if id != "" && !found {
		status, m.Err = http.StatusNotFound, fmt.Sprintf("No scenario %q.", id)
	}
	title := "Scenarios"
	if m.Detail != nil {
		title = m.Detail.Name
	}
	render(w, r, status, views.ScenariosPage(s.page(title, "scenarios"), m))
}

// scenarioDetail is the detail view: description, diagram, devices,
// suggested behaviour, and where it would run on this host.
func scenarioDetail(sc scenarios.Scenario, l jobs.Layout, layoutErr error) *views.ScenarioDetail {
	d := &views.ScenarioDetail{ID: sc.ID, Name: sc.Name, Description: sc.Description, Version: sc.Version, Folder: sc.Folder,
		Shipped: sc.Shipped, Problems: sc.Problems, Diagram: diagram(sc), SSHAuth: sc.SSHAuthMode()}
	d.Auth = "Any password is accepted (the instances run with an empty --password and --enable-password)."
	if sc.Auth.Mode == scenarios.AuthManifest {
		d.Auth = "The manifest's password and enable password."
	}
	var slots []scenarios.Slot
	switch {
	case layoutErr != nil:
		d.Fit = "This host has no usable instance layout: " + layoutErr.Error()
	case scenarios.Fit(sc, l) != "":
		d.Fit = scenarios.Fit(sc, l)
	default:
		slots, _ = scenarios.Place(sc, l)
		used := scenarios.Instances(sc, l)
		d.Fits = true
		d.Fit = fmt.Sprintf("Fits: %d devices on %d of this host's %d instances; the other %d are not used.", len(sc.Devices), used, l.IPCount, l.IPCount-used)
	}
	for i, dv := range sc.Devices {
		row := views.ScenarioDevice{Hostname: dv.Hostname, Vendor: dv.Vendor, Driver: dv.Driver, Model: dv.Model,
			Commands: slices.Contains(sc.Commands, dv.Hostname), Tier: "-"}
		if t := sc.Tier(dv.Hostname); t >= 0 {
			row.Tier = strconv.Itoa(t)
		}
		if i < len(slots) {
			row.Slot = fmt.Sprintf("%s:%d", slots[i].IP, slots[i].Port)
		}
		d.Devices = append(d.Devices, row)
	}
	if r := sc.SuggestedRuntime; r != nil {
		if r.DelayMS != nil {
			d.Behaviour = append(d.Behaviour, fmt.Sprintf("Response delay %d to %d ms", r.DelayMS.Min, r.DelayMS.Max))
		}
		var faults []string
		for _, k := range slices.Sorted(maps.Keys(r.Faults)) {
			if r.Faults[k] > 0 {
				faults = append(faults, fmt.Sprintf("%s %s%%", k, strconv.FormatFloat(r.Faults[k]*100, 'f', -1, 64)))
			}
		}
		if len(faults) == 0 {
			d.Behaviour = append(d.Behaviour, "No faults")
		} else {
			d.Behaviour = append(d.Behaviour, "Faults: "+strings.Join(faults, ", "))
		}
	}
	for _, drv := range sc.DeviceDrivers() {
		d.Badges = append(d.Badges, ui.BadgeData{Kind: ui.BadgeProtocol, Text: drv})
	}
	return d
}

// Diagram geometry, in SVG user units.
const (
	nodeW, nodeH = 132, 40
	colW, rowH   = 172, 104
	pad          = 24
)

// diagram lays the topology out in tiers, top first, nodes spread evenly
// across each tier, links as straight lines between node centres.
func diagram(sc scenarios.Scenario) views.Diagram {
	tiers := map[int][]string{}
	for _, n := range sc.Topology.Nodes {
		tiers[n.Tier] = append(tiers[n.Tier], n.ID)
	}
	order := slices.Sorted(maps.Keys(tiers))
	widest := 1
	for _, t := range order {
		widest = max(widest, len(tiers[t]))
	}
	g := views.Diagram{W: widest*colW + 2*pad, H: len(order)*rowH + 2*pad - (rowH - nodeH)}
	at := map[string][2]int{}
	vendor := map[string]scenarios.Device{}
	for _, d := range sc.Devices {
		vendor[d.Hostname] = d
	}
	for row, t := range order {
		ids := tiers[t]
		for i, id := range ids {
			cx := pad + (g.W-2*pad)*(2*i+1)/(2*len(ids))
			cy := pad + row*rowH + nodeH/2
			at[id] = [2]int{cx, cy}
			dv := vendor[id]
			g.Nodes = append(g.Nodes, views.DiagramNode{ID: id, X: cx - nodeW/2, Y: cy - nodeH/2, W: nodeW, H: nodeH, CX: cx, CY: cy,
				Detail: dv.Vendor + " · " + dv.Driver, Label: fmt.Sprintf("%s, %s %s, tier %d", id, dv.Vendor, dv.Driver, t)})
		}
	}
	for _, l := range sc.Topology.Links {
		a, okA := at[l[0]]
		b, okB := at[l[1]]
		if okA && okB {
			g.Links = append(g.Links, views.DiagramLink{X1: a[0], Y1: a[1], X2: b[0], Y2: b[1], Label: l[0] + " to " + l[1]})
		}
	}
	return g
}
