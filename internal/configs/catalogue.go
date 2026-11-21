package configs

import (
	"encoding/json"
	"io"

	"github.com/rcfg-sim/rcfg-sim/internal/drivers"
)

// Family is a device family (D13): ip or optical.
type Family struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ModelOption is a generator option that applies to a model, with what a
// form needs to offer it.
type ModelOption struct {
	Name    string `json:"name"` // testbench key, e.g. rne_dual_home_pct
	Flag    string `json:"flag"` // rcfg-sim-gen flag, e.g. --rne-dual-home-pct
	Type    string `json:"type"` // int
	Min     int    `json:"min"`
	Max     int    `json:"max"`
	Default int    `json:"default"`
	Help    string `json:"help"` // one sentence
}

// CatalogueModel is one generator model as the catalogue lists it.
type CatalogueModel struct {
	ID          string        `json:"id"`
	Family      string        `json:"family"`
	Vendor      string        `json:"vendor"`
	Driver      string        `json:"driver"`
	Protocol    string        `json:"protocol"` // cli or tl1, from the driver
	Description string        `json:"description"`
	AvgBytes    int64         `json:"avg_bytes"`
	Options     []ModelOption `json:"options"`
}

// Catalogue is everything a front end needs to offer the generator's
// models: families, models with their options, and drivers.
type Catalogue struct {
	Families []Family         `json:"families"`
	Models   []CatalogueModel `json:"models"`
	Drivers  []drivers.Driver `json:"drivers"`
}

// GetCatalogue builds the catalogue from the model registry and the driver
// metadata. Models are in canonical order (modelOrder).
func GetCatalogue() Catalogue {
	c := Catalogue{Families: append([]Family(nil), families...), Drivers: append([]drivers.Driver(nil), drivers.All...)}
	for _, name := range modelOrder {
		m := registry[name]
		d, _ := drivers.Lookup(m.template)
		opts := m.options
		if opts == nil {
			opts = []ModelOption{}
		}
		c.Models = append(c.Models, CatalogueModel{ID: name, Family: m.family, Vendor: m.vendor, Driver: m.template,
			Protocol: d.Protocol, Description: m.description, AvgBytes: m.avgBytes, Options: opts})
	}
	return c
}

// WriteCatalogueJSON writes the catalogue as indented JSON.
func WriteCatalogueJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(GetCatalogue())
}
