package configs

import (
	"fmt"
	"math/rand"
	"strings"
)

// cienaModelName is the public model name: used in --distribution, written to
// the manifest size_bucket column, and documented as API. Vendor/model/protocol
// so future 6500 form factors (ciena-6500-2slot, …) slot in alongside it.
const cienaModelName = "ciena-6500-tl1"

// cienaGNEModelName is a Ciena 6500 acting as a Gateway NE (GNE) that fronts
// several Remote NEs (RNEs). The generated config carries the GNE's own shelf
// inventory plus each RNE's, which the ciena_tl1 driver routes to by TID.
const cienaGNEModelName = "ciena-6500-tl1-gne"

// modelHostname extracts the manifest hostname from a model's rendered data.
// Each vendor's data struct names this field differently (Cisco Hostname,
// Ciena SID); the manifest hostname becomes the device's runtime SID/TID.
func modelHostname(data any) string {
	switch d := data.(type) {
	case TemplateData:
		return d.Hostname
	case CienaEqptData:
		return d.SID
	case CienaGNEData:
		return d.SID // the GNE is the SSH-addressable node; RNEs are not manifest rows
	default:
		return ""
	}
}

// cienaModel returns the registry entry for the Ciena 6500 7-slot optical. Its
// rendered config file is a TL1 RTRV-EQPT::ALL inventory payload, mmap-streamed
// at runtime by the ciena_tl1 driver; the other RTRV-* responses are synthesized
// live by that driver.
func cienaModel() model {
	return model{
		name:     cienaModelName,
		vendor:   "Ciena",
		template: "ciena_tl1",
		tmplFile: "ciena_tl1_eqpt.tmpl",
		build:    func(cfg Config, index int, m model) any { return buildCienaEqpt(cfg, index) },
	}
}

// cienaGNEModel returns the registry entry for a Ciena 6500 GNE. Same runtime
// driver (ciena_tl1) as the standalone model; the GNE template additionally
// emits per-RNE inventory sections that the driver routes to by TID.
func cienaGNEModel() model {
	return model{
		name:     cienaGNEModelName,
		vendor:   "Ciena",
		template: "ciena_tl1",
		tmplFile: "ciena_tl1_gne.tmpl",
		build:    func(cfg Config, index int, m model) any { return buildCienaGNE(cfg, index) },
	}
}

// CienaGNEData is the payload for templates/ciena_tl1_gne.tmpl: the GNE's own
// shelf (the embedded CienaEqptData) plus the shelves of the RNEs reachable
// through it (each a CienaEqptData whose SID is the RNE's TID). Deterministic
// via the same deviceRand stream as the standalone builder.
type CienaGNEData struct {
	CienaEqptData
	RNEs []CienaEqptData
}

// buildCienaGNE builds a GNE with 2–5 RNEs behind it. The GNE shelf is the same
// shape as a standalone node; each RNE is a full shelf re-identified with an
// RNE-<CITY> TID. All randomness flows from deviceRand(seed, index) so output is
// byte-reproducible.
func buildCienaGNE(cfg Config, index int) CienaGNEData {
	rng := deviceRand(cfg.Seed, index)
	gne := buildCienaShelf(cfg, index, rng)

	nRNE := 2 + rng.Intn(4) // 2..5
	used := map[string]bool{}
	var rnes []CienaEqptData
	for i := 0; i < nRNE; i++ {
		tid := "RNE-" + strings.ToUpper(citySyllables[rng.Intn(len(citySyllables))])
		if used[tid] {
			continue // dedupe RNE TID collisions within one GNE
		}
		used[tid] = true
		r := buildCienaShelf(cfg, index, rng)
		r.SID = tid
		r.ShelfSerial = serialFor(tid)
		r.NodeIP = ipPlusOffset(cfg.IPBase, index/cfg.DevicesPerIP)
		rnes = append(rnes, r)
	}
	return CienaGNEData{CienaEqptData: gne, RNEs: rnes}
}

// CienaEqptData is the payload for templates/ciena_tl1_eqpt.tmpl: the equipment
// inventory of a 6500 7-slot shelf. Fully determined by (seed, index) via
// deviceRand, so a fixed seed yields byte-identical output. Disjoint from the
// Cisco TemplateData so vendor data shapes never cross-couple.
type CienaEqptData struct {
	SID         string
	NodeIP      string
	ShelfSerial string
	SwVersion   string
	Slots       []CienaSlot
}

type CienaSlot struct {
	Slot     int
	Equipped bool
	CardType string
	CLEI     string
	Serial   string
	PartNum  string
	State    string
	Ports    []CienaPort
}

type CienaPort struct {
	Port       int
	Equipped   bool
	OpticType  string
	Wavelength string
	Serial     string
	State      string
}

// Fixed catalogs — a 6500 7-slot draws cards/optics from a bounded set, so the
// inventory looks realistic without unbounded cardinality.
var (
	cienaCards  = []string{"WL3N", "OTR2", "EDFA", "10X10G", "OSC", "SP2", "XCIF"}
	cienaOptics = []string{"SFP+", "QSFP28", "CFP2"}
)

// buildCienaEqpt builds one standalone shelf with its own deterministic rng.
func buildCienaEqpt(cfg Config, index int) CienaEqptData {
	return buildCienaShelf(cfg, index, deviceRand(cfg.Seed, index))
}

// buildCienaShelf builds one 6500 shelf, drawing from the supplied rng. Taking
// the rng as a parameter lets a GNE build its own shelf plus several RNE shelves
// off a single deterministic stream.
func buildCienaShelf(cfg Config, index int, rng *rand.Rand) CienaEqptData {
	city := citySyllables[rng.Intn(len(citySyllables))]
	sid := fmt.Sprintf("CIENA-%s-%04d", strings.ToUpper(city), 1000+(index%9000))

	d := CienaEqptData{
		SID:         sid,
		NodeIP:      ipPlusOffset(cfg.IPBase, index/cfg.DevicesPerIP),
		ShelfSerial: serialFor(sid),
		SwVersion:   "12.4",
	}

	nEquipped := 4 + rng.Intn(4) // 4..7 of the 7 payload slots populated
	for s := 1; s <= 7; s++ {
		slot := CienaSlot{Slot: s, Equipped: s <= nEquipped}
		if slot.Equipped {
			slot.CardType = cienaCards[rng.Intn(len(cienaCards))]
			slot.CLEI = cienaCLEI(rng)
			slot.Serial = cienaCardSerial(rng, s)
			slot.PartNum = fmt.Sprintf("NTK%03d%s", 500+rng.Intn(99), string(rune('A'+rng.Intn(26))))
			slot.State = "IS-NR"
			if rng.Intn(20) == 0 {
				slot.State = "OOS-AU"
			}
			nPorts := 2 + rng.Intn(7) // 2..8 ports per card
			for p := 1; p <= nPorts; p++ {
				port := CienaPort{Port: p, Equipped: rng.Intn(4) != 0}
				if port.Equipped {
					port.OpticType = cienaOptics[rng.Intn(len(cienaOptics))]
					port.Wavelength = cienaLambda(rng)
					port.Serial = cienaCardSerial(rng, s*100+p)
					port.State = "IS-NR"
				}
				slot.Ports = append(slot.Ports, port)
			}
		}
		d.Slots = append(d.Slots, slot)
	}
	return d
}

func cienaCLEI(rng *rand.Rand) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b strings.Builder
	b.WriteString("WMOT")
	for i := 0; i < 6; i++ {
		b.WriteByte(alpha[rng.Intn(len(alpha))])
	}
	return b.String()
}

func cienaCardSerial(rng *rand.Rand, slot int) string {
	const alpha = "ABCDEFGHJKLMNPQRSTUVWXYZ0123456789"
	var b strings.Builder
	b.WriteString("LBC")
	for i := 0; i < 7; i++ {
		b.WriteByte(alpha[rng.Intn(len(alpha))])
	}
	return fmt.Sprintf("%s%02d", b.String(), slot%100)
}

// cienaLambda returns an ITU C-band channel centre frequency / wavelength.
func cienaLambda(rng *rand.Rand) string {
	// C-band ~191.0..196.0 THz; render as nm for readability.
	nm := 1528.0 + float64(rng.Intn(400))*0.05
	return fmt.Sprintf("%.2f", nm)
}
