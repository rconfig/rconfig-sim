package sshsrv

import (
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/drivers"
)

// TestDriverMetadata keeps internal/drivers (what the generator's
// catalogue prints) and the registered drivers in step: the same ids, and
// each driver's RequiresSSHAuth is its metadata's.
func TestDriverMetadata(t *testing.T) {
	for name, d := range driverRegistry {
		meta, ok := drivers.Lookup(name)
		if !ok {
			t.Errorf("driver %q is registered but has no metadata in internal/drivers", name)
			continue
		}
		if d.Name() != meta.ID || d.RequiresSSHAuth() != meta.RequiresSSHAuth {
			t.Errorf("driver %q: name %q, RequiresSSHAuth %v; metadata %+v", name, d.Name(), d.RequiresSSHAuth(), meta)
		}
	}
	for _, meta := range drivers.All {
		if _, ok := driverRegistry[meta.ID]; !ok {
			t.Errorf("internal/drivers lists %q, but no such driver is registered", meta.ID)
		}
	}
}
