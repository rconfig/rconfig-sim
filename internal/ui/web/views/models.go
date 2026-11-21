// Package views holds the rcfg-sim-ui templ components and the models they
// render. Run `make ui-gen` after editing a .templ file; the generated
// *_templ.go files are committed.
package views

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/presets"
	ui "github.com/rcfg-sim/rcfg-sim/internal/ui/web/views/components"
)

// Page is what the layout needs on every page.
type Page struct {
	Title string
	Nav   string    // "fleet" or "failures"
	Up    int       // instances up
	Total int       // instances discovered
	User  string    // the logged-in user, for the header
	Bench BenchPill // the active testbench; zero State hides the pill
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
	// Checklist is the first-login checklist; nil once dismissed.
	Checklist *ChecklistModel
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
	case fleet.StatusPortClash:
		return "Port clash"
	case fleet.StatusBadEnv:
		return "Bad env file"
	case fleet.StatusUnused:
		return "Not used by this testbench"
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
	case fleet.StatusAdminOff:
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

// Health is the header pill's instances up and total. Instances the
// testbench does not use are in neither: stopped is what they should be.
func Health(vs []fleet.View) (up, total int) {
	for _, v := range vs {
		switch v.Status {
		case fleet.StatusUnused:
		case fleet.StatusUp:
			up++
			total++
		default:
			total++
		}
	}
	return up, total
}

func healthUp(vs []fleet.View) int    { up, _ := Health(vs); return up }
func healthTotal(vs []fleet.View) int { _, t := Health(vs); return t }

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

// BuildModel is the Build screen (10.4).
type BuildModel struct {
	NotAdopted string // why the install is not on the data/ layout; "" if it is
	Err        string // a layout problem that blocks builds
	Live       *BuildInfo
	Previous   *BuildInfo // what rollback would restore; nil if none
	Instances  int
	PerIP      int
	MinCount   int
	MaxCount   int
	Count      int // the form's starting total
	Seed       int64
	Groups     []CountGroup
	Unknowns   bool // the live build's seed or distribution is unknown (adopted)
	Job        *JobModel
	DataDir    string
}

// BuildInfo describes one build.
type BuildInfo struct {
	Name, Created, Distribution, Seed string
	Count                             int
	Adopted                           bool
}

// CountGroup is one family's count rows on the Build screen.
type CountGroup struct {
	Name string // the family's name
	Rows []ui.Count
}

// EstimateModel is the Build screen's estimate panel.
type EstimateModel struct {
	Err      string
	Steps    []string // manual steps for a count that needs more IPs
	Total    int      // devices, the sum of the count rows
	Bars     []Bar
	Disk     string
	RAM      string
	Free     string
	Generate string
	Restart  string
	Fits     bool
	Ready    bool // nothing blocks Run
	Unknowns bool
}

// Bar is one model's segment of the stacked distribution bar.
type Bar struct {
	Model string
	X, W  string // percent of the bar
	Class string
	Count int
	Pct   int
}

// JobModel is a running or finished build or rollback.
type JobModel struct {
	ID            string
	Kind          string
	Lines         []string
	Finished      bool
	Err           string
	OfferRollback bool
}

// ModelFills colours the stacked distribution bar, by model order. Listed
// in full so Tailwind generates each class.
var ModelFills = []string{
	"fill-accent-200", "fill-accent-300", "fill-accent-400", "fill-accent-500", "fill-accent-600",
	"fill-accent-700", "fill-accent-800", "fill-accent-900", "fill-accent-950",
	"fill-st-active", "fill-st-ok", "fill-st-hang", "fill-st-auth",
}

// BenchPill is the active testbench, for the header pill on every page.
type BenchPill struct {
	State    string // idle, starting, running, stopping or failed
	Name     string
	Elapsed  string
	StopIn   string // time left before auto-stop; "" for none
	Modified bool
	Error    string
	RunID    string
}

// TestbenchRow is one line of the Testbenches list.
type TestbenchRow struct {
	Slug        string
	Name        string
	Description string
	Version     int
	Devices     int
	Duration    string
	Shipped     bool
	LastRun     string // when it last ran, "" if never
	LastRunID   string
	Fit         []string // why it cannot start here; empty if it can
	Active      bool     // the active testbench
	Families    []string // family names, for the badges
	Problem     string   // why it needs attention; it cannot start until fixed
}

// TestbenchesModel is the Testbenches screen (10.5).
type TestbenchesModel struct {
	Show   string // filter: "" (all), "shipped" or "yours"
	Groups []BenchGroup
	Err    string // some files could not be read
	Job    *JobModel
	Active BenchPill
}

// OverrideRow is one instance override in the editor. Empty fields are
// not overridden.
type OverrideRow struct {
	IP          string
	Faults      string // type=rate,...
	ProfileSeed string
	Rules       string // comma-separated
	DelayMin    string
	DelayMax    string
	MaxSessions string
	Active      string
}

// TestbenchEditModel is the testbench editor.
type TestbenchEditModel struct {
	Slug         string // "" for a new testbench
	Shipped      bool
	Name         string
	Description  string
	Version      int
	Devices      int
	Seed         int64
	Distribution string
	Options      []KV // generator options the UI does not expose, carried through
	Faults       []FaultRow
	ProfileSeed  int64
	Rules        string // one per line
	DelayMin     int64
	DelayMax     int64
	MaxSessions  int64
	Active       string // "all" or a count
	Overrides    []OverrideRow
	Instances    []string // instance IPs on this host, for override rows
	Duration     string
	Presets      []string
	Fit          []string
}

// KV is a key and value.
type KV struct{ K, V string }

// RunRow is one line of the Runs list.
type RunRow struct {
	ID       string
	Name     string
	Version  int
	Started  string
	Ended    string
	Duration string
	Outcome  string
	Sessions string
	Faults   string
	Bytes    string
	Tags     []string
}

// RunsModel is the Runs screen (10.6).
type RunsModel struct {
	Rows []RunRow
	Err  string
	Tag  string // the tag filter, if any
}

// RunTotalsRow is a line of the totals table: a name and its values.
type RunTotalsRow struct {
	Name   string
	Values []string
}

// RunModel is one run's detail.
type RunModel struct {
	Row        RunRow
	Reason     string
	Snapshot   string // the testbench as started, JSON
	Events     []RunEvent
	Columns    []string // totals table headings
	Totals     []RunTotalsRow
	Instances  []RunTotalsRow
	Build      string
	Notes      string
	Tags       string // comma-separated, for the form
	Incomplete bool   // a ui_gap: totals miss traffic
}

// RunEvent is a timeline entry.
type RunEvent struct{ At, Kind, Text string }

// CompareModel is two runs side by side.
type CompareModel struct {
	A, B     RunRow
	Notes    [][3]string
	Totals   [][3]string
	Settings [][3]string
}

func outcomeClass(o string) string {
	switch o {
	case "running":
		return "text-st-active"
	case "stopped", "auto-stopped":
		return "text-st-ok"
	case "failed":
		return "text-st-error"
	}
	return "text-muted"
}

func pillClass(state string) string {
	switch state {
	case "running":
		return "border-st-ok text-st-ok"
	case "starting", "stopping":
		return "border-st-active text-st-active"
	case "failed":
		return "border-st-error text-st-error"
	}
	return "border-line text-muted"
}

// benchEmpty is the empty Testbenches list's text for the filter show.
func benchEmpty(show string) string {
	switch show {
	case "shipped":
		return "This generator supports none of the shipped testbenches."
	case "yours":
		return "Testbenches you create, duplicate or import appear here."
	}
	return "Create one, or import a testbench file below."
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinDelay(lo, hi string) string {
	if lo == "" && hi == "" {
		return ""
	}
	return lo + "-" + hi
}

// DevicesModel is the Devices screen (10.7) for one scope: the live build,
// a testbench's build or a run's build.
type DevicesModel struct {
	Scope      string // shown in the heading: "live build 20261101-0751", "testbench Smoke A", ...
	ScopeQuery string // testbench=... or run=..., carried on every link; "" for the live build
	Build      string
	Missing    string // the build is not kept: why, and the rows are empty
	BuildSlug  string // with Missing: offer to build this testbench's build
	NotLive    bool   // a kept build the instances are not serving
	Summary    DeviceSummary
	Filters    DeviceFilters
	Rows       []DeviceRow
	Page       int
	Pages      int
	Matched    int    // rows the filters match, all pages
	Query      string // the current query string, for sort and page links
	Open       string // a hostname whose drawer opens on load
	Instances  []string
	Models     []string
	Families   []string // family names from the catalogue, for the filter
}

// DeviceSummary is the counts bar; each is a link that filters.
type DeviceSummary struct {
	Total, Active, Unknown int
	Inactive               []KVCount
	Families               []KVCount
	Models                 []KVCount
}

// KVCount is a label, its count and the link that filters by it.
type KVCount struct {
	Label string
	Count int
	Href  string
}

// DeviceFilters are the filter form's current values.
type DeviceFilters struct {
	Status, Reason, Family, Instance, Model, Profile, Outcome, Q string
}

// DeviceRow is one table row.
type DeviceRow struct {
	Hostname, IP, Instance, Model, Family, Username, Password, Profile, Status, Reason, LastOutcome, LastSeen string
	Port                                                                                                      int
	Href                                                                                                      string // the drawer
}

// DeviceDrawerModel is one device's drawer.
type DeviceDrawerModel struct {
	Row            DeviceRow
	EnablePassword string
	Template       string
	ProfilePinned  bool
	SSH            string // the ssh command line
	TestCommand    string
	TestURL        string // "" when Test login is not possible (build not live)
	Events         []string
	Err            string
}

// LoginTestModel is a Test login result.
type LoginTestModel struct {
	OK        bool
	Stage     string
	LoginTime string
	Total     string
	Command   string
	Output    []string
	Err       string
	Hint      string
}

// SortLink is a column heading link.
type SortLink struct {
	Label  string
	Href   string
	Active bool
	Desc   bool
}

// ChecklistModel is the first-login checklist on the Fleet screen.
type ChecklistModel struct {
	Items []ChecklistItem
	Done  int
}

// ChecklistItem is one line of it.
type ChecklistItem struct {
	Text string
	Href string
	Done bool
}

func statusClass2(s string) string {
	switch s {
	case "active":
		return "text-st-ok"
	case "inactive":
		return "text-muted"
	}
	return "text-st-auth"
}

func pageHref(query string, page int) string {
	v, _ := url.ParseQuery(query)
	v.Set("page", strconv.Itoa(page))
	return "/devices?" + v.Encode()
}

func scopeSuffix(scope string) string {
	if scope == "" {
		return ""
	}
	return "?" + scope
}

func scopeAmp(scope string) string {
	if scope == "" {
		return ""
	}
	return "&" + scope
}

func scopeFields(scope string) []KV {
	v, _ := url.ParseQuery(scope)
	var out []KV
	for k, vs := range v {
		for _, s := range vs {
			out = append(out, KV{K: k, V: s})
		}
	}
	return out
}

func urlQuery(s string) string { return url.QueryEscape(s) }

func statusPill(s fleet.Status) string {
	switch s {
	case fleet.StatusUp:
		return "ok"
	case fleet.StatusDown, fleet.StatusUnused:
		return "neutral"
	case fleet.StatusAdminOff:
		return "warn"
	}
	return "error"
}

func rollingKind(r RollingView) string {
	switch {
	case r.Err != "":
		return "error"
	case r.Running:
		return "warn"
	}
	return "ok"
}

func rollingTitle(r RollingView) string {
	switch {
	case r.Running:
		return "Rolling restart in progress"
	case r.Err != "":
		return "Rolling restart stopped"
	}
	return "Rolling restart finished"
}

func errLine(e string) []string {
	if e == "" {
		return nil
	}
	return []string{e}
}

func ruleKindOptions() []ui.Option {
	out := make([]ui.Option, len(RuleKinds))
	for i, k := range RuleKinds {
		out[i] = ui.Option{Value: k, Label: k}
	}
	return out
}

// presetVals is the hx-vals JSON naming a preset.
func presetVals(name string) string {
	b, _ := json.Marshal(map[string]string{"name": name})
	return string(b)
}

func tailOutcomeOptions() []ui.Option {
	out := []ui.Option{{Value: "", Label: "Any outcome"}}
	for _, o := range []string{"ok", "error", "auth_fail", "disconnect"} {
		out = append(out, ui.Option{Value: o, Label: o})
	}
	return out
}

func tailFaultOptions() []ui.Option {
	out := []ui.Option{{Value: "", Label: "Any fault"}}
	for _, f := range FaultTypes {
		out = append(out, ui.Option{Value: f.Type, Label: f.Type})
	}
	return out
}

// anyOptions is a filter's choices, led by "any".
func anyOptions(opts []string) []ui.Option {
	out := []ui.Option{{Value: "", Label: "any"}}
	for _, o := range opts {
		out = append(out, ui.Option{Value: o, Label: o})
	}
	return out
}

func hintLines(h string) []string {
	if h == "" {
		return nil
	}
	return []string{h}
}

// BenchGroup is one kind's testbenches on the list (10.5).
type BenchGroup struct {
	Title string
	Help  string
	Rows  []TestbenchRow
}

// FlowModel is the guided testbench flow (10.5): one form, four steps.
type FlowModel struct {
	Step    int // 0 kind, 1 devices, 2 behaviour, 3 review
	Steps   []ui.Step
	Draft   string // the draft as JSON, carried in a hidden field
	Editing bool   // editing a saved testbench (kind fixed)
	Shipped bool   // editing a shipped one: Save as only
	Slug    string
	Version int
	Error   string // why the last Next or Save was refused

	// Step 1.
	Kind           string
	KindLabel      string
	KindChoices    []ui.Choice
	ShippedChoices []ui.Choice
	From           string

	// Step 2.
	ShowIP, ShowOptical   bool
	IPRows, OpticalRows   []ui.Count
	IPTotal, OpticalTotal int
	Total                 int
	IPPresets             []ui.Option
	SSHAuth               string
	SSHChoices            []ui.Option
	Options               []OptionField
	DualHome              bool // the dual-homed RNE option is offered: show its diagram
	Seed                  int64
	Fit                   []string
	EstimateLine          string
	CountHint             string
	// A scenario testbench's devices step: the package choice cards.
	ShowScenario    bool
	Scenario        string
	ScenarioChoices []ui.Choice

	// Step 3: the behaviour fields (TestbenchEditModel's).
	Behaviour TestbenchEditModel

	// Step 4.
	Name, Description string
	Summary           string
	Mix               []MixSegment
	Happen            []string
	Problems          []string
}

// OptionField is one catalogue model option on the devices step.
type OptionField struct {
	Name, Label, Help, Flag string
	Value, Min, Max         int
	Models                  string // which models it applies to
	Default                 int
}

// MixSegment is one part of the review step's stacked device-mix bar.
type MixSegment struct {
	Label string
	N     int
	Pct   float64
	X     string
	W     string
	Class string
}

func benchPillKind(state string) string {
	switch state {
	case "running":
		return "ok"
	case "starting", "stopping":
		return "active"
	case "failed":
		return "error"
	}
	return "neutral"
}

// benchRunHref is where the pill's name goes: the running run, else the
// Testbenches list.
func benchRunHref(b BenchPill) string {
	if b.RunID != "" {
		return "/runs/" + b.RunID
	}
	return "/testbenches"
}

// Thousands formats n with thousands separators (9.6: numbers line up).
func Thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// seedChanged is 1 when the seed is not the generator default (42), for
// the Advanced fold's count.
func seedChanged(seed int64) int {
	if seed != 42 {
		return 1
	}
	return 0
}

func saveStartURL(m FlowModel) string {
	if !m.Shipped && m.Editing {
		return "/testbenches/save?start=1"
	}
	return "/testbenches/save?as=1&start=1"
}

func presetOptions(names []string) []ui.Option {
	out := make([]ui.Option, len(names))
	for i, n := range names {
		out[i] = ui.Option{Value: n, Label: n}
	}
	return out
}

func instanceOptions(cur string, instances []string) []ui.Option {
	out := []ui.Option{{Value: "", Label: "remove"}}
	if cur != "" && !contains(instances, cur) {
		out = append(out, ui.Option{Value: cur, Label: cur + " (not on this host)"})
	}
	for _, ip := range instances {
		out = append(out, ui.Option{Value: ip, Label: ip})
	}
	return out
}

// ScenariosModel is the scenario library (10.8).
type ScenariosModel struct {
	Err       string
	Dir       string // --scenarios-dir
	Cards     []ui.Choice
	Selected  string
	Detail    *ScenarioDetail
	Attention []ScenarioProblem
}

// ScenarioProblem is a package that cannot be used, and why.
type ScenarioProblem struct {
	Name, Folder string
	Shipped      bool
	Problems     []string
}

// ScenarioDetail is one scenario's detail view.
type ScenarioDetail struct {
	ID, Name, Description string
	Version               int
	Folder                string
	Shipped               bool
	Badges                []ui.BadgeData
	Diagram               Diagram
	Devices               []ScenarioDevice
	Behaviour             []string
	Auth, SSHAuth         string
	Fit                   string
	Fits                  bool
	Problems              []string
}

// ScenarioDevice is one row of the detail's device table.
type ScenarioDevice struct {
	Hostname, Vendor, Driver, Model, Tier string
	Commands                              bool
	Slot                                  string // ip:port on this host; "" when it does not fit
}

// Diagram is a topology drawn as SVG: nodes in tiers, links as lines.
type Diagram struct {
	W, H  int
	Nodes []DiagramNode
	Links []DiagramLink
}

// DiagramNode is one device box. X, Y is its top left; CX, CY its centre.
type DiagramNode struct {
	ID            string
	X, Y, W, H    int
	CX, CY        int
	Detail, Label string
}

// DiagramLink is one link between two node centres.
type DiagramLink struct {
	X1, Y1, X2, Y2 int
	Label          string
}

// scenarioProblemTitle names a package that cannot be used.
func scenarioProblemTitle(a ScenarioProblem) string {
	where := "your package in folder " + a.Folder
	if a.Shipped {
		where = "shipped package " + a.Folder
	}
	return a.Name + " (" + where + ") cannot be used:"
}

// viewBox is a diagram's SVG viewBox.
func (d Diagram) viewBox() string { return fmt.Sprintf("0 0 %d %d", d.W, d.H) }
