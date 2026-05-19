# Changelog

All notable changes to `rcfg-sim` are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.0.2] — 2026-05-19

Bucket-label rename and five new stress-test size tiers. Breaking change: every `--distribution` string and every `size_bucket` value in existing manifests is invalidated. Migration is a mechanical rename — see below.

### Breaking

- **Renamed all four size buckets** to clothing-size labels:
  - `small` → `sm`
  - `medium` → `md`
  - `large` → `lg`
  - `huge` → `xl`
- **Renamed the corresponding templates** under `internal/configs/templates/`: `small.tmpl` → `sm.tmpl`, `medium.tmpl` → `md.tmpl`, `large.tmpl` → `lg.tmpl`, `huge.tmpl` → `xl.tmpl`.
- **Changed the `--distribution` default** on `rcfg-sim-gen` from `"small:40,medium:40,large:15,huge:5"` to `"sm:40,md:40,lg:15,xl:5"`.
- **Manifest `size_bucket` column values** now emit the new labels. Any rConfig group import or downstream tool that filters on bucket names must be updated.

#### Migration

```bash
# Old invocation:
rcfg-sim-gen --distribution "small:40,medium:40,large:15,huge:5"

# New invocation (identical distribution):
rcfg-sim-gen --distribution "sm:40,md:40,lg:15,xl:5"
```

For pre-generated manifests, either regenerate or `sed -i 's/,small$/,sm/; s/,medium$/,md/; s/,large$/,lg/; s/,huge$/,xl/' manifest.csv`.

### Added

- **Five new stress-test size tiers** above `xl`, accessible from `rcfg-sim-gen --distribution`:
  - `2xl` — ~8 MB hint, ~6 MB realised (256 interfaces, 100 ACLs, 6 K static routes, 30 BGP neighbours, 50 VRFs).
  - `3xl` — ~16 MB hint, ~13 MB realised (384 interfaces, 160 ACLs, 12 K static routes, 40 BGP, 70 VRFs).
  - `4xl` — ~32 MB hint, ~25 MB realised (512 interfaces, 240 ACLs, 24 K static routes, 60 BGP, 100 VRFs).
  - `5xl` — ~64 MB hint, ~52 MB realised (768 interfaces, 380 ACLs, 48 K static routes, 100 BGP, 150 VRFs).
  - `6xl` — ~128 MB hint, ~107 MB realised (1024 interfaces, 600 ACLs, 96 K static routes, 160 BGP, 220 VRFs).

  These tiers share the existing `xl` template via a template alias map — they differ from `xl` only in profile counts, not in feature set, so adding them costs ~80 lines of profile data with no duplicated boilerplate.

### Changed

- The "unknown bucket" error from `rcfg-sim-gen --distribution` now lists valid bucket names dynamically rather than hard-coding the v1 list.
- README, `TEST-SCENARIOS.md`, and `FEATURE-TESTS.md` updated for the new bucket labels; the size-bucket table in the README now documents all nine tiers.
- Added [CLAUDE.md](CLAUDE.md) with project rules for versioning, commit conventions, and release workflow.

## [0.0.1] — 2026-04-21

Initial public release. High-density Cisco IOS SSH simulator for load testing [rConfig](https://www.rconfig.com).

### Added

- **50,000+ concurrent SSH listeners** on a single host via `mmap`'d configs — `MAP_SHARED`/`PROT_READ`, zero-copy delivery on the hot path.
- **Real Cisco IOS emulation**: enable mode, Cisco-style abbreviated commands (`sh run` → `show running-config`), ambiguity detection, realistic prompts (`>` / `#`), deterministic per-device serial numbers, the ten-or-so commands rConfig's stock collection template issues (`terminal length 0`, `terminal pager 0`, `enable`, `show version`, `show running-config`, `show startup-config`, `show inventory`, `exit` / `quit` / `logout` / `end`).
- **Deterministic config generator** (`rcfg-sim-gen`): four size buckets (small ~30 KB to huge ~5 MB) with parameterised hostnames, ACLs, interfaces, VLANs, routing, AAA stanzas; 200 fictional sites; seeded RNG produces byte-identical output across runs; parallel rendering sized to `runtime.NumCPU()`.
- **Prometheus metrics** with bounded label cardinality (verified by test): `rcfgsim_active_sessions`, `rcfgsim_sessions_total{result}`, `rcfgsim_session_duration_seconds`, `rcfgsim_command_duration_seconds{command}`, `rcfgsim_bytes_sent_total`, `rcfgsim_auth_attempts_total{result}`, `rcfgsim_handshake_duration_seconds`, `rcfgsim_faults_injected_total{type}`. Plus standard Go runtime + process collectors. `/healthz` for liveness.
- **Four fault injection types** with per-session RNG and verified zero overhead when disabled: `auth_fail` (reject handshake), `disconnect_mid` (TCP RST during `show running-config`), `slow_response` (10–50× delay multiplier), `malformed` (truncate / inject marker / bit-flip). Each independently toggleable via `--fault-types`.
- **Systemd-native operation**: one `rcfg-sim@<LISTEN_IP>.service` instance per IP alias, independent restart, graceful drain (up to 30 s) via SIGTERM, per-instance env file at `/etc/rcfg-sim/<IP>.env`, journal logging with structured JSON, sandbox hardening (`NoNewPrivileges`, `ProtectSystem=full`, `PrivateTmp`).
- **Deployment artifacts**: `deploy/systemd/rcfg-sim@.service` template unit, `deploy/ip-aliases.sh` for batched idempotent IP alias management, `deploy/sysctl-rcfg-sim.conf` tuning drop-in, `deploy/limits-rcfg-sim.conf` for interactive use.
- **Makefile targets**: `build`, `test`, `integration`, `bench`, `vet`, `fmt`, `install`, `uninstall`, `generate-configs`, `deploy-aliases`, `remove-aliases`.
- **Documentation**: comprehensive README (quickstart through production runbook), [FEATURE-TESTS.md](FEATURE-TESTS.md) with 36 runnable manual feature-verification procedures, [TEST-SCENARIOS.md](TEST-SCENARIOS.md) with 9 end-to-end load-testing scenarios, [SECURITY.md](SECURITY.md) reporting policy.
- **CI**: GitHub Actions workflow testing `go vet`, `gofmt`, unit + integration tests, and build across Go 1.22 / 1.23 / 1.24.

### Security

- Password auth only; no SSH public key auth (out of scope for the rConfig collection flow).
- Documented test credentials (`admin` / `admin` / `enable123`) are for lab use — not production secrets.
- Metrics endpoint exposes only bounded label sets; no hostname or session-ID labels that could leak cardinality.
- Systemd unit runs as unprivileged `rcfgsim` user under sandbox; `LimitNOFILE=200000`.

### Known limitations

- Single-host scale: target is 50k devices on one Rocky 9 box; horizontal scaling across hosts is out of scope.
- Cisco IOS only — no multi-vendor emulation.
- No SSH config mutation (`configure terminal`, `write memory`, etc. return `% Invalid input`).
- `show startup-config` returns the same bytes as `show running-config` by design in v1.
- IPv4 only; no IPv6 listener binding.

See [README § Known limitations](README.md#known-limitations) for the full list.

[Unreleased]: https://github.com/rconfig/rconfig-sim/compare/v0.0.2...HEAD
[0.0.2]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.2
[0.0.1]: https://github.com/rconfig/rconfig-sim/releases/tag/v0.0.1
