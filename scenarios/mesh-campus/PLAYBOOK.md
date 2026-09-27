# mesh-campus playbook

How to stand up the mesh-campus scenario by hand and test rConfig Mesh against it. Follow the steps in order. Each step ends with a check.

## What you get

| Device | OS | Sim address | rConfig device_model |
| --- | --- | --- | --- |
| core-01 | Arista EOS | 10.1.1.2:12700 | vEOS |
| dist-01 | Juniper Junos | 10.1.1.2:12701 | vMX |
| access-01 | Cisco IOS | 10.1.1.2:12702 | WS-C2960X |

Plus the two real lab devices, router1 (10.1.1.170) and mikrotik (10.1.1.188).

## 1. Build the sim

On the sim host (10.1.1.2):

```
cd /var/www/html/rconfig-sim
make build
```

**Check:** `ls bin/` shows `rcfg-sim` and `rcfg-sim-gen`.

## 2. Put the scenario in place

The scenario lives at `scenarios/mesh-campus/` with `manifest.csv`, `configs/`, `commands/`, `README.md` and this playbook.

**Check:** `ls scenarios/mesh-campus/commands` shows `access-01  core-01  dist-01`.

The manifest `ip` column must be `10.1.1.2` and `config_file` paths must be absolute.

## 3. Start the sim in tmux

Run it in tmux so it survives your terminal closing.

```
tmux new -s sim
cd /var/www/html/rconfig-sim
./bin/rcfg-sim \
  --manifest scenarios/mesh-campus/manifest.csv \
  --listen-ip 10.1.1.2 --port-start 12700 --port-count 3 \
  --commands-root scenarios/mesh-campus/commands \
  --password "" --enable-password "" \
  --metrics-addr 127.0.0.1:19100
```

Detach with `Ctrl-b d`. Reattach with `tmux attach -t sim`.

**Check:** `ss -tlnp | grep -E '1270[0-2]'` shows three listeners.

## 4. Smoke test each device

Any password works.

```
ssh -p 12700 admin@10.1.1.2    # core-01>   then: show lldp neighbors detail
ssh -p 12701 admin@10.1.1.2    # admin@dist-01>   then: show lldp neighbors
ssh -p 12702 admin@10.1.1.2    # access-01>   then: show cdp neighbors detail
```

**Check:** each command returns neighbour text, not `% Invalid input` or `unknown command.`

## 5. Prepare the real lab devices

Discovery must be on for the real pair, or the router1 and mikrotik link disappears.

- router1 (CSR): `cdp run` and `lldp run` globally, `cdp enable` on GigabitEthernet1
- mikrotik: `/ip neighbor discovery-settings set discover-interface-list=all`

**Check:** `show cdp neighbors` on router1 lists router-mikrotik.

## 6. Add the sim devices to rConfig

In rConfig (UI or the normal device service), add:

| Name | IP | Port override | device_model | Template | Main prompt | Enable prompt |
| --- | --- | --- | --- | --- | --- | --- |
| core-01 | 10.1.1.2 | 12700 | vEOS | Cisco IOS - SSH - Enable (id 4) | core-01# | core-01> |
| dist-01 | 10.1.1.2 | 12701 | vMX | Juniper JUNOS - SSH - No Enable | admin@dist-01> | blank |
| access-01 | 10.1.1.2 | 12702 | WS-C2960X | Cisco IOS - SSH - Enable (id 4) | access-01# | access-01> |

All three: username admin, any password (the sim accepts any), blank enable password, category Routers.

**Templates.** A fresh install has no EOS or Junos template loaded. EOS works with the Cisco IOS template because the sim uses IOS-style prompts. For Junos, add a template from `rConfig-templates/juniper/juniper-junos-ssh-noenable.yml`: copy the file into `storage/app/rconfig/templates/` and add a templates row pointing at it (the prototype used id 7000).

**Ids.** On a seeded lab the Postgres id sequences for devices and templates may still be at 1 and collide with seeded rows. Either reset the sequences or set ids explicitly (the prototype used 12700 to 12702).

**Check:** the three devices appear in rConfig inventory.

## 7. Check the Mesh config

`config/mesh.php` in rconfig8 must have:

- platforms: `CSR1000v => ios`, `Mikrotik => routeros`, `vEOS => eos`, `vMX => junos`, `WS-C2960X => ios`
- exclude: the duplicate lab records `1002, 1003, 1004, 1009, 1010, 9000, 30001`

After editing config: `php artisan config:clear`.

## 8. Collect and reconcile

In `/var/www/html/rconfig8`:

```
php artisan mesh:collect --all
php artisan mesh:reconcile --fresh
php artisan mesh:fleet
```

## 9. Expected result

**Links** (5 total, 3 bidirectional, 2 unidirectional, 1 unknown device):

| Link | Confidence | Protocol |
| --- | --- | --- |
| core-01 Ethernet1 to dist-01 ge-0/0/0 | bidirectional | lldp |
| core-01 Ethernet2 to access-01 GigabitEthernet0/1 | bidirectional | lldp |
| router1 GigabitEthernet1 to mikrotik ether4 | bidirectional | cdp |
| access-01 GigabitEthernet0/24 to ap-lobby-01 (unknown) | unidirectional | lldp |
| access-01 GigabitEthernet0/2 to router1 GigabitEthernet2 | unidirectional | cdp |

**Fleet:** coverage 5 of 8. router5, router6 and router8 flagged failing. access-01 and router1 flagged one-sided.

**Screens to walk through:**

- [ ] `/mesh/fleet` shows 5 of 8, click **failing** to filter
- [ ] `/mesh/map` shows 5 managed nodes, 1 dashed unknown node, 2 dashed links
- [ ] Click the core-01 to dist-01 link: lldp, bidirectional
- [ ] `/mesh/devices/<access-01 id>` shows 3 neighbours, match column `mac`, `ip` and `unknown`
- [ ] Raw viewer on access-01 LLDP shows raw text beside 2 parsed records
- [ ] `/mesh/schema` shows six tables with sensible row counts

## 10. Reset between runs

```
php artisan mesh:reconcile --fresh    # rebuild links and unknown devices from current data
```

To remove the scenario completely: delete the three sim devices in rConfig, then `tmux kill-session -t sim`.

## Pitfalls

- **Delete old sim devices in rConfig before re-adding them.** If an earlier core-01, dist-01 or access-01 record is still there, two devices share the same interface MACs and IP. Mesh then finds two candidates for a chassis ID and can no longer match identities: links come out as `unknown: ambiguous ip` or unidirectional. This happened twice in testing. Delete the old records first (or add their ids to `exclude` in `config/mesh.php`), then run `php artisan mesh:reconcile --fresh`.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Sim devices fail to connect | Sim not running, or stopped with its session | `tmux attach -t sim`, restart step 3 |
| `address already in use` on start | Another sim copy holds the ports | Stop the other copy, then restart |
| `% Invalid input` for a Mesh command | Command file missing or misnamed | File must be `commands/<hostname>/<slug>.txt`, slug = lowercase, spaces to `_` |
| Added or edited a command file, output unchanged | Folder listings and files are cached until restart | Restart the sim |
| core-01 to access-01 link is unidirectional | Short names (Et2, Gi0/1) not normalised on both ends | Check Normalise expands local and remote names |
| router1 to mikrotik link missing | Discovery off on the real devices | Redo step 5 |
| Every link to mikrotik shows unknown | Duplicate records not excluded | Check the exclude list in step 7 |
| Sessions drop after 5 minutes | Sim session limit | Expected, collections take seconds |