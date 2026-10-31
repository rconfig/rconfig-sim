package web

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// StateUnreachable marks the cells of an instance the controller could not
// read (down, admin API off, and so on). It is a controller-side value for
// the heatmap only, outside the admin API's state codes (section 6.1).
const StateUnreachable = 255

// stateNames are the heatmap legend keys, by state code.
var stateNames = map[byte]string{
	byte(activity.StateNever):      "never",
	byte(activity.StateOK):         "ok",
	byte(activity.StateAuthFail):   "auth_fail",
	byte(activity.StateDisconnect): "disconnect",
	byte(activity.StateError):      "error",
	byte(activity.StateHang):       "hang",
	byte(activity.StateInactive):   "inactive",
	byte(activity.StateActive):     "active",
	StateUnreachable:               "unreachable",
}

// LegendOrder is the order the legend lists states in.
var LegendOrder = []string{"active", "ok", "auth_fail", "disconnect", "error", "hang", "inactive", "never", "unreachable"}

// Frame is the whole fleet's device state at one moment: every instance's
// cells end to end, in instance (IP) order.
type Frame struct {
	At        int64           `json:"at"`  // unix milliseconds, when assembled
	Now       int64           `json:"now"` // unix seconds, server clock (for last-seen ages)
	Total     int             `json:"total"`
	Instances []FrameInstance `json:"instances"`
	State     string          `json:"state"`     // base64, one state byte per cell
	LastSeen  string          `json:"last_seen"` // base64, little-endian uint32 unix seconds per cell
	Counts    map[string]int  `json:"counts"`    // cells per state name
}

// FrameInstance is one instance's run of cells.
type FrameInstance struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	PortStart int    `json:"port_start"`
	Count     int    `json:"count"`  // cells (its port count)
	Offset    int    `json:"offset"` // first cell in the frame
	Reachable bool   `json:"reachable"`
	Status    string `json:"status"`
}

// AssembleFrame lays the instances' device states end to end. An instance
// with no usable devices answer gets unreachable cells, so the grid keeps
// its shape while an instance is down.
func AssembleFrame(views []fleet.View, devs map[string]*deviceResult, now time.Time) *Frame {
	f := &Frame{At: now.UnixMilli(), Now: now.Unix(), Counts: map[string]int{}}
	for _, v := range views {
		if v.Err != nil || v.PortCount <= 0 {
			continue
		}
		f.Instances = append(f.Instances, FrameInstance{
			Name: v.Name, IP: v.ListenIP, PortStart: v.PortStart, Count: v.PortCount, Offset: f.Total, Status: string(v.Status),
		})
		f.Total += v.PortCount
	}
	states := make([]byte, f.Total)
	seen := make([]byte, 4*f.Total)
	for i := range f.Instances {
		in := &f.Instances[i]
		cells := states[in.Offset : in.Offset+in.Count]
		r := devs[in.Name]
		if r == nil || r.err != nil || len(r.states) != in.Count || len(r.lastSeen) != 4*in.Count {
			for j := range cells {
				cells[j] = StateUnreachable
			}
			continue
		}
		in.Reachable = true
		copy(cells, r.states)
		copy(seen[4*in.Offset:], r.lastSeen)
	}
	for _, s := range states {
		name, ok := stateNames[s]
		if !ok {
			name = "never"
		}
		f.Counts[name]++
	}
	f.State = base64.StdEncoding.EncodeToString(states)
	f.LastSeen = base64.StdEncoding.EncodeToString(seen)
	return f
}

// decodeDevices unpacks an /admin/v1/devices answer.
func decodeDevices(d *admin.Devices) (states, lastSeen []byte, err error) {
	if states, err = base64.StdEncoding.DecodeString(d.State); err != nil {
		return nil, nil, fmt.Errorf("devices state: %w", err)
	}
	if lastSeen, err = base64.StdEncoding.DecodeString(d.LastSeen); err != nil {
		return nil, nil, fmt.Errorf("devices last_seen: %w", err)
	}
	if len(lastSeen) != 4*len(states) {
		return nil, nil, fmt.Errorf("devices: %d states but %d last_seen bytes", len(states), len(lastSeen))
	}
	return states, lastSeen, nil
}
