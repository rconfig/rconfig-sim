package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestImportBoundary enforces D7 (docs/UI-ARCHITECTURE.md): rcfg-sim and
// rcfg-sim-gen never link the web UI or its dependencies. Only
// cmd/rcfg-sim-ui and internal/ui/... may import them.
func TestImportBoundary(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	for _, cmd := range []string{
		"github.com/rcfg-sim/rcfg-sim/cmd/rcfg-sim",
		"github.com/rcfg-sim/rcfg-sim/cmd/rcfg-sim-gen",
	} {
		out, err := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("go list %s: %v\n%s", cmd, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			if strings.HasPrefix(dep, "github.com/rcfg-sim/rcfg-sim/internal/ui") ||
				strings.HasPrefix(dep, "github.com/a-h/templ") {
				t.Errorf("%s depends on %s; UI packages must stay out of the simulator binaries", cmd, dep)
			}
		}
	}

	// Control: the UI binary does pull in templ, so the check above can see it.
	out, err := exec.Command(goBin, "list", "-deps", "-f", "{{.ImportPath}}", "github.com/rcfg-sim/rcfg-sim/cmd/rcfg-sim-ui").CombinedOutput()
	if err != nil {
		t.Fatalf("go list rcfg-sim-ui: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "github.com/a-h/templ") {
		t.Fatal("control failed: rcfg-sim-ui should depend on templ")
	}
}
