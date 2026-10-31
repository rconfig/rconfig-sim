// Package views holds the rcfg-sim-ui templ components and the models they
// render. Run `make ui-gen` after editing a .templ file; the generated
// *_templ.go files are committed.
package views

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
)

// Page is what the layout needs on every page.
type Page struct {
	Title string
	Nav   string // "fleet" or "failures"
	Up    int    // instances up
	Total int    // instances discovered
	// Scripts and Styles are extra same-origin assets for this page, loaded
	// after the shared ones (Styles before app.css, so app.css wins).
	Scripts []string
	Styles  []string
}

// Flash is the result of an action, shown above the page content.
type Flash struct {
	Kind  string // "ok", "warn" or "error"
	Text  string
	Lines []string
}

// FleetModel is the Fleet screen.
type FleetModel struct {
	Views    []fleet.View
	Ref      *admin.State // the first up instance: the fleet-wide forms start from it
	Devices  int          // devices across up instances (the active-devices maximum)
	Active   int          // active devices across up instances
	CapTotal int64        // session caps across up instances
	Rolling  RollingView
	Poll     string // htmx polling interval, e.g. "2s"
}

// RollingView is the state of the last rolling restart.
type RollingView struct {
	Running bool
	Steps   []string
	Err     string
	Done    bool
}

// FailuresModel is the Failures screen's form.
type FailuresModel struct {
	Faults  []FaultRow
	Seed    int64
	Rules   []RuleRow
	Devices int // devices across up instances, for the estimates
	Presets []presets.Preset
	Loaded  string // the preset loaded into the form, if any
	Mixed   bool   // up instances disagree on faults or profiles
	NoneUp  bool
}

// FaultRow is one fault type's row.
type FaultRow struct {
	Type  string
	Label string
	Help  string
	Pct   float64 // percent, 0..100
}

// RuleRow is one device profile rule's row.
type RuleRow struct {
	Kind  string // hang, latency, refuse, disconnect
	Param string // "3600s", "200-800ms", or empty
	Pct   string // percent of the fleet
}

// Diff is the pending-changes bar: what Apply would change.
type Diff struct {
	Items    []DiffItem
	ApplyURL string // where the bar's Apply button posts
	Form     string // id of the form it submits
	Note     string
}

// DiffItem is one changed setting.
type DiffItem struct {
	What, From, To string
}

// AffectedModel is the "who is affected" panel.
type AffectedModel struct {
	Items     []fleet.Affected
	Shown     int
	Err       string
	ByProfile []ProfileCount
}

// ProfileCount is how many devices get one profile.
type ProfileCount struct {
	Profile string
	N       int
}

// FaultTypes are the Failures rows, in the fault package's order.
var FaultTypes = []FaultRow{
	{Type: "auth_fail", Label: "Auth failure", Help: "SSH password rejected; one roll per session"},
	{Type: "disconnect_mid", Label: "Disconnect mid-stream", Help: "TCP reset part-way through a config; one roll per config"},
	{Type: "slow_response", Label: "Slow response", Help: "Command delay x10-50; one roll per command"},
	{Type: "malformed", Label: "Malformed output", Help: "Config truncated, junk-injected or bit-flipped; one roll per config"},
}

// RuleKinds are the profile kinds the rule editor offers.
var RuleKinds = []string{"hang", "latency", "refuse", "disconnect"}

func itoa(n int) string       { return strconv.Itoa(n) }
func i64(n int64) string      { return strconv.FormatInt(n, 10) }
func pctStr(p float64) string { return strconv.FormatFloat(p, 'f', -1, 64) }
func estimate(devices int, pct float64) string {
	return strconv.Itoa(int(math.Round(float64(devices) * pct / 100)))
}

// rangeMax is a fault slider's maximum: 10% unless the value is already
// higher (9.1: 0 to 10% by default, extendable to 100%).
func rangeMax(p float64) string {
	if p > 10 {
		return "100"
	}
	return "10"
}

func anyOver10(rows []FaultRow) bool {
	for _, r := range rows {
		if r.Pct > 10 {
			return true
		}
	}
	return false
}

func rangeIDs() string {
	ids := make([]string, len(FaultTypes))
	for i, f := range FaultTypes {
		ids[i] = "range-" + f.Type
	}
	return strings.Join(ids, ",")
}

// HumanRate formats bytes per second.
func HumanRate(bps float64) string {
	units := []string{"B/s", "kB/s", "MB/s", "GB/s"}
	i := 0
	for bps >= 1000 && i < len(units)-1 {
		bps /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", bps, units[i])
	}
	return fmt.Sprintf("%.1f %s", bps, units[i])
}

func lastRate(h []float64) string {
	if len(h) == 0 {
		return "–"
	}
	return HumanRate(h[len(h)-1])
}

// sparkPoints maps a bytes/s history onto a 120x28 viewBox.
func sparkPoints(h []float64) string {
	if len(h) < 2 {
		return ""
	}
	hi := 0.0
	for _, v := range h {
		hi = math.Max(hi, v)
	}
	if hi == 0 {
		hi = 1
	}
	var b strings.Builder
	step := 120.0 / float64(len(h)-1)
	for i, v := range h {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", float64(i)*step, 27-v/hi*26)
	}
	return b.String()
}

// sessionPct is active sessions as a percentage of the cap, for the bar.
func sessionPct(st *admin.State) string {
	if st == nil || st.MaxSessions <= 0 {
		return "0"
	}
	p := float64(st.ActiveSessions) / float64(st.MaxSessions) * 100
	return strconv.FormatFloat(math.Min(p, 100), 'f', 1, 64)
}

func statusLabel(s fleet.Status) string {
	switch s {
	case fleet.StatusUp:
		return "Up"
	case fleet.StatusDown:
		return "Down"
	case fleet.StatusAdminOff:
		return "Admin API off"
	case fleet.StatusAuth:
		return "Token rejected"
	case fleet.StatusPortClash:
		return "Port clash"
	case fleet.StatusBadEnv:
		return "Bad env file"
	default:
		return "Error"
	}
}

// statusClass is the pill colour. Every pill also carries its label in
// text: colour is never the only signal (9.1).
func statusClass(s fleet.Status) string {
	switch s {
	case fleet.StatusUp:
		return "text-st-ok border-st-ok"
	case fleet.StatusAdminOff, fleet.StatusAuth:
		return "text-st-auth border-st-auth"
	default:
		return "text-st-error border-st-error"
	}
}

func portRange(v fleet.View) string {
	return fmt.Sprintf("%d–%d", v.PortStart, v.PortStart+v.PortCount-1)
}

func flashClass(kind string) string {
	switch kind {
	case "ok":
		return "border-st-ok"
	case "warn":
		return "border-st-auth"
	default:
		return "border-st-error"
	}
}

func upViews(vs []fleet.View) []fleet.View {
	var out []fleet.View
	for _, v := range vs {
		if v.Status == fleet.StatusUp {
			out = append(out, v)
		}
	}
	return out
}

func joinComma(s []string) string { return strings.Join(s, ", ") }

// SummaryModel is the Activity summary strip.
type SummaryModel struct {
	Instances int // in the frame
	Reachable int // whose devices were read
	Devices   int
	Legend    []LegendItem
	Upstream  uint64 // events instances reported dropping since the page opened
}

// LegendItem is one state in the heatmap legend.
type LegendItem struct {
	Name  string
	Class string // swatch colour
	N     int
}

// DeviceModel is the heatmap side panel.
type DeviceModel struct {
	Instance   string
	Host       string
	IP         string
	Port       int
	State      string
	StateClass string
	LastSeen   string
	Profile    string
	Pinned     bool
	Events     []string // one line each, oldest first
	Err        string
}

// StateClass is the swatch colour for a heatmap state name; activity.js
// uses the same --ui-st-* variables.
func StateClass(name string) string {
	switch name {
	case "active":
		return "bg-st-active"
	case "ok":
		return "bg-st-ok"
	case "auth_fail":
		return "bg-st-auth"
	case "disconnect":
		return "bg-st-disconnect"
	case "error":
		return "bg-st-error"
	case "hang":
		return "bg-st-hang"
	case "inactive":
		return "bg-st-inactive"
	case "never":
		return "bg-st-never"
	default:
		return "bg-panel-2"
	}
}
