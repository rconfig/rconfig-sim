package main

import (
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

func TestParseFaults(t *testing.T) {
	cases := []struct {
		name    string
		rates   string
		types   string
		rate    float64
		wantErr bool
		want    string // fault.Set.String()
	}{
		{"nothing set", "", "", 0, false, ""},
		{"legacy", "", "auth_fail,malformed", 0.1, false, "auth_fail=0.1,malformed=0.1"},
		{"per-type", "auth_fail=0.02,disconnect_mid=0.005", "", 0, false, "auth_fail=0.02,disconnect_mid=0.005"},
		// The systemd unit always passes all three, legacy ones at defaults.
		{"per-type with legacy defaults", "slow_response=1", "", 0.0, false, "slow_response=1"},
		{"both: non-zero rate", "auth_fail=0.1", "", 0.1, true, ""},
		{"both: types", "auth_fail=0.1", "auth_fail", 0, true, ""},
		{"bad per-type", "auth_fail=2", "", 0, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseFaults(tc.rates, tc.types, tc.rate)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v, got %v", tc.wantErr, err)
			}
			if err == nil && s.String() != tc.want {
				t.Fatalf("want %q, got %q", tc.want, s.String())
			}
		})
	}

	// Legacy and per-type forms give the same Set.
	a, _ := parseFaults("", "auth_fail,slow_response", 0.25)
	b, _ := parseFaults("auth_fail=0.25,slow_response=0.25", "", 0)
	for _, ft := range fault.AllTypes {
		if a.Rate(ft) != b.Rate(ft) {
			t.Errorf("%s: legacy %v, per-type %v", ft, a.Rate(ft), b.Rate(ft))
		}
	}
}

func TestProfileItems(t *testing.T) {
	got := profileItems(" hang:3600s@3% ,, latency:200-800ms@10%")
	if len(got) != 2 || got[0] != "hang:3600s@3%" || got[1] != "latency:200-800ms@10%" {
		t.Fatalf("got %q", got)
	}
	if got := profileItems(""); got != nil {
		t.Fatalf("empty spec: got %q", got)
	}
}

func TestBuildVersion(t *testing.T) {
	old := version
	defer func() { version = old }()
	version = "v0.0.6"
	if got := buildVersion(); got != "0.0.6" {
		t.Fatalf("want 0.0.6, got %q", got)
	}
	version = ""
	if got := buildVersion(); got == "" {
		t.Fatal("empty version")
	}
}

func TestLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:9100": true, "[::1]:9100": true, "localhost:9100": true,
		"0.0.0.0:9100": false, "10.50.0.1:9100": false, ":9100": false, "bad": false,
	} {
		if got := loopback(addr); got != want {
			t.Errorf("%s: want %v, got %v", addr, want, got)
		}
	}
}
