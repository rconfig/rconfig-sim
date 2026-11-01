package fleet

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
)

type fakeUnits map[string]string

func (u fakeUnits) IsActive(_ context.Context, name string) (string, error) { return u[name], nil }

func writeEnv(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o660); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUnitDefaultsMatchUnitFile keeps UnitDefaults in step with the
// Environment= lines in the shipped unit.
func TestUnitDefaultsMatchUnitFile(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/systemd/rcfg-sim@.service")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^Environment=([A-Z_]+)=(.*)$`)
	unit := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		unit[m[1]] = m[2]
	}
	for k, v := range UnitDefaults {
		if got, ok := unit[k]; !ok || got != v {
			t.Errorf("%s: UnitDefaults %q, unit file %q (present %v)", k, v, got, ok)
		}
	}
	if unit["LISTEN_IP"] != "%i" {
		t.Errorf("LISTEN_IP default must be the instance name, unit has %q", unit["LISTEN_IP"])
	}
}

func TestDiscover(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "10.50.0.2.env", "METRICS_ADDR=10.50.0.2:9100\n")
	writeEnv(t, dir, "10.50.0.10.env", "LISTEN_IP=10.50.0.10\nPORT_COUNT=100\nMETRICS_ADDR=0.0.0.0:9101\n")
	writeEnv(t, dir, "10.50.0.1.env", "METRICS_ADDR=10.50.0.1:9100\nPORT_START=abc\n")
	writeEnv(t, dir, "rcfg-sim-instance.env.sample", "LISTEN_IP=1.2.3.4\n")
	_ = os.Mkdir(filepath.Join(dir, "dir.env"), 0o755)

	insts, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range insts {
		names = append(names, in.Name)
	}
	if strings.Join(names, " ") != "10.50.0.1 10.50.0.2 10.50.0.10" {
		t.Fatalf("names/order: %v", names)
	}
	one, two, ten := insts[0], insts[1], insts[2]
	if one.Err == nil || !strings.Contains(one.Err.Error(), "PORT_START") {
		t.Errorf("bad PORT_START must be reported: %v", one.Err)
	}
	if two.Err != nil || two.ListenIP != "10.50.0.2" || two.PortStart != 10000 || two.PortCount != 2500 ||
		two.Manifest != "/opt/rcfg-sim/manifest.csv" || two.DialAddr != "10.50.0.2:9100" || two.Env["PROFILE_SEED"] != "42" {
		t.Errorf("defaults not applied: %+v", two)
	}
	if ten.DialAddr != "127.0.0.1:9101" || ten.PortCount != 100 {
		t.Errorf("wildcard bind must dial loopback: %+v", ten)
	}
	if len(one.PortClash)+len(two.PortClash)+len(ten.PortClash) != 0 {
		t.Errorf("no clash expected: %v %v %v", one.PortClash, two.PortClash, ten.PortClash)
	}
}

func TestDiscoverPortClash(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, "10.50.0.1.env", "")                              // default 0.0.0.0:9100
	writeEnv(t, dir, "10.50.0.2.env", "METRICS_ADDR=10.50.0.2:9100\n") // wildcard on .1 covers it
	writeEnv(t, dir, "10.50.0.3.env", "METRICS_ADDR=10.50.0.3:9101\n")
	insts, _ := Discover(dir)
	if !slices.Equal(insts[0].PortClash, []string{"10.50.0.2"}) || !slices.Equal(insts[1].PortClash, []string{"10.50.0.1"}) || insts[2].PortClash != nil {
		t.Fatalf("clashes: %v / %v / %v", insts[0].PortClash, insts[1].PortClash, insts[2].PortClash)
	}
}

func TestDesiredFromEnv(t *testing.T) {
	legacy := Instance{PortCount: 10, Env: map[string]string{}}
	for k, v := range UnitDefaults {
		legacy.Env[k] = v
	}
	legacy.Env["FAULT_RATE"] = "0.1"
	legacy.Env["FAULT_TYPES"] = "auth_fail"
	legacy.Env["DEVICE_PROFILES"] = " hang:3600s@3% , refuse@1%"
	d, err := DesiredFromEnv(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if d.Faults["auth_fail"] != 0.1 || d.Faults["malformed"] != 0 || d.ActiveDevices != 10 ||
		!slices.Equal(d.ProfileRules, []string{"hang:3600s@3%", "refuse@1%"}) || d.DelayMaxMS != 500 {
		t.Fatalf("desired: %+v", d)
	}
	legacy.Env["FAULT_RATES"] = "malformed=0.5"
	legacy.Env["FAULT_TYPES"] = ""
	legacy.Env["FAULT_RATE"] = "0"
	legacy.Env["ACTIVE_DEVICES"] = "7"
	if d, _ = DesiredFromEnv(legacy); d.Faults["malformed"] != 0.5 || d.Faults["auth_fail"] != 0 || d.ActiveDevices != 7 {
		t.Fatalf("per-type: %+v", d)
	}
	legacy.Env["PROFILE_SEED"] = "x"
	if _, err := DesiredFromEnv(legacy); err == nil {
		t.Fatal("bad seed must be an error")
	}
}

func newFleet(t *testing.T, dir string, units fakeUnits) *Fleet {
	t.Helper()
	f := New(Options{EnvDir: dir, Units: units})
	f.Poll(context.Background())
	return f
}

func statuses(f *Fleet) map[string]Status {
	out := map[string]Status{}
	for _, v := range f.Views() {
		out[v.Name] = v.Status
	}
	return out
}

func TestPollStatuses(t *testing.T) {
	dir := t.TempDir()
	uitest.Start(t, dir, "127.0.0.1", uitest.Options{})
	down := uitest.Start(t, dir, "127.0.0.2", uitest.Options{})
	off := uitest.Start(t, dir, "127.0.0.3", uitest.Options{})
	down.Down.Store(true)
	off.AdminOff.Store(true)

	f := newFleet(t, dir, fakeUnits{"127.0.0.2": "failed"})
	want := map[string]Status{"127.0.0.1": StatusUp, "127.0.0.2": StatusDown, "127.0.0.3": StatusAdminOff}
	if got := statuses(f); !mapsEqual(got, want) {
		t.Fatalf("statuses: %v", got)
	}
	v, _ := f.View("127.0.0.2")
	if v.Unit != "failed" {
		t.Errorf("down instance unit state: %q", v.Unit)
	}
	v, _ = f.View("127.0.0.3")
	if !strings.Contains(v.Detail, "ADMIN=true") {
		t.Errorf("admin-off card must say what to add: %q", v.Detail)
	}
	v, _ = f.View("127.0.0.1")
	if v.State == nil || v.State.Generation != 1 || len(v.Drift) != 0 {
		t.Fatalf("up instance: state %+v drift %v", v.State, v.Drift)
	}

	// Coming back: the next poll shows it up.
	down.Down.Store(false)
	f.Poll(context.Background())
	if statuses(f)["127.0.0.2"] != StatusUp {
		t.Fatal("restarted instance not up")
	}
}

func mapsEqual[K comparable, V comparable](a, b map[K]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestBytesHistory(t *testing.T) {
	dir := t.TempDir()
	in := uitest.Start(t, dir, "127.0.0.1", uitest.Options{})
	f := newFleet(t, dir, nil)
	in.Bytes.Store(1_000_000)
	f.Poll(context.Background())
	v, _ := f.View("127.0.0.1")
	if len(v.BytesPS) != 1 || v.BytesPS[0] <= 0 {
		t.Fatalf("bytes/s after two polls: %v", v.BytesPS)
	}
}

func TestParseBytesSent(t *testing.T) {
	for in, want := range map[string]float64{
		"# HELP x\nrcfgsim_bytes_sent_total 1.5e+06\n":                       1.5e6,
		"rcfgsim_bytes_sent_totalx 9\nrcfgsim_bytes_sent_total{a=\"b\"} 7\n": 7,
	} {
		got, err := ParseBytesSent(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("%q: got %v %v", in, got, err)
		}
	}
	if _, err := ParseBytesSent(strings.NewReader("other 1\n")); err == nil {
		t.Error("missing metric must be an error")
	}
}

func TestApplyFaultsFansOutAndPersists(t *testing.T) {
	dir := t.TempDir()
	a := uitest.Start(t, dir, "127.0.0.1", uitest.Options{ExtraEnv: "FAULT_RATE=0.1\nFAULT_TYPES=auth_fail\n"})
	b := uitest.Start(t, dir, "127.0.0.2", uitest.Options{})
	down := uitest.Start(t, dir, "127.0.0.3", uitest.Options{})
	off := uitest.Start(t, dir, "127.0.0.4", uitest.Options{})
	down.Down.Store(true)
	off.AdminOff.Store(true)
	f := newFleet(t, dir, nil)
	offBefore, _ := os.ReadFile(off.EnvPath)

	res := f.ApplyFaults(context.Background(), map[string]float64{"disconnect_mid": 0.005, "slow_response": 0.25})
	by := map[string]Result{}
	for _, r := range res {
		by[r.Instance] = r
	}
	if !by["127.0.0.1"].OK() || !by["127.0.0.2"].OK() || by["127.0.0.1"].NextStart {
		t.Fatalf("up instances: %+v", res)
	}
	if r := by["127.0.0.3"]; !r.OK() || !r.NextStart || r.State != nil {
		t.Fatalf("down instance must be saved for its next start: %+v", r)
	}
	if r := by["127.0.0.4"]; r.OK() || !r.Skipped {
		t.Fatalf("admin-off instance must be skipped: %+v", r)
	}
	for _, in := range []*uitest.Instance{a, b} {
		if r := in.Live().Faults.Rate(fault.TypeSlowResponse); r != 0.25 {
			t.Errorf("%s: live slow_response %v", in.Name, r)
		}
	}
	for _, in := range []*uitest.Instance{a, b, down} {
		e, _ := envfile.Read(in.EnvPath)
		if v, _ := e.Get("FAULT_RATES"); v != "disconnect_mid=0.005,slow_response=0.25" {
			t.Errorf("%s: FAULT_RATES %q", in.Name, v)
		}
	}
	e, _ := envfile.Read(a.EnvPath)
	if r, _ := e.Get("FAULT_RATE"); r != "0" {
		t.Errorf("legacy FAULT_RATE must be cleared, got %q", r)
	}
	if after, _ := os.ReadFile(off.EnvPath); string(after) != string(offBefore) {
		t.Error("a skipped instance must keep its env file")
	}

	// After the write, the next poll sees no drift; the down instance comes
	// back on the saved settings, also without drift.
	down.Down.Store(false)
	f.Poll(context.Background())
	if v, _ := f.View("127.0.0.1"); len(v.Drift) != 0 {
		t.Fatalf("drift after apply: %v", v.Drift)
	}
	if v, _ := f.View("127.0.0.3"); !slices.Equal(v.Drift, []string{"faults"}) {
		// The fake keeps its old live config across "restart"; a real
		// instance would read the env file. Drift proves the file changed.
		t.Fatalf("down instance env file not updated: drift %v", v.Drift)
	}
}

// TestPartialApplyWritesOnlyAcceptedKeys: when an instance accepts the faults
// and refuses the profiles, the fault keys are written and the profile keys
// are not (section 5.4).
func TestPartialApplyWritesOnlyAcceptedKeys(t *testing.T) {
	dir := t.TempDir()
	up := uitest.Start(t, dir, "127.0.0.1", uitest.Options{ExtraEnv: "DEVICE_PROFILES=refuse@1%\n"})
	down := uitest.Start(t, dir, "127.0.0.2", uitest.Options{ExtraEnv: "DEVICE_PROFILES=refuse@1%\n"})
	down.Down.Store(true)
	f := newFleet(t, dir, nil)

	// Rules adding up to 120%: valid syntax per rule, refused as a whole.
	res := f.ApplyFailures(context.Background(), map[string]float64{"malformed": 0.5}, 9, []string{"refuse@60%", "disconnect@60%"})
	for _, r := range res {
		if r.OK() || r.Err == nil || !strings.Contains(r.Err.Error(), "profiles") || !slices.Equal(r.Applied, []string{"faults"}) {
			t.Fatalf("%s: want faults applied and profiles refused, got %+v", r.Instance, r)
		}
	}
	if up.Live().Faults.Rate(fault.TypeMalformed) != 0.5 {
		t.Fatal("up instance: faults not applied")
	}
	for _, in := range []*uitest.Instance{up, down} {
		e, _ := envfile.Read(in.EnvPath)
		if v, _ := e.Get("FAULT_RATES"); v != "malformed=0.5" {
			t.Errorf("%s: accepted FAULT_RATES not written: %q", in.Name, v)
		}
		if v, _ := e.Get("DEVICE_PROFILES"); v != "refuse@1%" {
			t.Errorf("%s: refused DEVICE_PROFILES written: %q", in.Name, v)
		}
		if v, ok := e.Get("PROFILE_SEED"); ok {
			t.Errorf("%s: refused PROFILE_SEED written: %q", in.Name, v)
		}
	}
}

func TestDriftAndReapply(t *testing.T) {
	dir := t.TempDir()
	in := uitest.Start(t, dir, "127.0.0.1", uitest.Options{})
	f := newFleet(t, dir, nil)
	if v, _ := f.View(in.Name); len(v.Drift) != 0 {
		t.Fatalf("fresh instance drift: %v", v.Drift)
	}

	// A hand edit of the env file.
	b, _ := os.ReadFile(in.EnvPath)
	_ = os.WriteFile(in.EnvPath, append(b, []byte("MAX_CONCURRENT_SESSIONS=12\nDEVICE_PROFILES=refuse@50%\n")...), 0o660)
	f.Poll(context.Background())
	v, _ := f.View(in.Name)
	if !slices.Equal(v.Drift, []string{"profiles", "session cap"}) {
		t.Fatalf("drift: %v", v.Drift)
	}

	if r := f.Reapply(context.Background(), in.Name); !r.OK() {
		t.Fatalf("reapply: %+v", r)
	}
	if l := in.Live(); l.MaxSessions != 12 || !slices.Equal(l.ProfileRules, []string{"refuse@50%"}) {
		t.Fatalf("live after reapply: %+v", l)
	}
	f.Poll(context.Background())
	if v, _ := f.View(in.Name); len(v.Drift) != 0 {
		t.Fatalf("drift after reapply: %v", v.Drift)
	}
}

func TestApplyActiveSplitsAndPersists(t *testing.T) {
	dir := t.TempDir()
	a := uitest.Start(t, dir, "127.0.0.1", uitest.Options{Devices: 3})
	b := uitest.Start(t, dir, "127.0.0.2", uitest.Options{Devices: 7})
	f := newFleet(t, dir, nil)
	res, err := f.ApplyActive(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if !r.OK() {
			t.Fatalf("%+v", r)
		}
	}
	if a.Live().ActiveDevices+b.Live().ActiveDevices != 5 || a.Live().ActiveDevices != 2 || b.Live().ActiveDevices != 3 {
		t.Fatalf("split: %d + %d", a.Live().ActiveDevices, b.Live().ActiveDevices)
	}
	e, _ := envfile.Read(b.EnvPath)
	if v, _ := e.Get("ACTIVE_DEVICES"); v != "3" {
		t.Fatalf("ACTIVE_DEVICES %q", v)
	}
	if _, err := f.ApplyActive(context.Background(), 11); err == nil {
		t.Fatal("target over the fleet total must fail")
	}
}

func TestApplySessionCapsAndDelay(t *testing.T) {
	dir := t.TempDir()
	a := uitest.Start(t, dir, "127.0.0.1", uitest.Options{})
	b := uitest.Start(t, dir, "127.0.0.2", uitest.Options{})
	f := newFleet(t, dir, nil)
	res := f.ApplySessionCaps(context.Background(), map[string]int64{"127.0.0.2": 9})
	if len(res) != 1 || !res[0].OK() || b.Live().MaxSessions != 9 || a.Live().MaxSessions != 5000 {
		t.Fatalf("caps: %+v a=%d b=%d", res, a.Live().MaxSessions, b.Live().MaxSessions)
	}
	for _, r := range f.ApplyDelay(context.Background(), 0, 20) {
		if !r.OK() {
			t.Fatalf("delay: %+v", r)
		}
	}
	e, _ := envfile.Read(a.EnvPath)
	if v, _ := e.Get("RESPONSE_DELAY_MS_MAX"); v != "20" {
		t.Fatalf("RESPONSE_DELAY_MS_MAX %q", v)
	}
	res = f.ApplyDelay(context.Background(), 30, 20)
	if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "min <= max") {
		t.Fatalf("instance validation error must come back: %+v", res[0])
	}
}

func TestSplitActive(t *testing.T) {
	cases := []struct {
		target  int
		weights []int
		want    []int
	}{
		{0, []int{3, 7}, []int{0, 0}},
		{10, []int{3, 7}, []int{3, 7}},
		{5, []int{3, 7}, []int{2, 3}},
		{1, []int{1, 1, 1}, []int{1, 0, 0}},
		{2, []int{1, 1, 1}, []int{1, 1, 0}},
		{7, []int{2500, 2500, 2500}, []int{3, 2, 2}},
		{0, nil, []int{}},
	}
	for _, c := range cases {
		got, err := SplitActive(c.target, c.weights)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("SplitActive(%d, %v) = %v, %v; want %v", c.target, c.weights, got, err, c.want)
		}
	}
	// Exact total and per-instance bound, over many targets.
	w := []int{2500, 1700, 13, 999}
	for target := 0; target <= 5212; target += 37 {
		got, _ := SplitActive(target, w)
		sum := 0
		for i, g := range got {
			sum += g
			if g > w[i] {
				t.Fatalf("target %d: share %d over weight %d", target, g, w[i])
			}
		}
		if sum != target {
			t.Fatalf("target %d: shares add to %d", target, sum)
		}
	}
	if _, err := SplitActive(-1, w); err == nil {
		t.Error("negative target must fail")
	}
}

func TestAffectedBy(t *testing.T) {
	dir := t.TempDir()
	a := uitest.Start(t, dir, "127.0.0.1", uitest.Options{Devices: 50})
	b := uitest.Start(t, dir, "127.0.0.2", uitest.Options{Devices: 50})
	uitest.WriteManifest(t, dir, map[string]string{"rtr-127.0.0.2-0": "hang:7200s"}, a, b)
	f := newFleet(t, dir, nil)

	got, err := f.AffectedBy(42, []string{"refuse@10%"})
	if err != nil {
		t.Fatal(err)
	}
	var refused, pinned int
	for _, x := range got {
		switch {
		case x.Pinned:
			pinned++
			if x.Host != "rtr-127.0.0.2-0" || x.Profile != "hang:2h0m0s" {
				t.Errorf("pinned: %+v", x)
			}
		case x.Profile == "refuse":
			refused++
		}
	}
	if pinned != 1 || refused == 0 || refused > 25 {
		t.Fatalf("pinned %d, refused %d of 100 at 10%%", pinned, refused)
	}
	// Same seed, same devices.
	again, _ := f.AffectedBy(42, []string{"refuse@10%"})
	if len(again) != len(got) || again[0] != got[0] {
		t.Fatal("selection must be stable for a seed")
	}
	if _, err := f.AffectedBy(42, []string{"bogus@1%"}); err == nil {
		t.Fatal("bad rule must fail")
	}
}

// TestApplyActiveIncludesDownInstances (section 5.4): the split covers up
// and down instances, a down instance weighted by PORT_COUNT from its env
// file, and its share is saved to ACTIVE_DEVICES for its next start.
func TestApplyActiveIncludesDownInstances(t *testing.T) {
	dir := t.TempDir()
	up := uitest.Start(t, dir, "127.0.0.1", uitest.Options{Devices: 3})
	down := uitest.Start(t, dir, "127.0.0.2", uitest.Options{Devices: 7})
	off := uitest.Start(t, dir, "127.0.0.3", uitest.Options{Devices: 5})
	down.Down.Store(true)
	off.AdminOff.Store(true)
	f := newFleet(t, dir, nil)
	offBefore, _ := os.ReadFile(off.EnvPath)

	if max, active := f.ActiveDevicesTotal(); max != 10 || active != 3+7 {
		t.Fatalf("totals: max %d active %d, want 10 and 10 (down instance reads ACTIVE_DEVICES=all)", max, active)
	}
	split, err := f.ActiveSplit(5)
	if err != nil || len(split) != 2 || split["127.0.0.1"]+split["127.0.0.2"] != 5 {
		t.Fatalf("split: %v %v", split, err)
	}
	if _, err := f.ActiveSplit(11); err == nil {
		t.Fatal("a target over up+down capacity must fail")
	}

	res, err := f.ApplyActive(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Result{}
	for _, r := range res {
		by[r.Instance] = r
	}
	if !by["127.0.0.1"].OK() || by["127.0.0.1"].NextStart {
		t.Fatalf("up: %+v", by["127.0.0.1"])
	}
	if r := by["127.0.0.2"]; !r.OK() || !r.NextStart {
		t.Fatalf("down: %+v", r)
	}
	if _, ok := by["127.0.0.3"]; ok {
		t.Fatal("an admin-off instance takes no share")
	}
	if up.Live().ActiveDevices != split["127.0.0.1"] {
		t.Fatalf("up live: %d, split %d", up.Live().ActiveDevices, split["127.0.0.1"])
	}
	e, _ := envfile.Read(down.EnvPath)
	if v, _ := e.Get("ACTIVE_DEVICES"); v != strconv.Itoa(split["127.0.0.2"]) {
		t.Fatalf("down ACTIVE_DEVICES %q, split %d", v, split["127.0.0.2"])
	}
	if after, _ := os.ReadFile(off.EnvPath); string(after) != string(offBefore) {
		t.Fatal("admin-off env file changed")
	}

	// The down instance's saved share counts in the totals.
	f.Poll(context.Background())
	if max, active := f.ActiveDevicesTotal(); max != 10 || active != 5 {
		t.Fatalf("totals after apply: max %d active %d", max, active)
	}
}
