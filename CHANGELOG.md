# Changelog

All notable changes to `rcfg-sim` are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Breaking

- The `rcfg-sim@.service` unit now defaults `HOST_KEY` to `/opt/rcfg-sim/host-keys/<IP>.key` and can no longer read `/etc/rcfg-sim`. Instances that kept their key there will generate a new one (and SSH clients will see a changed host key) unless you move it first.

#### Migration

```bash
sudo install -d -m 0700 -o rcfgsim -g rcfgsim /opt/rcfg-sim/host-keys
for env in /etc/rcfg-sim/*.env; do
  ip=$(basename "$env" .env)
  key=$(sed -n 's/^HOST_KEY=//p' "$env"); key=${key:-/etc/rcfg-sim/ssh_host_rsa_key}
  case "$key" in /etc/rcfg-sim/*)
    [ -f "$key" ] || continue
    sudo install -m 0600 -o rcfgsim -g rcfgsim "$key" "/opt/rcfg-sim/host-keys/$ip.key"
    sudo sed -i "/^HOST_KEY=/d" "$env"
    echo "HOST_KEY=/opt/rcfg-sim/host-keys/$ip.key" | sudo tee -a "$env" >/dev/null ;;
  esac
done
sudo make install && sudo systemctl daemon-reload && sudo systemctl restart 'rcfg-sim@*'
```

### Added

- Device profiles (`hang`, `latency`, `refuse`, `disconnect`) give chosen devices the same misbehaviour on every run. Assign them to a share of the fleet with `--device-profiles` and `--profile-seed`, or pin one device with a new optional `profile` manifest column. See [Device profiles](README.md#device-profiles).
- `--fault-rates` sets a separate probability for each fault type, e.g. `auth_fail=0.02,disconnect_mid=0.005`.
- `--active-devices` limits how many devices accept sessions, and `--max-session` (default `5m`) sets how long a session may last.
- The systemd unit passes the new flags through env variables, and every variable now falls back to the flag's default when left out.
- An optional admin API (`--admin`) lets you change faults, profiles, delays and active devices at runtime and watch live activity. It has no authentication, so keep `--metrics-addr` on loopback. See [Admin API](README.md#admin-api).
- New optional web UI, `rcfg-sim-ui`, with a single-user login. It has Fleet, Failures, Build and Activity screens for controlling instances, setting up faults, regenerating the fleet and watching live traffic. Install it with `make install-ui`; see [Web UI](README.md#web-ui).
- `make adopt-data` moves an existing install's configs and manifest into the versioned `data/` layout used by the Build screen.

### Changed

- Building from source now needs Go 1.26 or later.
- A `hang` device's session cap is extended by the length of its hang, so long hangs are no longer cut short.
- `make install` gives `/etc/rcfg-sim` to group `rcfgsim-ui` so the web UI can edit env files.

## [0.0.5] — 2026-09-27

### Breaking

- Ciena neighbour discovery now uses `RTRV-NE-LIST` with the real 6500 response format, replacing the invented `RTRV-NBR`. The metric label changes from `CmdTL1RtrvNbr` to `CmdTL1RtrvNeList`.
- rcfg-sim now refuses to start when the manifest names an unknown driver, instead of quietly serving it as Cisco IOS.

#### Migration

Send `RTRV-NE-LIST:<tid>:<ctag>;` instead of `RTRV-NBR` and update dashboards to `command="CmdTL1RtrvNeList"`. Fix any manifest `template` value the new startup error reports, or set it to `cisco_ios`.

### Added

- New Infinera DTN-X (`infinera-dtnx-tl1`) and Cisco ONS 15454 (`cisco-ons15454-tl1`) TL1 gateway models. Each uses its vendor's own neighbour-listing format, and Infinera splits long responses across pages the way real hardware does.
- `--commands-root DIR` lets a device answer any command from `DIR/<hostname>/<slug>.txt`, for `cisco_ios` and `junos` devices.
- New minimal Juniper `junos` driver for `show configuration` and `set cli` commands.
- New `scenarios/mesh-campus/` with three multi-vendor devices whose neighbour data agrees, for testing topology discovery.

### Changed

- `--rne-dual-home-pct` makes a share of remote NEs reachable through several gateways, as real optical networks are built. It defaults to 0, so output is unchanged unless you set it.

### Fixed

- TL1 `ACT-USER` now accepts quoted passwords.
- Fixed two integration tests that failed now and then: one on port collisions, one on a race.

## [0.0.4] — 2026-06-07

### Added

- New `ciena-6500-tl1-gne` model: a Ciena gateway node that relays `RTRV-*` commands to 2–5 remote nodes behind it, addressed by their TID.

## [0.0.3] — 2026-06-06

### Added

- Multi-vendor driver framework: the manifest `template` column now picks each device's CLI behaviour. Cisco output is unchanged.
- New Ciena 6500 TL1 model (`ciena-6500-tl1`) with `ACT-USER` login and the common `RTRV-*` commands.
- `--ssh-auth` (`password`, `driver`, `none`) controls whether devices ask for an SSH password, to match how real TL1 devices are reached.

### Changed

- Building from source now needs Go 1.24 or later.

## [0.0.2] — 2026-05-19

### Breaking

- Size buckets were renamed `small`/`medium`/`large`/`huge` → `sm`/`md`/`lg`/`xl`, which breaks existing `--distribution` strings and manifests.

#### Migration

```bash
rcfg-sim-gen --distribution "sm:40,md:40,lg:15,xl:5"   # was small:40,medium:40,large:15,huge:5
sed -i 's/,small$/,sm/; s/,medium$/,md/; s/,large$/,lg/; s/,huge$/,xl/' manifest.csv
```

### Added

- Five larger stress-test sizes, `2xl` to `6xl`, from about 6 MB to about 107 MB per config.

## [0.0.1] — 2026-04-21

### Added

- First public release: a Cisco IOS SSH simulator that serves 50,000+ devices from one host, plus a deterministic config generator, Prometheus metrics, fault injection and systemd deployment files.

[Unreleased]: https://github.com/rconfig/rconfig-sim/compare/v0.0.5...HEAD
[0.0.5]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.5
[0.0.4]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.4
[0.0.3]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.3
[0.0.2]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.2
[0.0.1]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.1
