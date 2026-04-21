package configs

// TemplateData is the full payload available to every template.
// Each template pulls whatever subset it needs.
type TemplateData struct {
	Hostname        string
	SizeBucket      string
	DeviceKind      string
	Site            string
	MgmtIP          string
	DomainName      string
	Clock           string
	FileSizeHint    int
	SerialNumber    string
	NameServers     []string
	EnableSecret    string
	Users           []User
	SNMPCommunityRO string
	SNMPCommunityRW string
	SNMPLocation    string
	SNMPContact     string
	LoggingHosts    []string
	NTPServers      []string
	BannerMOTD      []string
	TACACSServers   []string
	TACACSKey       string
	LocalASN        int

	Interfaces    []Interface
	Subinterfaces []Subinterface
	VLANs         []VLAN
	ACLs          []ACL
	OSPF          *OSPFProcess
	BGP           *BGPProcess
	StaticRoutes  []StaticRoute
	PrefixLists   []PrefixList
	RouteMaps     []RouteMap
	VRFs          []VRF
	CryptoMaps    []CryptoMap
	QoSClassMaps  []QoSClassMap
	QoSPolicyMaps []QoSPolicyMap

	IncludeBGP    bool
	IncludeOSPF   bool
	IncludeCrypto bool
	IncludeQoS    bool
	IncludeVRF    bool
}

type User struct {
	Name       string
	Privilege  int
	SecretHash string
}

type Interface struct {
	Name             string
	Description      string
	Mode             string // "access" | "trunk" | "routed"
	IPAddress        string
	Netmask          string
	VLAN             int
	AllowedVLANsStr  string
	Shutdown         bool
	ServicePolicyIn  string
	ServicePolicyOut string
	ACLIn            string
	ACLOut           string
	OSPFArea         int
	OSPFCost         int
	HSRPGroup        int
	HSRPIP           string
	HSRPPriority     int
	Speed            string
	Duplex           string
	MTU              int
	Portfast         bool
	BPDUGuard        bool
	VRF              string
}

type Subinterface struct {
	Name        string
	Description string
	EncapVLAN   int
	VRF         string
	IPAddress   string
	Netmask     string
	HSRPGroup   int
	HSRPIP      string
}

type VLAN struct {
	ID   int
	Name string
}

type ACL struct {
	Name    string
	Remark  string
	Entries []ACLEntry
}

type ACLEntry struct {
	Seq      int
	Action   string
	Protocol string
	Source   string
	Dest     string
	Extra    string
}

type OSPFProcess struct {
	ID           int
	RouterID     string
	Redistribute []string
	Areas        []OSPFArea
	PassiveIfs   []string
	Networks     []OSPFNetwork
}

type OSPFArea struct {
	ID   int
	Type string
}

type OSPFNetwork struct {
	Network  string
	Wildcard string
	Area     int
}

type BGPProcess struct {
	LocalAS       int
	RouterID      string
	LogAdjChanges bool
	Neighbors     []BGPNeighbor
	Networks      []BGPNetwork
	Redistribute  []string
	Aggregate     []string
}

type BGPNeighbor struct {
	IP            string
	RemoteAS      int
	Description   string
	UpdateSource  string
	EBGPMultihop  int
	SoftReconfig  bool
	NextHopSelf   bool
	RouteMapIn    string
	RouteMapOut   string
	PrefixListIn  string
	PrefixListOut string
	SendCommunity bool
	MaxPrefix     int
}

type BGPNetwork struct {
	Network  string
	Mask     string
	RouteMap string
}

type StaticRoute struct {
	Network string
	Mask    string
	NextHop string
	AD      int
	Name    string
	VRF     string
}

type PrefixList struct {
	Name        string
	Description string
	Entries     []PrefixListEntry
}

type PrefixListEntry struct {
	Seq    int
	Action string
	Prefix string
	GE     int
	LE     int
}

type RouteMap struct {
	Name    string
	Entries []RouteMapEntry
}

type RouteMapEntry struct {
	Seq    int
	Action string
	Match  []string
	Set    []string
}

type VRF struct {
	Name     string
	RD       string
	RTImport []string
	RTExport []string
}

type CryptoMap struct {
	Name         string
	Seq          int
	PeerIP       string
	ACL          string
	TransformSet string
	PFS          string
	LifetimeSec  int
}

type QoSClassMap struct {
	Name  string
	Match []string
}

type QoSPolicyMap struct {
	Name    string
	Classes []QoSPolicyClass
}

type QoSPolicyClass struct {
	Name        string
	Priority    int
	Bandwidth   int
	MarkingDSCP string
	PoliceBPS   int
}
