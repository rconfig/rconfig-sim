package configs

import (
	"fmt"
	"math/rand"
	"strings"
)

// buildDeviceData assembles every TemplateData field for a single device.
// It is deterministic: same (cfg.Seed, index, bucket) ⇒ same output.
func buildDeviceData(cfg Config, index int, bucket string) TemplateData {
	rng := deviceRand(cfg.Seed, index)
	prof := profiles[bucket]

	siteIdx := rng.Intn(200)
	site := siteCode(siteIdx)
	siteFunc := siteFunctions[rng.Intn(len(siteFunctions))]
	city := citySyllables[rng.Intn(len(citySyllables))]

	hostPrefix := "rtr"
	if prof.deviceKind == "switch" {
		hostPrefix = "sw"
	}
	hostname := fmt.Sprintf("%s-%s-%s-%04d", hostPrefix, city, siteFunc, 1000+(index%9000))

	mgmtIP := ipPlusOffset(cfg.IPBase, index/cfg.DevicesPerIP)

	d := TemplateData{
		Hostname:        hostname,
		SizeBucket:      bucket,
		DeviceKind:      prof.deviceKind,
		Site:            site,
		MgmtIP:          mgmtIP,
		DomainName:      fmt.Sprintf("%s.%s.example.net", site, siteFunc),
		Clock:           "10:23:45.123 UTC Mon Mar 10 2026",
		FileSizeHint:    prof.fileSizeHint,
		SerialNumber:    serialFor(hostname),
		NameServers:     randomIPs(rng, 2, randomMgmtIP),
		EnableSecret:    pseudoCiscoMD5(rng),
		SNMPCommunityRO: snmpCommunity(rng),
		SNMPCommunityRW: snmpCommunity(rng),
		SNMPLocation: fmt.Sprintf("%s / bld-%02d / floor-%02d / rack-%02d",
			strings.ToUpper(city), 1+rng.Intn(20), 1+rng.Intn(12), 1+rng.Intn(60)),
		SNMPContact:   "NetOps <netops@example.net>",
		LoggingHosts:  randomIPs(rng, 2, randomMgmtIP),
		NTPServers:    randomIPs(rng, 3, randomMgmtIP),
		BannerMOTD:    bannerLines(rng, hostname, siteFunc),
		TACACSServers: randomIPs(rng, 2, randomMgmtIP),
		TACACSKey:     pseudoCiscoMD5(rng),
		LocalASN:      64500 + rng.Intn(512),
		IncludeBGP:    prof.hasBGP,
		IncludeOSPF:   prof.hasOSPF,
		IncludeCrypto: prof.hasCrypto,
		IncludeQoS:    prof.hasQoS,
		IncludeVRF:    prof.hasVRF,
	}

	d.Users = buildUsers(rng, 1+rng.Intn(5), cfg.Username)
	d.VLANs = buildVLANs(rng, prof.vlanCount)
	d.VRFs = buildVRFs(rng, prof.vrfCount)
	d.Interfaces = buildInterfaces(rng, prof, d.VLANs, d.VRFs)
	d.Subinterfaces = buildSubinterfaces(rng, prof, d.VRFs)
	d.ACLs = buildACLs(rng, prof)
	if prof.hasOSPF {
		d.OSPF = buildOSPF(rng, prof)
	}
	if prof.hasBGP {
		d.BGP = buildBGP(rng, prof, d.LocalASN)
	}
	d.StaticRoutes = buildStaticRoutes(rng, prof, d.VRFs)
	d.PrefixLists = buildPrefixLists(rng, prof)
	d.RouteMaps = buildRouteMaps(rng, prof)
	if prof.hasCrypto {
		d.CryptoMaps = buildCryptoMaps(rng, prof)
	}
	if prof.hasQoS {
		d.QoSClassMaps, d.QoSPolicyMaps = buildQoS(rng, prof)
	}

	return d
}

func bannerLines(rng *rand.Rand, hostname, siteFunc string) []string {
	return []string{
		"****************************************************************",
		"*                                                              *",
		fmt.Sprintf("*   %s  (%s)", hostname, strings.ToUpper(siteFunc)),
		"*   AUTHORISED ACCESS ONLY                                     *",
		"*   All activity is logged. Disconnect if not authorised.      *",
		"*   Contact: netops@example.net                                *",
		"*                                                              *",
		"****************************************************************",
	}
}

func buildUsers(rng *rand.Rand, n int, primary string) []User {
	out := make([]User, 0, n+1)
	out = append(out, User{Name: primary, Privilege: 15, SecretHash: pseudoCiscoMD5(rng)})
	roleNames := []string{"netops", "readonly", "support", "audit", "helpdesk", "monitor"}
	seen := map[string]bool{primary: true}
	for i := 0; i < n; i++ {
		name := roleNames[rng.Intn(len(roleNames))]
		if seen[name] {
			name = fmt.Sprintf("%s%d", name, 1+rng.Intn(9))
		}
		seen[name] = true
		priv := 1
		if rng.Intn(3) == 0 {
			priv = 15
		}
		out = append(out, User{Name: name, Privilege: priv, SecretHash: pseudoCiscoMD5(rng)})
	}
	return out
}

func buildVLANs(rng *rand.Rand, n int) []VLAN {
	if n == 0 {
		return nil
	}
	out := make([]VLAN, 0, n)
	used := map[int]bool{1: true}
	roles := []string{"DATA", "VOICE", "MGMT", "GUEST", "IOT", "PRINTERS", "CAMERAS",
		"LAB", "OPS", "SERVERS", "DMZ", "STORAGE", "BACKUP", "WIRELESS", "KIOSK",
		"PAYMENTS", "POS", "HVAC", "SECURITY", "MEDIA", "VIDEO", "VOIP", "CORP",
		"SCIENCE", "TRANSIT", "TRANSIT-A", "TRANSIT-B", "CUST-A", "CUST-B", "CUST-C",
		"UPLINK", "PEER", "INTERNAL", "EXTERNAL", "FILTER", "ROUTER", "HONEYPOT",
		"PRINT", "SCAN", "SENSORS", "INFRA"}
	for i := 0; i < n; i++ {
		var id int
		for {
			id = 10 + rng.Intn(3990)
			if !used[id] {
				used[id] = true
				break
			}
		}
		role := roles[(i+rng.Intn(len(roles)))%len(roles)]
		out = append(out, VLAN{ID: id, Name: fmt.Sprintf("%s-%04d", role, id)})
	}
	return out
}

func buildVRFs(rng *rand.Rand, n int) []VRF {
	if n == 0 {
		return nil
	}
	asn := 65000 + rng.Intn(500)
	out := make([]VRF, 0, n)
	labels := []string{"MGMT", "INTERNET", "SHARED", "GUEST", "DMZ", "STORAGE", "IOT", "VOICE",
		"TRANSIT", "PEER", "CUST-A", "CUST-B", "CUST-C", "CUST-D", "LAB",
		"PROD", "DEV", "QA", "STAGE", "PCI", "HIPAA", "OOB", "LEGAL",
		"FINANCE", "HR", "ENG", "SALES", "NOC", "SOC", "BACKUP"}
	for i := 0; i < n; i++ {
		name := labels[i%len(labels)]
		if i >= len(labels) {
			name = fmt.Sprintf("%s-%d", name, i/len(labels))
		}
		rd := fmt.Sprintf("%d:%d", asn, 100+i)
		rt := fmt.Sprintf("%d:%d", asn, 100+i)
		out = append(out, VRF{
			Name:     name,
			RD:       rd,
			RTImport: []string{rt},
			RTExport: []string{rt},
		})
	}
	return out
}

func buildInterfaces(rng *rand.Rand, prof profile, vlans []VLAN, vrfs []VRF) []Interface {
	out := make([]Interface, 0, prof.interfaceCount+3)

	// Loopback0 always
	loopIP := randomMgmtIP(rng)
	out = append(out, Interface{
		Name:        "Loopback0",
		Description: "Router-ID / management loopback",
		Mode:        "routed",
		IPAddress:   loopIP,
		Netmask:     "255.255.255.255",
	})
	// Mgmt interface if router
	if prof.deviceKind == "router" {
		out = append(out, Interface{
			Name:        "GigabitEthernet0/0",
			Description: "OOB MGMT",
			Mode:        "routed",
			IPAddress:   randomMgmtIP(rng),
			Netmask:     "255.255.255.0",
			VRF:         "MGMT",
		})
	}

	// Data-plane interfaces.
	namePrefix := "GigabitEthernet"
	slotMajor, slotMinor := 1, 0
	if prof.deviceKind == "switch" {
		namePrefix = "GigabitEthernet"
		slotMajor = 0
		slotMinor = 1
	}
	for i := 0; i < prof.interfaceCount; i++ {
		name := fmt.Sprintf("%s%d/%d", namePrefix, slotMajor, slotMinor+i)
		if prof.deviceKind == "router" {
			name = fmt.Sprintf("%s%d/%d/%d", namePrefix, slotMajor, i/12, i%12)
		}
		intf := Interface{
			Name: name,
			MTU:  1500,
		}
		if prof.deviceKind == "switch" {
			// majority access, some trunk
			if len(vlans) == 0 {
				intf.Mode = "access"
				intf.VLAN = 1
			} else if rng.Intn(12) == 0 {
				intf.Mode = "trunk"
				intf.VLAN = vlans[0].ID
				intf.AllowedVLANsStr = allowedVLANsString(vlans, rng)
			} else {
				intf.Mode = "access"
				intf.VLAN = vlans[rng.Intn(len(vlans))].ID
				intf.Portfast = true
				intf.BPDUGuard = true
			}
			intf.Description = fmt.Sprintf("%s port %d // user-%04d-%s-%s / drop-%s%02d-port-%02d",
				strings.ToUpper(intf.Mode), i, 1+rng.Intn(9999),
				citySyllables[rng.Intn(len(citySyllables))],
				siteFunctions[rng.Intn(len(siteFunctions))],
				citySyllables[rng.Intn(len(citySyllables))], 1+rng.Intn(60), i)
		} else {
			intf.Mode = "routed"
			intf.IPAddress = randomRFC1918(rng)
			intf.Netmask = "255.255.255.0"
			peerCity := citySyllables[rng.Intn(len(citySyllables))]
			peerFunc := siteFunctions[rng.Intn(len(siteFunctions))]
			intf.Description = fmt.Sprintf("to-peer-rtr-%s-%s-%04d // circuit-C%06d vlan-%04d svc-%s",
				peerCity, peerFunc, 1000+rng.Intn(9000),
				100000+rng.Intn(900000), 100+rng.Intn(3900),
				[]string{"inet", "mpls-vpn", "mpls-l2vpn", "internet", "transit", "peering", "backbone"}[rng.Intn(7)])
			if prof.hasOSPF && rng.Intn(2) == 0 {
				intf.OSPFArea = rng.Intn(max(prof.ospfAreas, 1))
				intf.OSPFCost = 10 + rng.Intn(90)
			}
			if prof.hasQoS && rng.Intn(3) == 0 {
				intf.ServicePolicyOut = "WAN-OUT-POLICY"
			}
			if rng.Intn(8) == 0 && prof.aclCount > 0 {
				intf.ACLIn = fmt.Sprintf("ACL-IN-%02d", rng.Intn(prof.aclCount))
			}
			if rng.Intn(10) == 0 {
				intf.HSRPGroup = 10 + rng.Intn(90)
				intf.HSRPIP = randomRFC1918(rng)
				intf.HSRPPriority = 100 + rng.Intn(100)
			}
			if len(vrfs) > 0 && rng.Intn(6) == 0 {
				intf.VRF = vrfs[rng.Intn(len(vrfs))].Name
			}
		}
		if rng.Intn(25) == 0 {
			intf.Shutdown = true
		}
		out = append(out, intf)
	}
	return out
}

func buildSubinterfaces(rng *rand.Rand, prof profile, vrfs []VRF) []Subinterface {
	if prof.subinterfaceCount == 0 {
		return nil
	}
	out := make([]Subinterface, 0, prof.subinterfaceCount)
	for i := 0; i < prof.subinterfaceCount; i++ {
		parent := fmt.Sprintf("GigabitEthernet1/%d/%d", i/12, i%12)
		vlan := 100 + i*2
		si := Subinterface{
			Name:        fmt.Sprintf("%s.%d", parent, vlan),
			Description: fmt.Sprintf("VLAN %d sub-if", vlan),
			EncapVLAN:   vlan,
			IPAddress:   randomRFC1918(rng),
			Netmask:     "255.255.255.252",
		}
		if len(vrfs) > 0 && rng.Intn(3) == 0 {
			si.VRF = vrfs[rng.Intn(len(vrfs))].Name
		}
		if rng.Intn(5) == 0 {
			si.HSRPGroup = 10 + rng.Intn(90)
			si.HSRPIP = randomRFC1918(rng)
		}
		out = append(out, si)
	}
	return out
}

func allowedVLANsString(vlans []VLAN, rng *rand.Rand) string {
	if len(vlans) == 0 {
		return "1"
	}
	parts := make([]string, 0, len(vlans))
	for _, v := range vlans {
		parts = append(parts, fmt.Sprintf("%d", v.ID))
	}
	return strings.Join(parts, ",")
}

func buildACLs(rng *rand.Rand, prof profile) []ACL {
	if prof.aclCount == 0 {
		return nil
	}
	protocols := []string{"tcp", "tcp", "udp", "udp", "ip", "icmp"}
	actions := []string{"permit", "permit", "permit", "permit", "deny"} // biased toward permit
	portExtras := []string{
		"eq 22", "eq 80", "eq 443", "eq 53", "eq 123", "eq 161",
		"range 1024 65535", "gt 1024", "lt 1024",
		"eq 25", "eq 587", "eq 3389", "eq 8080", "eq 8443", "eq 5060",
		"eq 1812", "eq 1813", "eq 389", "eq 636", "eq 445",
		"range 49152 65535", "eq 443 established", "gt 32768",
	}
	stickyExtras := []string{"log", "log-input", "fragments", "established",
		"dscp ef", "dscp af41", "dscp cs1", "precedence critical", "precedence flash"}

	out := make([]ACL, 0, prof.aclCount)
	for i := 0; i < prof.aclCount; i++ {
		aclName := fmt.Sprintf("ACL-IN-%02d", i)
		if i%4 == 1 {
			aclName = fmt.Sprintf("ACL-OUT-%02d", i)
		} else if i%4 == 2 {
			aclName = fmt.Sprintf("ACL-DMZ-%02d", i)
		} else if i%4 == 3 {
			aclName = fmt.Sprintf("ACL-MGMT-%02d", i)
		}
		entries := prof.aclEntriesMin + rng.Intn(prof.aclEntriesMax-prof.aclEntriesMin+1)
		es := make([]ACLEntry, 0, entries)
		for s := 0; s < entries; s++ {
			action := actions[rng.Intn(len(actions))]
			proto := protocols[rng.Intn(len(protocols))]
			source := aclAddrSpec(rng)
			dest := aclAddrSpec(rng)
			var extraParts []string
			if proto == "tcp" || proto == "udp" {
				extraParts = append(extraParts, portExtras[rng.Intn(len(portExtras))])
			}
			if rng.Intn(3) == 0 {
				extraParts = append(extraParts, stickyExtras[rng.Intn(len(stickyExtras))])
			}
			es = append(es, ACLEntry{
				Seq:      10 + s*10,
				Action:   action,
				Protocol: proto,
				Source:   source,
				Dest:     dest,
				Extra:    strings.Join(extraParts, " "),
			})
		}
		// Trailing deny-any-any log for realism
		es = append(es, ACLEntry{
			Seq: 10 + entries*10, Action: "deny", Protocol: "ip",
			Source: "any", Dest: "any", Extra: "log",
		})
		out = append(out, ACL{
			Name:    aclName,
			Remark:  fmt.Sprintf("auto-generated %d-entry ACL for %s", entries, aclName),
			Entries: es,
		})
	}
	return out
}

func aclAddrSpec(rng *rand.Rand) string {
	switch rng.Intn(7) {
	case 0:
		return "any"
	case 1:
		return fmt.Sprintf("host %s", randomRFC1918(rng))
	case 2:
		return fmt.Sprintf("host %s", randomRFC1918(rng))
	case 3:
		return fmt.Sprintf("%s %s", randomRFC1918Network(rng), wildcardFor(24))
	case 4:
		return fmt.Sprintf("%s %s", randomRFC1918Network(rng), wildcardFor(22))
	case 5:
		return fmt.Sprintf("%s %s", randomRFC1918Network(rng), wildcardFor(16))
	default:
		return fmt.Sprintf("%s %s", randomRFC1918Network(rng), wildcardFor(28))
	}
}

// randomRFC1918Network returns the network address for a /24 in RFC1918 space.
func randomRFC1918Network(rng *rand.Rand) string {
	switch rng.Intn(3) {
	case 0:
		return fmt.Sprintf("10.%d.%d.0", rng.Intn(256), rng.Intn(256))
	case 1:
		return fmt.Sprintf("172.%d.%d.0", 16+rng.Intn(16), rng.Intn(256))
	default:
		return fmt.Sprintf("192.168.%d.0", rng.Intn(256))
	}
}

func buildOSPF(rng *rand.Rand, prof profile) *OSPFProcess {
	proc := &OSPFProcess{
		ID:           1,
		RouterID:     randomMgmtIP(rng),
		Redistribute: []string{"static subnets route-map STATIC-TO-OSPF", "connected subnets"},
	}
	for i := 1; i < max(prof.ospfAreas, 1); i++ {
		areaType := "stub"
		if i > 1 && rng.Intn(2) == 0 {
			areaType = "nssa"
		}
		proc.Areas = append(proc.Areas, OSPFArea{ID: i, Type: areaType})
	}
	// network statements
	numNets := prof.ospfAreas * 4
	if numNets < 4 {
		numNets = 4
	}
	for i := 0; i < numNets; i++ {
		proc.Networks = append(proc.Networks, OSPFNetwork{
			Network:  fmt.Sprintf("10.%d.%d.0", rng.Intn(256), rng.Intn(256)),
			Wildcard: "0.0.0.255",
			Area:     rng.Intn(max(prof.ospfAreas, 1)),
		})
	}
	for i := 0; i < 4; i++ {
		proc.PassiveIfs = append(proc.PassiveIfs, fmt.Sprintf("GigabitEthernet1/%d/%d", i, rng.Intn(12)))
	}
	return proc
}

func buildBGP(rng *rand.Rand, prof profile, localAS int) *BGPProcess {
	proc := &BGPProcess{
		LocalAS:       localAS,
		RouterID:      randomMgmtIP(rng),
		LogAdjChanges: true,
		Redistribute:  []string{"static route-map STATIC-TO-BGP", "connected"},
	}
	for i := 0; i < prof.bgpNeighbors; i++ {
		n := BGPNeighbor{
			IP:            randomPublicIP(rng),
			RemoteAS:      64512 + rng.Intn(1024),
			Description:   fmt.Sprintf("ebgp-peer-%d", i),
			UpdateSource:  "Loopback0",
			EBGPMultihop:  2,
			SoftReconfig:  true,
			NextHopSelf:   true,
			RouteMapIn:    fmt.Sprintf("RM-IN-%02d", i%prof.routeMapCount),
			RouteMapOut:   fmt.Sprintf("RM-OUT-%02d", i%prof.routeMapCount),
			PrefixListIn:  fmt.Sprintf("PL-IN-%02d", i%max(prof.prefixListCount, 1)),
			PrefixListOut: fmt.Sprintf("PL-OUT-%02d", i%max(prof.prefixListCount, 1)),
			SendCommunity: true,
			MaxPrefix:     100000,
		}
		proc.Neighbors = append(proc.Neighbors, n)
	}
	for i := 0; i < 20; i++ {
		proc.Networks = append(proc.Networks, BGPNetwork{
			Network: fmt.Sprintf("10.%d.0.0", rng.Intn(256)),
			Mask:    "255.255.0.0",
		})
	}
	return proc
}

func buildStaticRoutes(rng *rand.Rand, prof profile, vrfs []VRF) []StaticRoute {
	if prof.staticRouteCount == 0 {
		return nil
	}
	out := make([]StaticRoute, 0, prof.staticRouteCount)
	for i := 0; i < prof.staticRouteCount; i++ {
		r := StaticRoute{
			Network: fmt.Sprintf("10.%d.%d.0", rng.Intn(256), rng.Intn(256)),
			Mask:    "255.255.255.0",
			NextHop: randomRFC1918(rng),
		}
		if rng.Intn(10) == 0 {
			r.AD = 200 + rng.Intn(55)
		}
		if rng.Intn(4) == 0 {
			r.Name = fmt.Sprintf("route-%d", i)
		}
		if len(vrfs) > 0 && rng.Intn(5) == 0 {
			r.VRF = vrfs[rng.Intn(len(vrfs))].Name
		}
		out = append(out, r)
	}
	return out
}

func buildPrefixLists(rng *rand.Rand, prof profile) []PrefixList {
	if prof.prefixListCount == 0 {
		return nil
	}
	out := make([]PrefixList, 0, prof.prefixListCount)
	for i := 0; i < prof.prefixListCount; i++ {
		name := fmt.Sprintf("PL-IN-%02d", i)
		if i%3 == 1 {
			name = fmt.Sprintf("PL-OUT-%02d", i)
		} else if i%3 == 2 {
			name = fmt.Sprintf("PL-ANY-%02d", i)
		}
		entries := prof.prefixListEntriesMin + rng.Intn(prof.prefixListEntriesMax-prof.prefixListEntriesMin+1)
		es := make([]PrefixListEntry, 0, entries)
		for s := 0; s < entries; s++ {
			pl := 16 + rng.Intn(8)
			entry := PrefixListEntry{
				Seq:    10 + s*5,
				Action: "permit",
				Prefix: fmt.Sprintf("%d.%d.0.0/%d", rng.Intn(256), rng.Intn(256), pl),
				GE:     pl,
				LE:     pl + rng.Intn(32-pl+1),
			}
			if rng.Intn(10) == 0 {
				entry.Action = "deny"
			}
			es = append(es, entry)
		}
		out = append(out, PrefixList{
			Name:        name,
			Description: fmt.Sprintf("autogen %s (%d entries)", name, entries),
			Entries:     es,
		})
	}
	return out
}

func buildRouteMaps(rng *rand.Rand, prof profile) []RouteMap {
	if prof.routeMapCount == 0 {
		return nil
	}
	out := make([]RouteMap, 0, prof.routeMapCount)
	names := []string{"RM-IN-", "RM-OUT-", "STATIC-TO-OSPF-", "STATIC-TO-BGP-", "LOCAL-PREF-", "AS-PATH-"}
	for i := 0; i < prof.routeMapCount; i++ {
		name := fmt.Sprintf("%s%02d", names[i%len(names)], i)
		nEntries := 2 + rng.Intn(3)
		es := make([]RouteMapEntry, 0, nEntries)
		for s := 0; s < nEntries; s++ {
			action := "permit"
			if s == nEntries-1 && rng.Intn(5) == 0 {
				action = "deny"
			}
			e := RouteMapEntry{
				Seq:    10 + s*10,
				Action: action,
				Match:  []string{fmt.Sprintf("ip address prefix-list PL-IN-%02d", s%max(prof.prefixListCount, 1))},
				Set:    []string{fmt.Sprintf("local-preference %d", 100+rng.Intn(200))},
			}
			if rng.Intn(3) == 0 {
				e.Set = append(e.Set, fmt.Sprintf("community %d:%d", 64500+rng.Intn(100), rng.Intn(1000)))
			}
			if rng.Intn(4) == 0 {
				e.Match = append(e.Match, fmt.Sprintf("as-path %d", 10+rng.Intn(40)))
			}
			es = append(es, e)
		}
		out = append(out, RouteMap{Name: name, Entries: es})
	}
	return out
}

func buildCryptoMaps(rng *rand.Rand, prof profile) []CryptoMap {
	n := 2
	if prof.hasVRF {
		n = 4
	}
	out := make([]CryptoMap, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, CryptoMap{
			Name:         "VPN-MAP",
			Seq:          10 + i*10,
			PeerIP:       randomPublicIP(rng),
			ACL:          fmt.Sprintf("CRYPTO-ACL-%02d", i),
			TransformSet: "AES256-SHA",
			PFS:          "group14",
			LifetimeSec:  3600,
		})
	}
	return out
}

func buildQoS(rng *rand.Rand, prof profile) ([]QoSClassMap, []QoSPolicyMap) {
	classMaps := []QoSClassMap{
		{Name: "VOICE", Match: []string{"ip dscp ef"}},
		{Name: "VIDEO", Match: []string{"ip dscp af41"}},
		{Name: "CRITICAL", Match: []string{"ip dscp af31"}},
		{Name: "BULK", Match: []string{"ip dscp af11"}},
		{Name: "SCAVENGER", Match: []string{"ip dscp cs1"}},
	}
	policyMap := QoSPolicyMap{
		Name: "WAN-OUT-POLICY",
		Classes: []QoSPolicyClass{
			{Name: "VOICE", Priority: 30, MarkingDSCP: "ef"},
			{Name: "VIDEO", Bandwidth: 25, MarkingDSCP: "af41"},
			{Name: "CRITICAL", Bandwidth: 20, MarkingDSCP: "af31"},
			{Name: "BULK", Bandwidth: 15, MarkingDSCP: "af11"},
			{Name: "SCAVENGER", Bandwidth: 1, MarkingDSCP: "cs1"},
			{Name: "class-default", Bandwidth: 9, PoliceBPS: 10000000},
		},
	}
	return classMaps, []QoSPolicyMap{policyMap}
}
