package configs

import (
	"fmt"
	"math/rand"
	"strings"
)

// Generator models for the non-Ciena TL1 optical platforms.
//
// Both are gateway models: one addressable node whose config also carries the inventories of
// the elements reachable through it, delimited by a per-vendor marker that the matching driver
// splits on at session start. The shape mirrors ciena-6500-tl1-gne; only the marker, the
// element naming and the inventory text differ.

const infineraModelName = "infinera-dtnx-tl1"

const ciscoONSModelName = "cisco-ons15454-tl1"

func infineraModel() model {
	return model{
		name:     infineraModelName,
		vendor:   "Infinera",
		template: "infinera_tl1",
		tmplFile: "infinera_tl1_node.tmpl",
		build:    func(cfg Config, index int, m model) any { return buildInfineraNode(cfg, index) },
	}
}

func ciscoONSModel() model {
	return model{
		name:     ciscoONSModelName,
		vendor:   "Cisco",
		template: "cisco_ons_tl1",
		tmplFile: "cisco_ons_tl1_gne.tmpl",
		build:    func(cfg Config, index int, m model) any { return buildCiscoONSGNE(cfg, index) },
	}
}

// OpticalNodeData is one node's inventory: the payload for both vendors' templates.
type OpticalNodeData struct {
	SID       string
	NodeIP    string
	Serial    string
	SwVersion string
	Slots     []OpticalSlot
}

// OpticalSlot is one populated slot in a shelf or chassis.
type OpticalSlot struct {
	Slot     int
	CardType string
	Serial   string
	State    string
}

// InfineraNodeData is the payload for templates/infinera_tl1_node.tmpl: the node's own
// inventory plus the nodes reachable through it.
type InfineraNodeData struct {
	OpticalNodeData
	Remotes []OpticalNodeData
}

// CiscoONSGNEData is the payload for templates/cisco_ons_tl1_gne.tmpl: the gateway's own
// inventory plus the end NEs behind it. Cisco calls them ENEs, not RNEs.
type CiscoONSGNEData struct {
	OpticalNodeData
	ENEs []OpticalNodeData
}

// buildInfineraNode builds a DTN-X node fronting 12 to 30 remote nodes.
//
// The count is deliberately larger than the Ciena model's 2 to 5: RTRV-TIDMAP pages its
// response every 10 records, so a node with only a handful of neighbours would answer in a
// single block and never exercise the paging path that this vendor exists to reproduce.
func buildInfineraNode(cfg Config, index int) InfineraNodeData {
	rng := deviceRand(cfg.Seed, index)
	local := buildOpticalNode(cfg, index, rng, "INF", 8)

	nRemote := 12 + rng.Intn(19) // 12..30
	used := map[string]bool{}
	var remotes []OpticalNodeData
	for i := 0; i < nRemote; i++ {
		tid := infineraTID(rng)
		if used[tid] {
			continue
		}
		used[tid] = true

		r := buildOpticalNode(cfg, index, rng, "INF", 8)
		r.SID = tid
		r.Serial = serialFor(tid)
		remotes = append(remotes, r)
	}

	return InfineraNodeData{OpticalNodeData: local, Remotes: remotes}
}

// buildCiscoONSGNE builds an ONS 15454 gateway fronting 3 to 8 end NEs.
func buildCiscoONSGNE(cfg Config, index int) CiscoONSGNEData {
	rng := deviceRand(cfg.Seed, index)
	local := buildOpticalNode(cfg, index, rng, "ONS", 12)

	nENE := 3 + rng.Intn(6) // 3..8
	used := map[string]bool{}
	var enes []OpticalNodeData
	for i := 0; i < nENE; i++ {
		tid := ciscoONSTID(rng)
		if used[tid] {
			continue
		}
		used[tid] = true

		e := buildOpticalNode(cfg, index, rng, "ONS", 12)
		e.SID = tid
		e.Serial = serialFor(tid)
		enes = append(enes, e)
	}

	return CiscoONSGNEData{OpticalNodeData: local, ENEs: enes}
}

// infineraTID builds a DTN-X style TID in the shape seen in the customer's RTRV-TIDMAP output
// (e.g. CSVLTNFCO1Y): a site code, then TN, then a short suffix. Opaque and alphanumeric, unlike
// Ciena's readable RNE-<CITY>, which is the point: a client must not assume TIDs are legible.
func infineraTID(rng *rand.Rand) string {
	city := strings.ToUpper(citySyllables[rng.Intn(len(citySyllables))])

	return fmt.Sprintf("%sTN%s%dY", city, string(rune('A'+rng.Intn(26))), rng.Intn(10))
}

// ciscoONSTID builds an ONS style node name, as seen in the vendor documentation (TID-000).
func ciscoONSTID(rng *rand.Rand) string {
	return fmt.Sprintf("TID-%03d", rng.Intn(1000))
}

// buildOpticalNode builds one node's inventory from the supplied rng, so a whole gateway and
// its elements come off a single deterministic stream.
func buildOpticalNode(cfg Config, index int, rng *rand.Rand, prefix string, slots int) OpticalNodeData {
	city := strings.ToUpper(citySyllables[rng.Intn(len(citySyllables))])
	sid := fmt.Sprintf("%s-%s-%04d", prefix, city, 1000+(index%9000))

	d := OpticalNodeData{
		SID:       sid,
		NodeIP:    ipPlusOffset(cfg.IPBase, index/cfg.DevicesPerIP),
		Serial:    serialFor(sid),
		SwVersion: opticalSwVersion(prefix),
	}

	nEquipped := slots/2 + rng.Intn(slots/2+1)
	for s := 1; s <= nEquipped; s++ {
		card := OpticalSlot{
			Slot:     s,
			CardType: opticalCards[rng.Intn(len(opticalCards))],
			Serial:   cienaCardSerial(rng, s),
			State:    "IS-NR",
		}
		if rng.Intn(20) == 0 {
			card.State = "OOS-AU"
		}
		d.Slots = append(d.Slots, card)
	}

	return d
}

func opticalSwVersion(prefix string) string {
	if prefix == "ONS" {
		return "09.10-005E-16.28"
	}

	return "R21.3"
}

var opticalCards = []string{"OTR4", "XT500", "AOFX", "OCG", "TAM", "TOM", "BMM", "MXP", "TCC2P", "OSCM"}
