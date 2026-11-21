// Package scenarios embeds the shipped scenario packages (docs/UI-ARCHITECTURE.md,
// Phase 9) so rcfg-sim-ui can list and install them without the source
// tree. Each package is a folder with scenario.json, manifest.csv,
// configs/ and optional commands/. The example folder is the
// --commands-root layout example, not a scenario, and is not embedded.
package scenarios

import "embed"

// Shipped holds the shipped packages, one folder each.
//
//go:embed mesh-campus
var Shipped embed.FS
