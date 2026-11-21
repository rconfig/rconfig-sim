package runs

import (
	"bytes"
	"encoding/csv"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

func counters(ok, bytes, auth float64) fleet.Counters {
	return fleet.Counters{Sessions: map[string]float64{"ok": ok}, Bytes: bytes, Faults: map[string]float64{"auth_fail": auth}}
}

func TestTotalsAndPeak(t *testing.T) {
	r := &Run{Totals: newTotals(), Instances: map[string]*Totals{}}
	r.AddSample(fleet.Sample{Instances: map[string]fleet.InstanceSample{
		"a": {Delta: counters(5, 100, 1), ActiveSessions: 3},
		"b": {Delta: counters(2, 50, 0), ActiveSessions: 4},
	}})
	r.AddSample(fleet.Sample{Instances: map[string]fleet.InstanceSample{
		"a": {Delta: counters(1, 10, 0), ActiveSessions: 6},
	}})
	if r.Totals.Sessions["ok"] != 8 || r.Totals.Bytes != 160 || r.Totals.Faults["auth_fail"] != 1 {
		t.Fatalf("totals %+v", r.Totals)
	}
	// Peak is the most open at one poll across the fleet: 3+4, not 6+4.
	if r.Totals.PeakActive != 7 || r.Instances["a"].PeakActive != 6 || r.Instances["b"].PeakActive != 4 {
		t.Fatalf("peaks %v %v %v", r.Totals.PeakActive, r.Instances["a"].PeakActive, r.Instances["b"].PeakActive)
	}
	if r.Totals.SessionsTotal() != 8 {
		t.Fatal("sessions total")
	}
}

func TestStoreKeepAndOrder(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s := NewStore(t.TempDir(), 3, func() time.Time { return now })
	var ids []string
	for i := 0; i < 5; i++ {
		r, err := s.New("tb", "TB", 1, []byte(`{"name":"TB"}`))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
		if i%2 == 1 {
			now = now.Add(time.Second)
		}
	}
	if ids[0] != "20261101-090000" || ids[1] != "20261101-090000-2" || ids[2] != "20261101-090001" {
		t.Fatalf("ids %v", ids)
	}
	list, err := s.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("list %d %v", len(list), err)
	}
	if list[0].ID != ids[4] || list[2].ID != ids[2] {
		t.Fatalf("newest first: %s .. %s", list[0].ID, list[2].ID)
	}
	if _, err := s.Get(ids[0]); err == nil {
		t.Fatal("oldest run not pruned")
	}
	if _, err := s.Get("../../etc/passwd"); err == nil {
		t.Fatal("path traversal")
	}

	r := list[0]
	r.Outcome, r.Ended = Stopped, now.Add(time.Minute)
	r.Events = append(r.Events, Event{At: now, Kind: "stop", Text: "Stopped"})
	if err := s.Save(r); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(r.ID)
	if got.Outcome != Stopped || len(got.Events) != 1 || got.Duration(time.Time{}) != time.Minute {
		t.Fatalf("saved run %+v", got)
	}
}

func TestCSVAndCompare(t *testing.T) {
	a := &Run{ID: "20261101-090000", Name: "A", Version: 1, Totals: newTotals(), Instances: map[string]*Totals{},
		Testbench: []byte(`{"name":"A","version":1,"build":{"devices":6,"seed":1},"runtime":{"faults":{"auth_fail":0.1}}}`)}
	b := &Run{ID: "20261101-100000", Name: "B", Version: 4, Totals: newTotals(), Instances: map[string]*Totals{},
		Testbench: []byte(`{"name":"B","version":4,"build":{"devices":6,"seed":2},"runtime":{"faults":{}}}`)}
	a.AddSample(fleet.Sample{Instances: map[string]fleet.InstanceSample{"10.0.0.1": {Delta: counters(5, 100, 1)}}})

	var buf bytes.Buffer
	if err := WriteCSV(&buf, a); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(rows) != 3 || rows[1][3] != "10.0.0.1" || rows[2][3] != "total" || rows[0][4] != "sessions_ok" || rows[2][4] != "5" {
		t.Fatalf("csv %v %v", rows, err)
	}

	c := Compare(a, b)
	var diff []string
	for _, r := range c.Settings {
		diff = append(diff, r.Label)
	}
	if got := strings.Join(diff, ","); got != "build.seed,name,runtime.faults.auth_fail" {
		t.Fatalf("settings that differ: %s", got)
	}
	if c.Totals[0].Label != "sessions" || c.Totals[0].A != "5" || c.Totals[0].B != "0" {
		t.Fatalf("totals %+v", c.Totals[0])
	}
}

func TestNotesAndTags(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	s := NewStore(t.TempDir(), 0, func() time.Time { return now })
	r, err := s.New("tb", "TB", 1, []byte(`{"name":"TB"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Update(r.ID, func(r *Run) error {
		return r.SetNotes("  rConfig 8.4.2 build 1203 ", SplitTags(" rc-8.4.2, nightly,, nightly "))
	})
	if err != nil || got.Notes != "rConfig 8.4.2 build 1203" || !slices.Equal(got.Tags, []string{"rc-8.4.2", "nightly"}) {
		t.Fatalf("update: %+v %v", got, err)
	}
	again, _ := s.Get(r.ID)
	if !again.HasTag("nightly") || again.HasTag("x") {
		t.Fatal("tags not stored")
	}
	if _, err := s.Update(r.ID, func(r *Run) error { return r.SetNotes(strings.Repeat("x", MaxNotes+1), nil) }); err == nil {
		t.Fatal("over-long notes accepted")
	}
	b := &Run{Totals: newTotals(), Instances: map[string]*Totals{}, Testbench: []byte(`{}`), Build: "B2"}
	c := Compare(again, b)
	if c.Notes[0].A != "rConfig 8.4.2 build 1203" || c.Notes[0].B != "-" || c.Notes[1].A != "rc-8.4.2, nightly" || c.Notes[2].B != "B2" {
		t.Fatalf("compare notes %+v", c.Notes)
	}
}
