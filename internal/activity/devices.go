package activity

import (
	"encoding/base64"
	"encoding/binary"
	"sync/atomic"
	"time"
)

// State is a device state code, as served by GET /admin/v1/devices.
type State uint8

// Device state codes. The numbers are part of the admin API; do not reorder.
const (
	StateNever      State = 0 // no session since start
	StateOK         State = 1
	StateAuthFail   State = 2
	StateDisconnect State = 3
	StateError      State = 4
	StateHang       State = 5 // a hang profile hung a session
	StateInactive   State = 6 // port index at or above active_devices
	StateActive     State = 7 // a session is in progress
)

// StateForOutcome maps a session outcome (the rcfgsim_sessions_total result
// label) to its state code. Unknown outcomes map to StateError.
func StateForOutcome(outcome string) State {
	switch outcome {
	case "ok":
		return StateOK
	case "auth_fail":
		return StateAuthFail
	case "disconnect":
		return StateDisconnect
	default:
		return StateError
	}
}

// stateMask holds the last final state in the low byte of a device's state
// word. The bits above count the sessions in progress, so overlapping
// sessions on one device report active until the last one ends.
const (
	stateMask    = 0xff
	sessionShift = 8
)

// DeviceTable holds each device's state and last-seen time, indexed by port
// offset (port minus the first port). Every method is safe for concurrent use
// and does not allocate, except the encoders. Out-of-range indexes are
// ignored.
type DeviceTable struct {
	state    []atomic.Uint32 // sessions in progress << 8 | last final State
	lastSeen []atomic.Uint32 // unix seconds; 0 = never
}

// NewDeviceTable returns a table for n devices, all in StateNever.
func NewDeviceTable(n int) *DeviceTable {
	if n < 0 {
		n = 0
	}
	return &DeviceTable{
		state:    make([]atomic.Uint32, n),
		lastSeen: make([]atomic.Uint32, n),
	}
}

// Len returns the number of devices.
func (t *DeviceTable) Len() int { return len(t.state) }

// Start marks a session in progress on device i. Pair with End.
func (t *DeviceTable) Start(i int) {
	if i < 0 || i >= len(t.state) {
		return
	}
	t.state[i].Add(1 << sessionShift)
}

// End ends a session that Start began: one fewer in progress, final state s,
// last seen now.
func (t *DeviceTable) End(i int, s State, now time.Time) {
	if i < 0 || i >= len(t.state) {
		return
	}
	for {
		old := t.state[i].Load()
		n := old >> sessionShift
		if n > 0 {
			n--
		}
		if t.state[i].CompareAndSwap(old, n<<sessionShift|uint32(s)) {
			break
		}
	}
	t.Touch(i, now)
}

// Set records final state s for a session that never started (refused
// before admission), leaving any sessions in progress counted, and sets last
// seen to now.
func (t *DeviceTable) Set(i int, s State, now time.Time) {
	if i < 0 || i >= len(t.state) {
		return
	}
	for {
		old := t.state[i].Load()
		if t.state[i].CompareAndSwap(old, old&^stateMask|uint32(s)) {
			break
		}
	}
	t.Touch(i, now)
}

// Touch sets device i's last seen time without changing its state.
func (t *DeviceTable) Touch(i int, now time.Time) {
	if i < 0 || i >= len(t.lastSeen) {
		return
	}
	t.lastSeen[i].Store(uint32(now.Unix()))
}

// State returns device i's current state: StateActive while any session is
// in progress, otherwise the last final state.
func (t *DeviceTable) State(i int) State {
	if i < 0 || i >= len(t.state) {
		return StateNever
	}
	w := t.state[i].Load()
	if w>>sessionShift > 0 {
		return StateActive
	}
	return State(w & stateMask)
}

// LastSeen returns device i's last seen time in unix seconds, 0 for never.
func (t *DeviceTable) LastSeen(i int) uint32 {
	if i < 0 || i >= len(t.lastSeen) {
		return 0
	}
	return t.lastSeen[i].Load()
}

// EncodeStates returns one state byte per device, base64 (standard, padded).
// Devices at or past activeDevices with no session in progress report
// StateInactive.
func (t *DeviceTable) EncodeStates(activeDevices int) string {
	buf := make([]byte, len(t.state))
	for i := range buf {
		s := t.State(i)
		if i >= activeDevices && s != StateActive {
			s = StateInactive
		}
		buf[i] = byte(s)
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// EncodeLastSeen returns each device's last seen time as a little-endian
// uint32 of unix seconds (0 = never), base64 (standard, padded).
func (t *DeviceTable) EncodeLastSeen() string {
	buf := make([]byte, 4*len(t.lastSeen))
	for i := range t.lastSeen {
		binary.LittleEndian.PutUint32(buf[4*i:], t.lastSeen[i].Load())
	}
	return base64.StdEncoding.EncodeToString(buf)
}
