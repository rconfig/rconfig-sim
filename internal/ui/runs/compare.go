package runs

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

// Row is one line of a comparison: a label and each run's value.
type Row struct {
	Label string
	A, B  string
}

// Comparison is two runs side by side.
type Comparison struct {
	A, B     *Run
	Notes    []Row // notes and tags
	Totals   []Row // headline totals
	Settings []Row // testbench settings that differ
}

// Compare puts two runs side by side: their totals, and the testbench
// settings that differ between their snapshots.
func Compare(a, b *Run) Comparison {
	c := Comparison{A: a, B: b, Notes: []Row{
		{"notes", orDash(a.Notes), orDash(b.Notes)},
		{"tags", orDash(strings.Join(a.Tags, ", ")), orDash(strings.Join(b.Tags, ", "))},
		{"build", orDash(a.Build), orDash(b.Build)},
	}}
	add := func(label string, f func(*Run) float64) {
		c.Totals = append(c.Totals, Row{label, num(f(a)), num(f(b))})
	}
	add("sessions", func(r *Run) float64 { return r.Totals.SessionsTotal() })
	for _, o := range fleet.Outcomes {
		add("sessions "+o, func(r *Run) float64 { return r.Totals.Sessions[o] })
	}
	for _, ft := range fault.AllTypes {
		add("faults "+ft.String(), func(r *Run) float64 { return r.Totals.Faults[ft.String()] })
	}
	add("bytes", func(r *Run) float64 { return r.Totals.Bytes })
	add("peak active sessions", func(r *Run) float64 { return r.Totals.PeakActive })
	add("duration (s)", func(r *Run) float64 { return r.Duration(r.Ended).Seconds() })

	fa, fb := flatten(a.Testbench), flatten(b.Testbench)
	keys := slices.Sorted(maps.Keys(fa))
	for k := range fb {
		if _, ok := fa[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if fa[k] != fb[k] {
			c.Settings = append(c.Settings, Row{k, orDash(fa[k]), orDash(fb[k])})
		}
	}
	return c
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// flatten turns a testbench snapshot into dotted keys and values, so two
// can be compared setting by setting. version is left out: it always
// differs between saves.
func flatten(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return out
	}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(strings.TrimPrefix(prefix+"."+k, "."), e)
			}
		case []any:
			parts := make([]string, len(x))
			for i, e := range x {
				parts[i] = fmt.Sprint(e)
			}
			out[prefix] = strings.Join(parts, ", ")
		default:
			out[prefix] = fmt.Sprint(x)
		}
	}
	walk("", v)
	delete(out, "version")
	return out
}
