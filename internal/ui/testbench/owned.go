package testbench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/envfile"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/fleet"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
)

// scenarioOwned are the env keys a scenario testbench takes over and the
// next generated testbench gives back (Phase 9 item 4).
var scenarioOwned = []string{envfile.CommandsRoot, envfile.Password, envfile.EnablePassword}

// ownedSnapshot is one instance's scenarioOwned keys as they were before a
// scenario took them over, kept in --data-dir/owned-keys/<IP>.json.
type ownedSnapshot struct {
	Instance string             `json:"instance"`
	Keys     map[string]*string `json:"keys"` // null: not set in the env file (the unit default applied)
}

func (m *Manager) ownedDir() string { return filepath.Join(m.opt.DataDir, "owned-keys") }

// saveOwned records the scenarioOwned keys of each named instance before a
// scenario writes them. An instance that already has a snapshot keeps it:
// a scenario after a scenario must not save the first one's values.
func (m *Manager) saveOwned(j *jobs.Job, envDir string, names []string) error {
	insts, err := fleet.Discover(envDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.ownedDir(), 0o750); err != nil {
		return err
	}
	var saved []string
	for _, in := range insts {
		p := filepath.Join(m.ownedDir(), in.Name+".json")
		if !slices.Contains(names, in.Name) {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			continue
		}
		f, err := envfile.Read(in.EnvPath)
		if err != nil {
			return fmt.Errorf("%s: %w", in.EnvPath, err)
		}
		snap := ownedSnapshot{Instance: in.Name, Keys: map[string]*string{}}
		for _, k := range scenarioOwned {
			snap.Keys[k] = nil
			if v, ok := f.Values[k]; ok {
				snap.Keys[k] = &v
			}
		}
		b, _ := json.MarshalIndent(snap, "", "  ")
		if err := writeAtomic(p, append(b, '\n')); err != nil {
			return fmt.Errorf("save %s: %w", p, err)
		}
		saved = append(saved, in.Name)
	}
	if len(saved) > 0 {
		j.Logf("Saved %s on %s, to put back when a generated testbench starts", strings.Join(scenarioOwned, ", "), strings.Join(saved, ", "))
	}
	return nil
}

// restoreOwned puts back every saved snapshot of the named instances,
// exactly as saved (a key that was not set is removed again), deletes it,
// and returns the instances whose values changed, which need a restart.
// With no snapshots (a generated testbench after a generated one) it
// touches nothing.
func (m *Manager) restoreOwned(j *jobs.Job, names []string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(m.ownedDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, p := range files {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		if !slices.Contains(names, name) {
			continue // not on this host any more; kept for when it is
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return changed, err
		}
		var snap ownedSnapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			return changed, fmt.Errorf("%s: %w", p, err)
		}
		for k := range snap.Keys {
			if !slices.Contains(scenarioOwned, k) {
				return changed, fmt.Errorf("%s: %s is not a key a scenario takes over", p, k)
			}
		}
		c, err := m.opt.Fleet.RestoreKeys(name, snap.Keys)
		if err != nil {
			return changed, fmt.Errorf("restore %s: %w", name, err)
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, err
		}
		j.Logf("Put back %s on %s as they were before the scenario", strings.Join(scenarioOwned, ", "), name)
		if c {
			changed = append(changed, name)
		}
	}
	return changed, nil
}
