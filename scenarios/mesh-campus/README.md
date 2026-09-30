# mesh-campus scenario

A small multi-vendor campus for the rConfig Mesh (LLDP/CDP) prototype. Every link is written so both ends agree.

| Device | OS | Driver | Port | Chassis / base MAC | Mgmt IP (advertised) |
| --- | --- | --- | --- | --- | --- |
| core-01 | Arista EOS 4.31 | cisco_ios | 12700 | 5254.00aa.0101 (Management1) | 10.20.0.1 |
| dist-01 | Junos 22.4 | junos | 12701 | 2c:6b:f5:aa:02:01 (fxp0) | 10.20.0.2 |
| access-01 | IOS 15.2 (C2960X) | cisco_ios | 12702 | 7c95.f3aa.0300 (Vlan1) | 10.20.0.3 |

Links:

| A side | B side | Protocol | Expected in Mesh |
| --- | --- | --- | --- |
| core-01 Ethernet1 | dist-01 ge-0/0/0 | LLDP | bidirectional |
| core-01 Ethernet2 | access-01 GigabitEthernet0/1 | LLDP | bidirectional (tests Et2 and Gi0/1 short names) |
| access-01 GigabitEthernet0/24 | ap-lobby-01 (not in rConfig) | LLDP | unknown device, unidirectional |
| access-01 GigabitEthernet0/2 | router1 GigabitEthernet2 (real lab CSR) | CDP | one-sided, the real CSR never sees the sim |

Chassis IDs match a MAC in each device's `show interfaces`, so Mesh should resolve identities by MAC.

## Run

The manifest `ip` column holds the address rConfig reaches the sim on (checked in as `10.1.1.2`), and `config_file` paths are absolute. Adjust both for your host, then:

```
rcfg-sim --manifest scenarios/mesh-campus/manifest.csv --listen-ip <SIM_IP> --port-start 12700 --port-count 3 --commands-root scenarios/mesh-campus/commands
```

For the full walkthrough (starting the sim, adding the devices to rConfig, collecting, expected links, pitfalls and troubleshooting), see the [playbook](PLAYBOOK.md).

The `TestMeshCampus_CommandFiles` integration test serves this scenario on loopback and checks every command file byte for byte.
