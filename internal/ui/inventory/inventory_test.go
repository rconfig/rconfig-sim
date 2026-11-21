package inventory

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rcfg-sim/rcfg-sim/internal/activity"
	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
)

const header = "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket,profile\n"

// manifest is n devices on IPs 10.0.0.1.. with perIP ports from 10000 each.
func manifest(n, perIP int) string {
	var b strings.Builder
	b.WriteString(header)
	for i := 0; i < n; i++ {
		model := []string{"sm", "md", "lg"}[i%3]
		fmt.Fprintf(&b, "rtr-%05d,10.0.%d.%d,%d,Cisco,cisco_ios,admin,\"p\"\"w,1\",en,/c/%d.cfg,%s,\n", i, i/perIP/250, 1+(i/perIP)%250, 10000+i%perIP, i, model)
	}
	return b.String()
}

func TestParseAndCache(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "manifest.csv")
	if err := os.WriteFile(p, []byte(manifest(6, 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCache()
	x, err := c.Load("B1", p)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := x.Lookup("rtr-00004")
	if !ok || d.IP != "10.0.0.2" || d.Port != 10001 || d.Password != `p"w,1` || d.Model != "md" || d.Template != "cisco_ios" {
		t.Fatalf("device %+v", d)
	}
	again, _ := c.Load("B1", p)
	if again != x {
		t.Fatal("same manifest parsed twice")
	}
	// A rewritten manifest (new hash) is parsed again.
	if err := os.WriteFile(p, []byte(manifest(3, 3)), 0o644); err != nil {
		t.Fatal(err)
	}
	if y, _ := c.Load("B1", p); y == x || len(y.Devices) != 3 {
		t.Fatal("changed manifest not reloaded")
	}
	c.Drop("B1")
	if len(c.Builds()) != 0 {
		t.Fatal("drop")
	}
	if _, err := Parse(strings.NewReader("a,b\n1,2\n")); err == nil {
		t.Fatal("manifest without hostname accepted")
	}
}

func TestLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("50k rows")
	}
	raw := manifest(50000, 2500)
	start := time.Now()
	x, err := Parse(strings.NewReader(raw))
	if err != nil || len(x.Devices) != 50000 {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("50k rows parsed in %s, want under 1s", d)
	}
	var views []fleet.View
	for i := 0; i < 20; i++ {
		ip := fmt.Sprintf("10.0.0.%d", i+1)
		views = append(views, upView(ip, 2500, 2000))
	}
	start = time.Now()
	rows := Join(x, Live{Views: views, LiveBuild: true})
	for _, q := range []Query{{}, {Sort: "ip", Desc: true}, {Status: Inactive, Q: "rtr-4"}, {Model: "lg", Sort: "model"}} {
		page, _ := Page(Filter(rows, q), q)
		_ = page
	}
	_ = Summarise(rows)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("join, four filters and sorts, summary on 50k took %s, want under 1s", d)
	}
}

func upView(ip string, ports, active int) fleet.View {
	return fleet.View{Instance: fleet.Instance{Name: ip, ListenIP: ip, PortStart: 10000, PortCount: ports},
		Status: fleet.StatusUp, State: &admin.State{PortStart: 10000, PortCount: ports, ActiveDevices: active,
			Profiles: admin.Profiles{Seed: 42, Rules: []string{}}}}
}

func TestStatusTable(t *testing.T) {
	x, err := Parse(strings.NewReader(manifest(18, 3) + "pinned-1,10.0.0.1,10002,Cisco,cisco_ios,a,b,c,/x,sm,hang:60s\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Six instances of 3 devices, one per row of the Phase 7 item 2 table.
	up := upView("10.0.0.1", 3, 2) // 10000, 10001 active; 10002 beyond active count
	down := fleet.View{Instance: fleet.Instance{Name: "10.0.0.2", ListenIP: "10.0.0.2", PortStart: 10000, PortCount: 3}, Status: fleet.StatusDown}
	off := fleet.View{Instance: fleet.Instance{Name: "10.0.0.3", ListenIP: "10.0.0.3", PortStart: 10000, PortCount: 3}, Status: fleet.StatusAdminOff}
	bad := fleet.View{Instance: fleet.Instance{Name: "10.0.0.4", ListenIP: "10.0.0.4", PortStart: 10000, PortCount: 3}, Status: fleet.StatusBadEnv}
	clash := fleet.View{Instance: fleet.Instance{Name: "10.0.0.5", ListenIP: "10.0.0.5", PortStart: 10000, PortCount: 3}, Status: fleet.StatusPortClash}
	// 10.0.0.6 has no instance.

	// Device table for 10.0.0.1: ok, auth_fail (seen), inactive.
	states := []byte{byte(activity.StateOK), byte(activity.StateAuthFail), byte(activity.StateInactive)}
	seen := make([]byte, 12)
	seen[4] = 100 // 10001 last seen at unix 100
	devs := map[string]*admin.Devices{"10.0.0.1": {PortStart: 10000, State: base64.StdEncoding.EncodeToString(states), LastSeen: base64.StdEncoding.EncodeToString(seen)}}

	live := Live{Views: []fleet.View{up, down, off, bad, clash}, Devices: devs, Benches: true, BenchRunning: true, LiveBuild: true}
	get := func(rows []Row, host string) Row {
		for _, r := range rows {
			if r.Hostname == host {
				return r
			}
		}
		t.Fatalf("no %s", host)
		return Row{}
	}
	rows := Join(x, live)
	for host, want := range map[string][2]string{
		"rtr-00000": {Active, ""},
		"rtr-00002": {Inactive, ReasonBeyondActive},
		"rtr-00003": {Inactive, ReasonDown},
		"rtr-00006": {Unknown, ReasonAdminOff},
		"rtr-00009": {Unknown, ReasonBadEnv},
		"rtr-00012": {Unknown, ReasonPortClash},
		"rtr-00015": {Unknown, ReasonNoInstance},
	} {
		if r := get(rows, host); r.Status != want[0] || r.Reason != want[1] {
			t.Errorf("%s: %s/%q, want %s/%q", host, r.Status, r.Reason, want[0], want[1])
		}
	}
	r := get(rows, "rtr-00001")
	if r.LastOutcome != "auth_fail" || r.LastSeen.Unix() != 100 || r.Instance != "10.0.0.1" {
		t.Errorf("last outcome %+v", r)
	}
	// A hang-profile device is active (reachable on purpose), and its
	// profile says so. This one is pinned at port index 2, so set the
	// active count to 3.
	up.State.ActiveDevices = 3
	live.Views[0] = up
	p := get(Join(x, live), "pinned-1")
	if p.Status != Active || !strings.HasPrefix(p.Profile, "hang") || !p.ProfilePin {
		t.Errorf("pinned hang device: %+v", p)
	}
	// No testbench running: a stopped instance's devices are "testbench stopped".
	live.BenchRunning = false
	if r := get(Join(x, live), "rtr-00003"); r.Reason != ReasonStopped {
		t.Errorf("no testbench running: %q", r.Reason)
	}
	// An instance the live build does not use (a scenario testbench).
	live.Views[1] = fleet.View{Instance: down.Instance, Status: fleet.StatusUnused}
	if r := get(Join(x, live), "rtr-00003"); r.Status != Inactive || r.Reason != "not used by this testbench" {
		t.Errorf("unused instance: %+v", r)
	}
	// A kept build that is not live.
	live.LiveBuild = false
	if r := get(Join(x, live), "rtr-00000"); r.Status != Inactive || r.Reason != ReasonNotLive {
		t.Errorf("not live: %+v", r)
	}
}

func TestQueryAndSummary(t *testing.T) {
	x, _ := Parse(strings.NewReader(manifest(9, 3)))
	rows := Join(x, Live{Views: []fleet.View{upView("10.0.0.1", 3, 1), upView("10.0.0.2", 3, 3)}, LiveBuild: true})
	q := ParseQuery(url.Values{"status": {"inactive"}, "sort": {"ip"}, "desc": {"1"}})
	got := Filter(rows, q)
	// 10.0.0.3 has no instance, so its devices are unknown, not inactive.
	if len(got) != 2 || got[0].Hostname != "rtr-00002" || got[1].Hostname != "rtr-00001" {
		t.Fatalf("inactive, ip desc: %+v", got)
	}
	if got := Filter(rows, Query{Q: "10.0.0.2"}); len(got) != 3 {
		t.Fatalf("search by IP: %d", len(got))
	}
	if got := Filter(rows, Query{Q: "RTR-0000"}); len(got) != 9 {
		t.Fatalf("search by hostname, any case: %d", len(got))
	}
	fam := Join(x, Live{Views: []fleet.View{upView("10.0.0.1", 3, 3)}, LiveBuild: true,
		FamilyOf: func(model, _ string) string {
			if model == "lg" {
				return "optical"
			}
			return "ip"
		}})
	if got := Filter(fam, Query{Family: "optical"}); len(got) != 3 || Summarise(fam).Families["ip"] != 6 {
		t.Fatalf("family filter %d, summary %v", len(got), Summarise(fam).Families)
	}
	if got := Filter(rows, Query{Model: "md", Profile: "none"}); len(got) != 3 {
		t.Fatalf("model filter: %d", len(got))
	}
	if q2 := ParseQuery(q.Values()); q2.Status != "inactive" || !q2.Desc || q2.Sort != "ip" {
		t.Fatalf("round trip %+v", q2)
	}
	page, pages := Page(rows, Query{Page: 2, PerPage: 4})
	if pages != 3 || len(page) != 4 || page[0].Hostname != rows[4].Hostname {
		t.Fatalf("page 2 of 3: %d rows, %d pages", len(page), pages)
	}
	s := Summarise(rows)
	if s.Total != 9 || s.Active != 4 || s.Inactive[ReasonBeyondActive] != 2 || s.Unknown != 3 || s.Models["sm"] != 3 {
		t.Fatalf("summary %+v", s)
	}
}

func TestExports(t *testing.T) {
	x, _ := Parse(strings.NewReader(manifest(1200, 600)))
	rows := Filter(Join(x, Live{Views: []fleet.View{upView("10.0.0.1", 600, 600)}, LiveBuild: true}), Query{Status: Active})
	if len(rows) != 600 {
		t.Fatalf("%d active", len(rows))
	}
	var buf bytes.Buffer
	flushes := 0
	if err := WriteCSV(&buf, rows, func() { flushes++ }); err != nil {
		t.Fatal(err)
	}
	if flushes == 0 {
		t.Error("CSV export did not stream (no flush)")
	}
	recs, err := csv.NewReader(bytes.NewReader(buf.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(recs[0], []string{"hostname", "ip", "port", "username", "password", "enable_password", "model", "status", "status_reason"}) {
		t.Fatalf("columns %v", recs[0])
	}
	if len(recs) != 601 || !slices.Equal(recs[1], []string{"rtr-00000", "10.0.0.1", "10000", "admin", `p"w,1`, "en", "sm", "active", ""}) {
		t.Fatalf("first row %v of %d", recs[1], len(recs))
	}
	if !strings.Contains(buf.String(), `"p""w,1"`) {
		t.Error("RFC 4180 quoting")
	}

	buf.Reset()
	flushes = 0
	if err := WriteJSON(&buf, rows, func() { flushes++ }); err != nil {
		t.Fatal(err)
	}
	var objs []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &objs); err != nil {
		t.Fatal(err)
	}
	if len(objs) != 600 || flushes == 0 || len(objs[0]) != 9 || objs[0]["port"] != float64(10000) || objs[0]["status_reason"] != "" {
		t.Fatalf("json: %d objects, %d flushes, %v", len(objs), flushes, objs[0])
	}
	buf.Reset()
	if err := WriteJSON(&buf, nil, nil); err != nil || strings.TrimSpace(buf.String()) != "[]" {
		t.Fatalf("empty json %q", buf.String())
	}
	if got := FileName("20261101-0751", "", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "csv"); got != "rcfg-sim-20261101-0751-all-20261101.csv" {
		t.Fatal(got)
	}
	if got := FileName("Smoke A", "inactive", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), "json"); got != "rcfg-sim-Smoke-A-inactive-20261101.json" {
		t.Fatal(got)
	}
}
