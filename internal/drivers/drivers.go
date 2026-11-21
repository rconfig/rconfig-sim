// Package drivers is the metadata of every device driver: its id (the
// manifest template value), vendor, protocol and whether the SSH transport
// authenticates. It has no dependencies, so the generator's catalogue
// (rcfg-sim-gen catalogue --json) and the SSH server (internal/sshsrv) read
// the same facts without rcfg-sim-gen linking the SSH stack (D7, D14). A
// test in internal/sshsrv keeps the registered drivers and this list in
// step.
package drivers

// Protocols.
const (
	CLI = "cli"
	TL1 = "tl1"
)

// Driver describes one driver.
type Driver struct {
	ID       string `json:"id"`
	Vendor   string `json:"vendor"`
	Protocol string `json:"protocol"`
	// RequiresSSHAuth: the device authenticates at the SSH layer (Cisco IOS,
	// Junos). TL1 devices log in in-band with ACT-USER instead. Consulted by
	// --ssh-auth=driver.
	RequiresSSHAuth bool `json:"requires_ssh_auth"`
}

// All is every driver, in a stable order.
var All = []Driver{
	{ID: "cisco_ios", Vendor: "Cisco", Protocol: CLI, RequiresSSHAuth: true},
	{ID: "junos", Vendor: "Juniper", Protocol: CLI, RequiresSSHAuth: true},
	{ID: "ciena_tl1", Vendor: "Ciena", Protocol: TL1, RequiresSSHAuth: false},
	{ID: "infinera_tl1", Vendor: "Infinera", Protocol: TL1, RequiresSSHAuth: false},
	{ID: "cisco_ons_tl1", Vendor: "Cisco", Protocol: TL1, RequiresSSHAuth: false},
}

// Lookup returns the driver with this id.
func Lookup(id string) (Driver, bool) {
	for _, d := range All {
		if d.ID == id {
			return d, true
		}
	}
	return Driver{}, false
}

// RequiresSSHAuth is Lookup(id).RequiresSSHAuth, for the drivers' own
// method. An unknown id panics: a driver without metadata is a programming
// error the sshsrv test catches.
func RequiresSSHAuth(id string) bool {
	d, ok := Lookup(id)
	if !ok {
		panic("drivers: no metadata for driver " + id)
	}
	return d.RequiresSSHAuth
}
