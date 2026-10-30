package profile

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseProfile(t *testing.T) {
	cases := []struct {
		in   string
		want *Profile
	}{
		{"", nil},
		{"hang:3600s", &Profile{Kind: KindHang, Hang: time.Hour}},
		{"hang:2h", &Profile{Kind: KindHang, Hang: 2 * time.Hour}},
		{"latency:500ms", &Profile{Kind: KindLatency, LatencyMin: 500 * time.Millisecond, LatencyMax: 500 * time.Millisecond}},
		{"latency:200-800ms", &Profile{Kind: KindLatency, LatencyMin: 200 * time.Millisecond, LatencyMax: 800 * time.Millisecond}},
		{"latency:1s-3s", &Profile{Kind: KindLatency, LatencyMin: time.Second, LatencyMax: 3 * time.Second}},
		{"refuse", &Profile{Kind: KindRefuse}},
		{"disconnect", &Profile{Kind: KindDisconnect}},
	}
	for _, c := range cases {
		got, err := ParseProfile(c.in)
		if err != nil {
			t.Fatalf("ParseProfile(%q): %v", c.in, err)
		}
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("ParseProfile(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseProfileRejects(t *testing.T) {
	for _, in := range []string{
		"hang", "hang:0s", "hang:-1s", "hang:abc",
		"latency", "latency:800-200ms", "latency:x-1s",
		"refuse:1s", "disconnect:yes", "slow",
	} {
		if _, err := ParseProfile(in); err == nil {
			t.Errorf("ParseProfile(%q): want error, got none", in)
		}
	}
}

func TestParse(t *testing.T) {
	rules, err := Parse(" hang:3600s@3% , latency:200-800ms@10%,refuse@0.5")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("want 3 rules, got %d", len(rules))
	}
	if rules[0].Profile.Kind != KindHang || math.Abs(rules[0].Fraction-0.03) > 1e-12 {
		t.Errorf("rule 0 = %+v", rules[0])
	}
	if rules[2].Profile.Kind != KindRefuse || math.Abs(rules[2].Fraction-0.005) > 1e-12 {
		t.Errorf("rule 2 = %+v", rules[2])
	}

	if rules, err := Parse(""); err != nil || len(rules) != 0 {
		t.Errorf("Parse(\"\") = %v, %v; want no rules", rules, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"hang:3600s",                // no percent
		"hang:3600s@0%",             // zero
		"hang:3600s@101%",           // over 100
		"hang:3600s@60%,refuse@41%", // sum over 100
		"@3%",                       // no profile
		"bogus@3%",
	} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q): want error, got none", in)
		}
	}
}

func hostnames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("rtr-%05d", i)
	}
	return out
}

func chosen(t *testing.T, seed int64, rules []Rule, hosts []string) []string {
	t.Helper()
	var out []string
	for _, h := range hosts {
		if Assign(h, seed, rules) != nil {
			out = append(out, h)
		}
	}
	return out
}

func TestAssignIsDeterministicPerSeed(t *testing.T) {
	rules, _ := Parse("hang:3600s@3%")
	hosts := hostnames(2000)

	first := chosen(t, 42, rules, hosts)
	again := chosen(t, 42, rules, hosts)
	other := chosen(t, 7, rules, hosts)

	if strings.Join(first, ",") != strings.Join(again, ",") {
		t.Fatal("same seed chose different devices")
	}
	if strings.Join(first, ",") == strings.Join(other, ",") {
		t.Fatal("different seeds chose the same devices")
	}
}

func TestAssignDoesNotDependOnOrder(t *testing.T) {
	rules, _ := Parse("hang:3600s@3%")
	hosts := hostnames(500)
	want := map[string]bool{}
	for _, h := range chosen(t, 42, rules, hosts) {
		want[h] = true
	}
	for i := len(hosts) - 1; i >= 0; i-- {
		if got := Assign(hosts[i], 42, rules) != nil; got != want[hosts[i]] {
			t.Fatalf("%s: assignment changed with iteration order", hosts[i])
		}
	}
}

func TestAssignHitsTheConfiguredShare(t *testing.T) {
	rules, _ := Parse("hang:3600s@3%,latency:200-800ms@10%")
	hosts := hostnames(10000)
	counts := map[Kind]int{}
	for _, h := range hosts {
		if p := Assign(h, 42, rules); p != nil {
			counts[p.Kind]++
		}
	}
	if got := float64(counts[KindHang]) / 100; math.Abs(got-3) > 0.5 {
		t.Errorf("hang share = %.2f%%, want 3%% +/- 0.5", got)
	}
	if got := float64(counts[KindLatency]) / 100; math.Abs(got-10) > 1 {
		t.Errorf("latency share = %.2f%%, want 10%% +/- 1", got)
	}
}

func TestAssignWithoutRulesIsNormal(t *testing.T) {
	if p := Assign("rtr-1", 42, nil); p != nil {
		t.Fatalf("want nil, got %v", p)
	}
}

func TestNilProfileIsSafe(t *testing.T) {
	var p *Profile
	if p.Is(KindHang) || p.HangDuration() != 0 || p.String() != "" {
		t.Fatal("nil profile must behave as a normal device")
	}
}
