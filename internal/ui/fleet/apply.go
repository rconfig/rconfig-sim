package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/rcfg-sim/rcfg-sim/internal/admin"
	"github.com/rcfg-sim/rcfg-sim/internal/fault"
	"github.com/rcfg-sim/rcfg-sim/internal/profile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
)

// maxDelayMS matches the admin API's limit on PUT /admin/v1/behaviour.
const maxDelayMS = 3_600_000

// Result is one instance's outcome in a fan-out (section 5.4).
type Result struct {
	Instance string
	// Err: the change, or part of it, was not applied. With Applied set, the
	// parts listed were applied and persisted and the rest were not.
	Err     error
	EnvErr  error        // the instance applied the change but the env file write failed (drift will show)
	State   *admin.State // the instance's state after the change; nil unless up
	Applied []string     // admin endpoints the instance accepted, in order
	// NextStart: the instance is down, so only its env file was written; the
	// change applies when it next starts.
	NextStart bool
	// Skipped: the instance was not in a state to take the change (admin API
	// off, token rejected, bad env file, port clash, error). Nothing written.
	Skipped bool
}

// OK reports whether the change was fully applied (or, for a down
// instance, saved for its next start) and persisted.
func (r Result) OK() bool { return r.Err == nil && r.EnvErr == nil }

// request is one admin API call in a fan-out, with the env keys it owns.
type request struct {
	path string
	body any
	// env gives the keys to persist once the instance accepted the call,
	// from the state it returned. Nil: persist nothing (re-apply).
	env func(admin.State) map[string]string
	// want gives the same keys from the requested values, for a down
	// instance whose env file is written without asking it. wantErr is set
	// when the requested values are invalid, so nothing is written.
	want    map[string]string
	wantErr error
}

// fanOut applies each target's requests in parallel, with a timeout per
// instance (section 5.4):
//
//   - Up: the calls are made in order and stop at the first refusal. The env
//     file gets exactly the keys of the calls the instance accepted.
//   - Down: the env file is written with the requested values, marked
//     NextStart, so a stopped instance does not come back on old settings.
//   - Anything else: skipped, nothing written.
func (f *Fleet) fanOut(ctx context.Context, targets []View, reqs func(View) []request) []Result {
	results := make([]Result, len(targets))
	var wg sync.WaitGroup
	for i, v := range targets {
		results[i].Instance = v.Name
		switch v.Status {
		case StatusUp:
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = f.applyUp(ctx, v, reqs(v))
			}()
		case StatusDown:
			results[i] = f.applyDown(v, reqs(v))
		default:
			results[i].Skipped = true
			results[i].Err = fmt.Errorf("skipped: %s", v.Status)
		}
	}
	wg.Wait()
	for _, r := range results {
		if r.Err != nil || r.EnvErr != nil {
			f.opt.Logger.Warn("fleet change not applied everywhere", "instance", r.Instance, "err", r.Err, "env_err", r.EnvErr)
		}
	}
	return results
}

func (f *Fleet) applyUp(ctx context.Context, v View, rqs []request) Result {
	res := Result{Instance: v.Name}
	ctx, cancel := context.WithTimeout(ctx, f.opt.ApplyTimeout)
	defer cancel()
	keys := map[string]string{}
	for _, rq := range rqs {
		st, err := f.put(ctx, v.DialAddr, rq.path, rq.body)
		if err != nil {
			res.Err = err
			break
		}
		res.State = st
		res.Applied = append(res.Applied, rq.path)
		if rq.env != nil {
			maps.Copy(keys, rq.env(*st))
		}
	}
	if len(keys) > 0 {
		res.EnvErr = f.writeEnv(v.EnvPath, keys)
	}
	return res
}

// applyDown writes a down instance's env file with the requested values,
// mirroring applyUp: in order, stopping at the first invalid request, so
// the file gets exactly the keys the instance would have accepted.
func (f *Fleet) applyDown(v View, rqs []request) Result {
	res := Result{Instance: v.Name}
	keys := map[string]string{}
	for _, rq := range rqs {
		if rq.wantErr != nil {
			res.Err = fmt.Errorf("%s: %w", rq.path, rq.wantErr)
			break
		}
		if rq.want == nil {
			res.Err = fmt.Errorf("not reachable (%s)", v.Status)
			break
		}
		maps.Copy(keys, rq.want)
		res.Applied = append(res.Applied, rq.path)
	}
	if len(keys) > 0 {
		res.NextStart = true
		res.EnvErr = f.writeEnv(v.EnvPath, keys)
	}
	return res
}

func (f *Fleet) put(ctx context.Context, addr, path string, body any) (*admin.State, error) {
	resp, err := f.adminRequest(ctx, http.MethodPut, addr, path, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s: %s", path, e.Error)
		}
		return nil, fmt.Errorf("%s: status %d", path, resp.StatusCode)
	}
	var st admin.State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: decode state: %w", path, err)
	}
	return &st, nil
}

func (f *Fleet) writeEnv(path string, set map[string]string) error {
	m, _ := f.envMu.LoadOrStore(path, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return envfile.Update(path, set)
}

// Requests for each kind of change. Each validates the requested values the
// way the admin API does, so a down instance's env file only ever gets
// values the instance will accept when it starts.

func faultsRequest(rates map[string]float64) request {
	rq := request{path: "faults", body: rates, env: func(st admin.State) map[string]string {
		set, err := fault.NewSetFromRates(st.Faults)
		if err != nil {
			return nil
		}
		return map[string]string{envfile.FaultRates: set.String()}
	}}
	if set, err := fault.NewSetFromRates(rates); err != nil {
		rq.wantErr = err
	} else {
		rq.want = map[string]string{envfile.FaultRates: set.String()}
	}
	return rq
}

func profilesRequest(seed int64, rules []string) request {
	if rules == nil {
		rules = []string{}
	}
	envOf := func(seed int64, rules []string) map[string]string {
		return map[string]string{
			envfile.DeviceProfiles: strings.Join(rules, ","),
			envfile.ProfileSeed:    strconv.FormatInt(seed, 10),
		}
	}
	rq := request{
		path: "profiles",
		body: map[string]any{"seed": seed, "rules": rules},
		env:  func(st admin.State) map[string]string { return envOf(st.Profiles.Seed, st.Profiles.Rules) },
	}
	for _, r := range rules {
		if strings.Contains(r, ",") || strings.TrimSpace(r) == "" {
			rq.wantErr = fmt.Errorf("rule %q: want one profile@percent item", r)
			return rq
		}
	}
	if _, err := profile.Parse(strings.Join(rules, ",")); err != nil {
		rq.wantErr = err
		return rq
	}
	rq.want = envOf(seed, rules)
	return rq
}

func delayRequest(minMS, maxMS int64) request {
	envOf := func(lo, hi int64) map[string]string {
		return map[string]string{
			envfile.ResponseDelayMSMin: strconv.FormatInt(lo, 10),
			envfile.ResponseDelayMSMax: strconv.FormatInt(hi, 10),
		}
	}
	rq := request{
		path: "behaviour",
		body: map[string]any{"delay_ms": map[string]int64{"min": minMS, "max": maxMS}},
		env:  func(st admin.State) map[string]string { return envOf(st.DelayMS.Min, st.DelayMS.Max) },
	}
	if minMS < 0 || minMS > maxMS || maxMS > maxDelayMS {
		rq.wantErr = fmt.Errorf("delay_ms: want 0 <= min <= max <= %d", maxDelayMS)
	} else {
		rq.want = envOf(minMS, maxMS)
	}
	return rq
}

func capRequest(n int64) request {
	rq := request{
		path: "behaviour",
		body: map[string]int64{"max_sessions": n},
		env: func(st admin.State) map[string]string {
			return map[string]string{envfile.MaxConcurrentSessions: strconv.FormatInt(st.MaxSessions, 10)}
		},
	}
	if n < 1 {
		rq.wantErr = fmt.Errorf("max_sessions must be >= 1")
	} else {
		rq.want = map[string]string{envfile.MaxConcurrentSessions: strconv.FormatInt(n, 10)}
	}
	return rq
}

func activeRequest(n int) request {
	return request{
		path: "active",
		body: map[string]int{"active_devices": n},
		env: func(st admin.State) map[string]string {
			return map[string]string{envfile.ActiveDevices: strconv.Itoa(st.ActiveDevices)}
		},
		// The split never exceeds an instance's weight, which for a down
		// instance is its PORT_COUNT, the most rcfg-sim accepts.
		want: map[string]string{envfile.ActiveDevices: strconv.Itoa(n)},
	}
}

func all(rqs ...request) func(View) []request {
	return func(View) []request { return rqs }
}

// ApplyFaults sets the same fault rates on every instance.
func (f *Fleet) ApplyFaults(ctx context.Context, rates map[string]float64) []Result {
	return f.fanOut(ctx, f.Views(), all(faultsRequest(rates)))
}

// ApplyProfiles sets the same device profile rules and seed on every
// instance. Selection is by seed and hostname, so percentages stay correct
// across the whole fleet.
func (f *Fleet) ApplyProfiles(ctx context.Context, seed int64, rules []string) []Result {
	return f.fanOut(ctx, f.Views(), all(profilesRequest(seed, rules)))
}

// ApplyFailures sets faults, then profiles, as the Failures screen's Apply
// does. If an instance accepts the faults and refuses the profiles, only
// the fault keys are written to its env file.
func (f *Fleet) ApplyFailures(ctx context.Context, rates map[string]float64, seed int64, rules []string) []Result {
	return f.fanOut(ctx, f.Views(), all(faultsRequest(rates), profilesRequest(seed, rules)))
}

// ApplyDelay sets the same response delay window on every instance.
func (f *Fleet) ApplyDelay(ctx context.Context, minMS, maxMS int64) []Result {
	return f.fanOut(ctx, f.Views(), all(delayRequest(minMS, maxMS)))
}

// ApplySessionCaps sets each named instance's session cap. Instances not in
// caps are left alone.
func (f *Fleet) ApplySessionCaps(ctx context.Context, caps map[string]int64) []Result {
	var targets []View
	for _, v := range f.Views() {
		if _, ok := caps[v.Name]; ok {
			targets = append(targets, v)
		}
	}
	return f.fanOut(ctx, targets, func(v View) []request { return []request{capRequest(caps[v.Name])} })
}

// activeWeight is an instance's share weight in the active-device split:
// an up instance's device count, a down instance's PORT_COUNT from its env
// file (section 5.4). Other states take no share.
func activeWeight(v View) (int, bool) {
	switch v.Status {
	case StatusUp:
		return min(v.State.Devices, v.State.PortCount), true
	case StatusDown:
		return v.PortCount, true
	}
	return 0, false
}

// ActiveDevicesTotal returns the fleet's active-device maximum (the sum of
// the split weights) and its current total: live for up instances, the env
// file's ACTIVE_DEVICES for down ones.
func (f *Fleet) ActiveDevicesTotal() (max, active int) {
	for _, v := range f.Views() {
		w, ok := activeWeight(v)
		if !ok {
			continue
		}
		max += w
		switch {
		case v.State != nil:
			active += v.State.ActiveDevices
		case v.Desired != nil:
			active += min(v.Desired.ActiveDevices, w)
		}
	}
	return max, active
}

// ActiveSplit is how a fleet active-device target divides across the up
// and down instances, in proportion to their weights (see activeWeight).
// A down instance's share is saved to its env file for its next start.
func (f *Fleet) ActiveSplit(target int) (map[string]int, error) {
	var names []string
	var weights []int
	for _, v := range f.Views() {
		if w, ok := activeWeight(v); ok {
			names = append(names, v.Name)
			weights = append(weights, w)
		}
	}
	split, err := SplitActive(target, weights)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(names))
	for i, n := range names {
		out[n] = split[i]
	}
	return out, nil
}

// ApplyActive splits a fleet-wide active-device target across the up and
// down instances (see ActiveSplit) and applies each share; a down
// instance's goes to its env file only.
func (f *Fleet) ApplyActive(ctx context.Context, target int) ([]Result, error) {
	split, err := f.ActiveSplit(target)
	if err != nil {
		return nil, err
	}
	var targets []View
	for _, v := range f.Views() {
		if _, ok := split[v.Name]; ok {
			targets = append(targets, v)
		}
	}
	return f.fanOut(ctx, targets, func(v View) []request { return []request{activeRequest(split[v.Name])} }), nil
}

// Reapply pushes an up instance's env file settings to its live config,
// ending drift. The env file is the source of truth and is not written.
func (f *Fleet) Reapply(ctx context.Context, name string) Result {
	v, ok := f.View(name)
	if !ok {
		return Result{Instance: name, Err: fmt.Errorf("no instance %q", name)}
	}
	if v.Status != StatusUp {
		return Result{Instance: name, Skipped: true, Err: fmt.Errorf("not reachable (%s)", v.Status)}
	}
	if v.Desired == nil {
		_, err := DesiredFromEnv(v.Instance)
		return Result{Instance: name, Err: fmt.Errorf("env file: %w", err)}
	}
	d := *v.Desired
	rqs := []request{
		{path: "faults", body: d.Faults},
		{path: "profiles", body: map[string]any{"seed": d.ProfileSeed, "rules": d.ProfileRules}},
		{path: "behaviour", body: map[string]any{"delay_ms": map[string]int64{"min": d.DelayMinMS, "max": d.DelayMaxMS}, "max_sessions": d.MaxSessions}},
		{path: "active", body: map[string]int{"active_devices": d.ActiveDevices}},
	}
	return f.fanOut(ctx, []View{v}, all(rqs...))[0]
}
