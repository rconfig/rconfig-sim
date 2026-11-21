package inventory

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// Statuses.
const (
	Active   = "active"
	Inactive = "inactive"
	Unknown  = "unknown"
)

// Reasons, from the table in Phase 7 item 2, plus three the table does not
// cover: an IP no instance serves, an admin API that answers with an
// error, and a kept build that is not the live one.
const (
	ReasonBeyondActive = "beyond active count"
	ReasonDown         = "instance down"
	ReasonStopped      = "testbench stopped"
	ReasonAdminOff     = "admin API off"
	ReasonBadEnv       = "bad env file"
	ReasonPortClash    = "port clash"
	ReasonAdminError   = "admin API error"
	ReasonNoInstance   = "no instance for this IP"
	ReasonNotLive      = "build not live"
	// ReasonUnused is an instance the live build has no devices on
	// (a scenario testbench that needs fewer instances, Phase 9).
	ReasonUnused = fleet.UnusedDetail
)

// Row is a device with its status, as the table and the exports show it.
type Row struct {
	Device
	Instance    string
	Family      string // the model's device family (catalogue id); "" when unknown
	Status      string
	Reason      string
	Profile     string // e.g. "hang:1h0m0s"; "" for a normal device
	ProfilePin  bool   // from the manifest's profile column
	LastOutcome string // device state name (6.1); "" when unknown
	LastSeen    time.Time
}

// Live is the fleet state a status join needs.
type Live struct {
	Views []fleet.View
	// Devices is each up instance's /admin/v1/devices answer; a missing
	// entry leaves last outcome and last seen empty.
	Devices map[string]*admin.Devices
	// BenchRunning: a testbench is running. With testbenches enabled and
	// none running, a stopped instance's devices are "testbench stopped".
	Benches      bool
	BenchRunning bool
	// LiveBuild is false when the index is a kept build that the instances
	// are not serving.
	LiveBuild bool
	// FamilyOf names a device's family from its model (size_bucket) and
	// driver (template), from the catalogue; nil leaves Family empty.
	FamilyOf func(model, template string) string
}

// stateNames are the 6.1 device state codes.
var stateNames = map[activity.State]string{
	activity.StateNever: "never", activity.StateOK: "ok", activity.StateAuthFail: "auth_fail",
	activity.StateDisconnect: "disconnect", activity.StateError: "error", activity.StateHang: "hang",
	activity.StateInactive: "inactive", activity.StateActive: "active",
}

type instInfo struct {
	view   fleet.View
	rules  []profile.Rule
	seed   int64
	states []byte
	seen   []byte
}

// Join gives every device in x its status, reason, profile and last
// activity (Phase 7 item 2). A device with a refuse or hang profile is
// active: it is reachable on purpose.
func Join(x *Index, live Live) []Row {
	byIP := map[string]*instInfo{}
	for _, v := range live.Views {
		in := &instInfo{view: v}
		switch {
		case v.State != nil:
			in.seed, in.rules = v.State.Profiles.Seed, parseRules(v.State.Profiles.Rules)
		case v.Desired != nil:
			in.seed, in.rules = v.Desired.ProfileSeed, parseRules(v.Desired.ProfileRules)
		}
		if d := live.Devices[v.Name]; d != nil {
			st, err1 := base64.StdEncoding.DecodeString(d.State)
			ls, err2 := base64.StdEncoding.DecodeString(d.LastSeen)
			if err1 == nil && err2 == nil && len(ls) == 4*len(st) {
				in.states, in.seen = st, ls
			}
		}
		byIP[v.ListenIP] = in
	}
	rows := make([]Row, len(x.Devices))
	for i, d := range x.Devices {
		r := Row{Device: d}
		if live.FamilyOf != nil {
			r.Family = live.FamilyOf(d.Model, d.Template)
		}
		in := byIP[d.IP]
		if in == nil || d.Port < in.view.PortStart || d.Port >= in.view.PortStart+in.view.PortCount {
			r.Status, r.Reason = Unknown, ReasonNoInstance
			rows[i] = r
			continue
		}
		r.Instance = in.view.Name
		r.Profile, r.ProfilePin = profileOf(d, in)
		if !live.LiveBuild {
			r.Status, r.Reason = Inactive, ReasonNotLive
			rows[i] = r
			continue
		}
		r.Status, r.Reason = statusOf(d, in.view, live)
		if idx := d.Port - in.view.PortStart; idx < len(in.states) {
			r.LastOutcome = stateNames[activity.State(in.states[idx])]
			if ts := binary.LittleEndian.Uint32(in.seen[4*idx:]); ts > 0 {
				r.LastSeen = time.Unix(int64(ts), 0).UTC()
			}
		}
		rows[i] = r
	}
	return rows
}

func statusOf(d Device, v fleet.View, live Live) (status, reason string) {
	switch v.Status {
	case fleet.StatusBadEnv:
		return Unknown, ReasonBadEnv
	case fleet.StatusPortClash:
		return Unknown, ReasonPortClash
	case fleet.StatusAdminOff:
		return Unknown, ReasonAdminOff
	case fleet.StatusError:
		return Unknown, ReasonAdminError
	case fleet.StatusUnused:
		return Inactive, ReasonUnused
	case fleet.StatusDown:
		if live.Benches && !live.BenchRunning {
			return Inactive, ReasonStopped
		}
		return Inactive, ReasonDown
	}
	if v.State == nil {
		return Unknown, ReasonAdminError
	}
	if d.Port-v.State.PortStart >= v.State.ActiveDevices {
		return Inactive, ReasonBeyondActive
	}
	return Active, ""
}

func parseRules(rules []string) []profile.Rule {
	r, err := profile.Parse(strings.Join(rules, ","))
	if err != nil {
		return nil
	}
	return r
}

// profileOf is the device's profile the way rcfg-sim picks it: the
// manifest's pinned profile, else the instance's rules and seed.
func profileOf(d Device, in *instInfo) (string, bool) {
	if d.Pinned != "" {
		if p, err := profile.ParseProfile(d.Pinned); err == nil && p != nil {
			return p.String(), true
		}
		return "", false
	}
	if p := profile.Assign(d.Hostname, in.seed, in.rules); p != nil {
		return p.String(), false
	}
	return "", false
}
