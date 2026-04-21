package sshsrv

import (
	"math/rand"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/fault"
)

// BenchmarkDispatchHotPath runs the ResolveCommand+Dispatch inner loop under
// three fault configurations:
//
//   - NoConfig: faults == nil. Represents operational baseline.
//   - ZeroRate: faults configured with every type AND rate=0. This is the
//     interesting case — operators may leave the flags in place while
//     temporarily disabling injection. The hot path must be statistically
//     identical to NoConfig.
//   - HighRate: every type enabled, rate=1.0. Upper bound showing the
//     overhead when faults are actually rolling.
//
// Run with:  go test -run XXX -bench BenchmarkDispatchHotPath -count 5 ./internal/sshsrv/
//
// Accept delta between NoConfig and ZeroRate is within 2% per the spec.
func BenchmarkDispatchHotPath(b *testing.B) {
	cases := []struct {
		name   string
		faults *fault.Set
	}{
		{"NoConfig", nil},
		{"ZeroRate", mustFaults(b, "auth_fail,disconnect_mid,slow_response,malformed", 0.0)},
		{"HighRate", mustFaults(b, "auth_fail,disconnect_mid,slow_response,malformed", 1.0)},
	}
	for _, tc := range cases {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			rng := rand.New(rand.NewSource(1))
			body := makeFakeConfig(2048)
			state := &State{Hostname: "rtr-x", Serial: "FOC1234ABCD", ConfigBytes: body}
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				// Resolve+Dispatch one of a rotating set of commands. We include
				// show running-config so the disconnect_mid/malformed branches
				// (whose checks are in runShell) also exercise Roll() via the
				// inline pre-write fault check below.
				inputs := []string{"show version", "sh run", "term len 0", "exit"}
				in := inputs[i%len(inputs)]
				cmd, canonical := ResolveCommand(in)
				resp := Dispatch(cmd, canonical, state)

				// Mirror the two hot-path fault decision points that exist in
				// runShell before config write and per-command delay. Both are
				// scalar short-circuits when Set is nil/zero.
				_ = tc.faults.Roll(rng, fault.TypeSlowResponse)
				if len(resp.ConfigOutput) > 0 {
					_ = tc.faults.Roll(rng, fault.TypeDisconnectMid)
					_ = tc.faults.Roll(rng, fault.TypeMalformed)
				}
				_ = resp.Output
			}
		})
	}
}

func mustFaults(b *testing.B, types string, rate float64) *fault.Set {
	b.Helper()
	s, err := fault.NewSet(types, rate)
	if err != nil {
		b.Fatalf("fault.NewSet: %v", err)
	}
	return s
}

func makeFakeConfig(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte('A' + (i % 26))
	}
	return out
}
