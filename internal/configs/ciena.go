package configs

import (
	"crypto/sha256"
	"encoding/binary"
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
//
// With cfg.RNEDualHomePct > 0 some RNEs are drawn from a fleet-wide shared pool
// instead of being private to this GNE, so the same RNE turns up behind several
// gateways. A shared RNE's shelf is derived from its TID rather than from this
// GNE's stream, which is what makes two gateways emit byte-identical sections for
// it without the generator workers having to coordinate. At the default of 0 no
// draw is taken at all, so output stays byte-identical to a build without this.
//
// Dual-homing also links each gateway to its neighbours by a guaranteed shared
// element (see linkRnes), so any two adjacent devices in the fleet always have a
// dual-homed RNE between them rather than only when the random draw happens to
// collide.
func buildCienaGNE(cfg Config, index int) CienaGNEData {
	rng := deviceRand(cfg.Seed, index)
	gne := buildCienaShelf(cfg, index, rng)

	nRNE := 2 + rng.Intn(4) // 2..5
	used := map[string]bool{}
	var rnes []CienaEqptData

	// Links to the neighbouring gateways come first and count towards this GNE's
	// total, so enabling dual-homing re-mixes which elements sit behind a gateway
	// rather than growing every gateway past the documented 2-5.
	for _, tid := range linkRnes(cfg, index) {
		used[tid] = true
		rnes = append(rnes, buildSharedRneShelf(cfg, tid))
	}

	for i := len(rnes); i < nRNE; i++ {
		city := strings.ToUpper(citySyllables[rng.Intn(len(citySyllables))])

		shared := false
		if cfg.RNEDualHomePct > 0 {
			shared = rng.Intn(100) < cfg.RNEDualHomePct
		}

		tid := rneTID(cfg, city, index, shared)
		if used[tid] {
			continue // dedupe RNE TID collisions within one GNE
		}
		used[tid] = true

		if shared {
			// Identical through every gateway that fronts it, so rConfig sees one device.
			rnes = append(rnes, buildSharedRneShelf(cfg, tid))

			continue
		}

		r := buildCienaShelf(cfg, index, rng)
		r.SID = tid
		r.ShelfSerial = serialFor(tid)
		r.NodeIP = ipPlusOffset(cfg.IPBase, index/cfg.DevicesPerIP)
		rnes = append(rnes, r)
	}
	return CienaGNEData{CienaEqptData: gne, RNEs: rnes}
}

// rneTID names a remote NE.
//
// A shared RNE keeps the bare "RNE-<CITY>" form so two gateways drawing the same
// city name the same element. A private one carries its gateway's index, which
// stops two gateways colliding on a city by chance and presenting unrelated
// equipment under one name. Without dual-homing enabled the historic bare form is
// kept for every RNE so existing fleets regenerate unchanged.
//
// The suffix stays alphanumeric: TIDs are matched as RNE-[A-Z0-9]+ on the wire.
func rneTID(cfg Config, city string, index int, shared bool) string {
	if shared || cfg.RNEDualHomePct == 0 {
		return "RNE-" + city
	}

	return fmt.Sprintf("RNE-%s%04d", city, index%10000)
}

// linkRnes returns the TIDs of the elements this gateway shares with its immediate
// neighbours in the fleet.
//
// Optical networks are built as chains and rings: an RNE sits between two gateways
// and is reachable through either, which is what makes losing one survivable. The
// link between gateway N-1 and N is named for that pair, so both devices derive the
// same TID independently — device N claims the link to N-1 and the link to N+1, and
// its neighbours claim the same two from their side. No coordination between
// generator workers, and every adjacent pair is guaranteed to share an element
// rather than only doing so when a random draw collides.
//
// Empty unless dual-homing is enabled, so default output is untouched.
func linkRnes(cfg Config, index int) []string {
	if cfg.RNEDualHomePct <= 0 {
		return nil
	}

	var tids []string
	if index > 0 {
		tids = append(tids, linkTID(index-1))
	}
	// The last gateway in the fleet has no successor, so it claims no forward link -
	// an element with only one gateway would not be dual-homed at all.
	if index+1 < cfg.Count {
		tids = append(tids, linkTID(index))
	}

	return tids
}

// linkTID names the element shared between gateway n and gateway n+1.
func linkTID(n int) string {
	return fmt.Sprintf("RNE-LINK%04d", n%10000)
}

// buildSharedRneShelf builds a remote NE entirely from its TID, so every gateway
// that fronts it renders the same bytes. Nothing about the GNE leaks in: the
// shelf contents, serial and management IP are all TID-derived.
func buildSharedRneShelf(cfg Config, tid string) CienaEqptData {
	seedIndex := tidSeedIndex(tid)

	r := buildCienaShelf(cfg, seedIndex, deviceRand(cfg.Seed, seedIndex))
	r.SID = tid
	r.ShelfSerial = serialFor(tid)
	r.NodeIP = ipPlusOffset(cfg.IPBase, seedIndex%256)

	return r
}

// tidSeedIndex turns a TID into the stable index its shelf is generated from.
func tidSeedIndex(tid string) int {
	sum := sha256.Sum256([]byte(tid))

	return int(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
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
