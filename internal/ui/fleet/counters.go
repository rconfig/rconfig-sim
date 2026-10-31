package fleet

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

// SeriesWindow is how much chart history the controller keeps in memory.
const SeriesWindow = 10 * time.Minute

// Session outcomes, the rcfgsim_sessions_total result label values.
var Outcomes = []string{"ok", "error", "auth_fail", "disconnect"}

// Counters is one scrape of the instance metrics the Activity charts use.
type Counters struct {
	ActiveSessions float64            // rcfgsim_active_sessions (gauge)
	Sessions       map[string]float64 // rcfgsim_sessions_total by result
	Bytes          float64            // rcfgsim_bytes_sent_total
	Faults         map[string]float64 // rcfgsim_faults_injected_total by type
}

// ParseCounters reads the series it needs from Prometheus text output.
// Lines it does not know are ignored. Missing series read as zero, as on a
// freshly started instance.
func ParseCounters(r io.Reader) (Counters, error) {
	c := Counters{Sessions: map[string]float64{}, Faults: map[string]float64{}}
	found := false
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, value, ok := parseSample(line)
		if !ok {
			continue
		}
		switch name {
		case "rcfgsim_active_sessions":
			c.ActiveSessions, found = value, true
		case "rcfgsim_sessions_total":
			c.Sessions[labels["result"]] += value
			found = true
		case "rcfgsim_bytes_sent_total":
			c.Bytes, found = value, true
		case "rcfgsim_faults_injected_total":
			c.Faults[labels["type"]] += value
			found = true
		}
	}
	if err := sc.Err(); err != nil {
		return c, err
	}
	if !found {
		return c, errors.New("no rcfgsim metrics found")
	}
	return c, nil
}

// parseSample splits `name{k="v",...} value [timestamp]`.
func parseSample(line string) (name string, labels map[string]string, value float64, ok bool) {
	rest := line
	if i := strings.IndexAny(rest, "{ "); i < 0 {
		return "", nil, 0, false
	} else {
		name, rest = rest[:i], rest[i:]
	}
	if strings.HasPrefix(rest, "{") {
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			return "", nil, 0, false
		}
		labels = map[string]string{}
		for _, kv := range strings.Split(rest[1:end], ",") {
			k, v, found := strings.Cut(kv, "=")
			if found {
				labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		rest = rest[end+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// ParseBytesSent finds rcfgsim_bytes_sent_total in Prometheus text output.
func ParseBytesSent(r io.Reader) (float64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if name, _, v, ok := parseSample(sc.Text()); ok && name == "rcfgsim_bytes_sent_total" {
			return v, nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("rcfgsim_bytes_sent_total not found")
}

// CounterDelta is how much a counter grew between two scrapes. A counter
// that went down was reset (the instance restarted), so the delta is the
// new value: everything counted since the restart, as Prometheus's rate()
// treats it.
func CounterDelta(prev, cur float64) float64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// RatePoint is one fleet-wide chart point: the sum over every instance
// scraped in both of the last two polls.
type RatePoint struct {
	T              float64            `json:"t"`        // unix seconds, millisecond precision
	ActiveSessions float64            `json:"active"`   // sessions open now
	Sessions       map[string]float64 `json:"sessions"` // sessions ended per second, by outcome
	Bytes          float64            `json:"bytes"`    // bytes sent per second
	Faults         map[string]float64 `json:"faults"`   // faults injected per second, by type
}

func newPoint(t time.Time) RatePoint {
	p := RatePoint{T: float64(t.UnixMilli()) / 1000, Sessions: map[string]float64{}, Faults: map[string]float64{}}
	for _, o := range Outcomes {
		p.Sessions[o] = 0
	}
	for _, ft := range fault.AllTypes {
		p.Faults[ft.String()] = 0
	}
	return p
}

// addRates adds one instance's rates between prev and cur, over dt seconds.
func (p *RatePoint) addRates(prev, cur Counters, dt float64) {
	p.ActiveSessions += cur.ActiveSessions
	p.Bytes += CounterDelta(prev.Bytes, cur.Bytes) / dt
	for k, v := range cur.Sessions {
		p.Sessions[k] += CounterDelta(prev.Sessions[k], v) / dt
	}
	for k, v := range cur.Faults {
		p.Faults[k] += CounterDelta(prev.Faults[k], v) / dt
	}
}

type counterSample struct {
	at time.Time
	c  Counters
}
