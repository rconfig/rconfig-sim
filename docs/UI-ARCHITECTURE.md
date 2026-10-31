<div align="center">

<img src="images/rconfig_sim_logo_and_icon_with_strapline_gradient.svg" alt="Sim by rConfig" width="280" />

# rcfg-sim web UI: architecture and build plan

</div>

| | |
|---|---|
| **Status** | Phases 1 and 2 committed (`ceaea5d`). Phases 3 and 4 implemented, uncommitted. Phase 5 item 0 done; rest of Phase 5 unblocked by the 7.5 rewrite. Open items in [section 17](#17-tracked-follow-ups) |
| **Tracking** | [RCO-1901](https://linear.app/rconfig/issue/RCO-1901/simulator-web-control-ui-for-rconfig-sim-rcfg-sim-ui) |
| **Branch** | `rco-1901-simulator-web-control-ui-for-rconfig-sim-rcfg-sim-ui` |
| **Baseline** | Must contain `f55abe2` (device profiles, RCO-1693). Cut from `origin/dev` (`75ffb93`) or later |
| **Owner** | Stephen |
| **Last updated** | 2026-10-30 |

---

## Contents

1. [How to use this document](#1-how-to-use-this-document)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [Current state of the code](#3-current-state-of-the-code)
4. [Decisions](#4-decisions)
5. [Architecture](#5-architecture)
6. [Admin API (inside `rcfg-sim`)](#6-admin-api-inside-rcfg-sim)
7. [Controller (`rcfg-sim-ui`)](#7-controller-rcfg-sim-ui)
8. [Security model](#8-security-model)
9. [Visual design](#9-visual-design)
10. [Screens](#10-screens)
11. [Repository layout and build](#11-repository-layout-and-build)
12. [Testing strategy](#12-testing-strategy)
13. [Public API changes and versioning](#13-public-api-changes-and-versioning)
14. [Phases](#14-phases)
15. [Open questions](#15-open-questions)
16. [Prompt template for coding agents](#16-prompt-template-for-coding-agents)
17. [Tracked follow-ups](#17-tracked-follow-ups)

---

## 1. How to use this document

This is the single source of truth for the UI work. Every coding-agent prompt for this work points at it.

**Before starting any phase, read sections 1 to 13 in full.** They hold the shared rules, the architecture, and the contracts between components. Then read only the phase section you are working on. Each phase section in [section 14](#14-phases) is self-contained: goal, scope, files, design detail, acceptance criteria and tests.

Rules for anyone (human or agent) working from this document:

- **Check the baseline first.** Run `git merge-base --is-ancestor f55abe2 HEAD`. If it fails, the branch is stale: stop and report, do not write code. `internal/profile` and `internal/sshsrv/fault_bench_test.go` must both exist.

- Follow [CLAUDE.md](../CLAUDE.md). Everything there still applies, especially the breaking-change surface and the CHANGELOG rules.
- **Do not commit, push or create branches.** Git is managed by the maintainer.
- **Do not use git worktrees.**
- **Never run `sudo` or anything that changes the host outside the repo** (system users, `/etc`, systemd units, polkit). Use temp directories and the injectable `--systemctl`. Do not stop or signal processes you did not start.
- **Run only localised tests** for the packages you changed (for example `go test ./internal/fault/...`). Do not run the full suite; the maintainer runs it.
- **No browser-driven test frameworks.** No Playwright, Cypress or Selenium. UI behaviour is tested in Go with `httptest` and template render tests (see [section 12](#12-testing-strategy)).
- Do not start the next phase. Stop at the end of the phase you were given and report what changed.
- If this document and the code disagree, stop and flag it rather than guessing.

---

## 2. Goals and non-goals

### Goals

A browser UI for a running simulator fleet that lets an operator:

- **Control the service**: see every instance, start, stop and restart instances, set capacity (session cap, active device count) and response delay.
- **Set device counts**: an instant soft device count, plus a full rebuild (count, distribution, seed) via the generator.
- **Inject failures**: per-type fault rates with sliders and numeric inputs, device profiles (hang, latency, refuse, disconnect) by percentage, saved presets.
- **Observe activity**: live sessions, throughput, faults by type, a whole-fleet heatmap, and a sampled event tail.

Constraints:

- Stays a static `CGO_ENABLED=0` build with no runtime dependencies.
- Zero cost on the SSH hot path when the UI and admin API are off, and near zero when they are on.
- Works on air-gapped lab networks: no CDN, no external fonts, no telemetry.
- Existing deployments that do not opt in see no change in behaviour.

### Non-goals (v1)

- User accounts, roles, or SSO.
- Adding or removing IP aliases from the UI (needs root; the UI shows the command instead).
- Editing command files or configs in the browser.
- Replacing Prometheus and Grafana for long-term metrics. The UI shows live and recent data only.
- Multi-host fleets. One controller manages the instances on its own host.

---

## 3. Current state of the code

Findings from reviewing `main` at `f55abe2`. These shape the design.

| Area | Today | Impact on the UI |
|---|---|---|
| Fault config | `fault.NewSet` in [cmd/rcfg-sim/main.go](../cmd/rcfg-sim/main.go) builds one immutable `*fault.Set` at startup. | No runtime change possible. Needs an atomic swap. |
| Fault model | `fault.Set` in [internal/fault/inject.go](../internal/fault/inject.go) has one `rate` shared by all types, plus on/off per type. | Per-type sliders need per-type rates. Model change. |
| Device profiles | Resolved once into `map[int]*profile.Profile` (by port) in `sshsrv.New`, [internal/sshsrv/server.go](../internal/sshsrv/server.go). | Needs a rebuild-and-swap path. |
| Session cap | `sem chan struct{}` sized from `--max-concurrent-sessions`. | A channel cannot be resized. Needs a counter-based cap. |
| Response delay | `ResponseDelayMinMS/MaxMS` read from `Config`. | Needs to be swappable. |
| Control plane | None. No admin routes, no SIGHUP handling. | Admin API needed. |
| HTTP | Metrics server serves `/metrics` and `/healthz` on `--metrics-addr`. | Admin routes can share this listener. |
| Activity data | Prometheus counters and histograms only. Session outcomes: `ok`, `error`, `auth_fail`, `disconnect`. | No per-device state, no event stream. Needs both. |
| Process model | One `rcfg-sim@<IP>` systemd instance per IP alias, each with its own metrics port. | The UI must aggregate across instances. |
| systemd unit | `ExecStart` in [deploy/systemd/rcfg-sim@.service](../deploy/systemd/rcfg-sim@.service) does not pass `--device-profiles`, `--profile-seed`, `--ssh-auth`, `--commands-root` or `--max-session`. | Those cannot be set via env files today. Fix in phase 1. |
| Device count | Set by `rcfg-sim-gen` (configs and manifest). Each instance mmaps its configs and binds `port-count` listeners at start. | A count change is a rebuild and restart, not a slider. |
| Dependencies | Three direct dependencies (Prometheus client, x/crypto, x/sys). The docs site advertises this. | Any new dependency is a visible decision (see D7). |

---

## 4. Decisions

| ID | Decision | Reason |
|---|---|---|
| D1 | UI stack is **templ + HTMX + SSE**, styled with **Tailwind CSS v4 via the standalone CLI**, charts with **uPlot**, the fleet heatmap on a plain `<canvas>`. | One static Go binary, no Node in CI, one language for contributors. SSE fits a one-way live feed and works through proxies more reliably than WebSockets. |
| D2 | Two components: an opt-in **admin API inside each `rcfg-sim` process**, and a new **`rcfg-sim-ui` controller binary**. | Device processes stay small. Privileged work (systemd, generator, env files) lives in one place. Scripts can drive the admin API without the UI. |
| D3 | Admin API is **off by default** (`--admin`), shares the metrics listener, and is versioned under `/admin/v1/`. | No behaviour change for existing users. Versioning lets the contract evolve. |
| D4 | Runtime config changes are applied by **atomic pointer swap** and take effect for **new sessions only**. | Keeps the hot path to one atomic load per session. No locking in the session loop. In-flight sessions are never altered mid-stream. |
| D5 | **Per-type fault rates** via a new `--fault-rates` flag. `--fault-rate` and `--fault-types` stay as they are. Setting both forms is an error. | Backward compatible with every existing command line and env file. |
| D6 | The controller **persists changes to `/etc/rcfg-sim/<IP>.env`**. Instances never write their own config. | A restart must never silently revert what the UI shows. |
| D7 | The `templ` runtime and any other UI dependency may only be imported by `cmd/rcfg-sim-ui` and `internal/ui/...`. **`rcfg-sim` and `rcfg-sim-gen` gain no new dependencies.** | Keeps the simulator binary lean and the docs claim true for it. Enforced by a test (see 12). |
| D8 | Generated artefacts (`*_templ.go`, the built CSS) are **committed**. CI fails if they are stale. | `go build` works with only the Go toolchain. Contributors do not need templ or Tailwind to build. |
| D9 | **No login**, but the UI is off unless started, binds to `127.0.0.1:8080` by default, and supports an optional `--ui-token`. | Login-free for the intended use (lab box, SSH tunnel), without exposing fleet control by accident on a public repo people copy from. |
| D10 | All front-end assets (htmx, the htmx SSE extension, uPlot, fonts) are **vendored at pinned versions** and embedded with `go:embed`. | Air-gapped labs. Reproducible builds. |
| D11 | "Service level" in v1 means **service control** (instances, capacity, delays). An **SLA-target mode** is a deferred phase. | The control features are needed regardless. SLA mode builds on them. |

---

## 5. Architecture

### 5.1 Components

```
 Browser
   |  HTML (templ) + HTMX requests + SSE (live data)
   v
+-----------------------------------------------------------------+
| rcfg-sim-ui  (new binary, systemd unit rcfg-sim-ui.service)     |
|                                                                 |
|  internal/ui/web        HTTP handlers, templ views, SSE hub     |
|  internal/ui/fleet      instance discovery, fan-out, aggregation|
|  internal/ui/envfile    read and update /etc/rcfg-sim/*.env     |
|  internal/ui/systemd    start/stop/restart via systemctl        |
|  internal/ui/jobs       generator runs, rolling restarts        |
|  internal/ui/presets    saved failure presets (JSON on disk)    |
+------------------------------+----------------------------------+
                               |  HTTP to each instance's
                               |  --metrics-addr, /admin/v1/*
          +--------------------+--------------------+
          v                    v                    v
+------------------+ +------------------+ +------------------+
| rcfg-sim@10.50.0.1| | rcfg-sim@10.50.0.2| ...  (up to N)  |
|  SSH listeners    | |  SSH listeners    |                  |
|  /metrics /healthz| |  /metrics /healthz|                  |
|  /admin/v1/*      | |  /admin/v1/*      |                  |
+------------------+ +------------------+ +------------------+
```

### 5.2 Inside `rcfg-sim` (new pieces)

```
internal/runtime        (new) Live config holder:
                          atomic.Pointer[Live] with faults, profiles,
                          delays, session cap, active device count.
                          A generation counter bumps on every swap.
internal/activity       (new) Event bus (ring buffer, sampled, zero cost
                          with no subscribers) and per-device state table.
internal/sshsrv         Reads runtime.Live once per session.
                          Publishes events and device state at session end.
internal/admin          (new) /admin/v1/* handlers on the metrics mux.
```

**Session path after the change:**

1. Accept. Load `live := s.runtime.Load()` once. This snapshot is used for the whole session.
2. If the device's port index is at or above `live.ActiveDevices`, treat as refused (same as the `refuse` profile, outcome `error`, device state `inactive`).
3. Admission against `live.MaxSessions` using an `atomic.Int64` counter (see phase 1).
4. Profile lookup in `live.Profiles`, faults rolled from `live.Faults`, delays from `live.Delay`.
5. At session end: one atomic store into the device state table; publish an event only if the bus has subscribers and the sample roll passes.

### 5.3 State and persistence

| State | Owner | Lifetime | Persisted where |
|---|---|---|---|
| Faults, profiles, delays, session cap, active count | Instance (in memory) | Until restart | Controller writes the env file on every change |
| Env files `/etc/rcfg-sim/<IP>.env` | Controller | Durable | Disk |
| Device state table | Instance | Until restart | Not persisted |
| Events | Instance ring buffer, then controller hub | Seconds | Not persisted |
| Presets | Controller | Durable | `/var/lib/rcfg-sim-ui/presets/*.json` |
| Build jobs | Controller | Until controller restart | Job log on disk |

**Drift.** On every poll the controller compares each instance's live config (from `/admin/v1/state`) with its env file. A mismatch is shown as a drift badge on the instance card with a "re-apply" action. Re-apply always pushes the **env file to the instance**: the file is the durable source of truth. This catches manual edits and failed writes.

### 5.4 Fleet-wide versus per-instance changes

The UI works at fleet level by default. The controller turns a fleet change into per-instance calls:

- Faults, profiles, delays: the same values are sent to every instance.
- Session cap: entered per instance (that is how the flag works) with a fleet total shown.
- Active devices: the fleet target is split across instances in proportion to each instance's device count, rounded so the total is exact.
- Profile percentages stay fleet-correct because selection is by seed and hostname, not by instance (see `internal/profile`).

Apply order is parallel with a short timeout per instance. The result screen lists any instance that failed.

- **Up instances**: the env file is updated with **exactly the keys each instance accepted**. If faults are accepted and profiles are rejected, the fault keys are written and the profile keys are not.
- **Down instances**: the env file is written anyway, and the result screen marks the instance "applies on next start". Otherwise a stopped instance would come back on old settings with no drift to show for it.
- **Admin API off, token rejected, bad env file, port clash**: skipped and listed. Nothing is written.
- **Active devices**: the fleet target is split across up **and** down instances, using `PORT_COUNT` from the env file for down instances, so a down instance's `ACTIVE_DEVICES` is saved for its next start.

---

## 6. Admin API (inside `rcfg-sim`)

Enabled by `--admin` (default `false`). Mounted on the `--metrics-addr` mux under `/admin/v1/`. If `--admin-token` is set, every admin request must carry `Authorization: Bearer <token>`; `/metrics` and `/healthz` are unaffected. All bodies are JSON. Errors return `4xx` with `{"error": "..."}`. Unknown JSON fields are rejected with `400`, and bodies over 64 KB are refused. Every successful `PUT` that changes something returns the full new state and bumps `generation`. A `PUT` that changes nothing (including an empty `{}`) returns the state without bumping `generation`.

`version` comes from `-X main.version` set by `make build` (`git describe`), falling back to Go build info and then `dev`.

### `GET /admin/v1/state`

```json
{
  "version": "0.0.6",
  "listen_ip": "10.50.0.1",
  "port_start": 10000,
  "port_count": 2500,
  "devices": 2500,
  "started_at": "2026-10-30T09:00:00Z",
  "generation": 7,
  "faults": { "auth_fail": 0.02, "disconnect_mid": 0.005, "slow_response": 0, "malformed": 0 },
  "profiles": { "seed": 42, "rules": ["hang:3600s@3%", "latency:200-800ms@10%"] },
  "delay_ms": { "min": 50, "max": 500 },
  "max_sessions": 5000,
  "active_devices": 2500,
  "active_sessions": 312
}
```

Profile rules use the same string syntax as `--device-profiles`, parsed by the existing `profile.Parse`. One syntax, one parser.

### `PUT /admin/v1/faults`

Body: the `faults` object above. Missing types mean `0`. Each rate must be in `[0,1]`.

### `PUT /admin/v1/profiles`

Body: `{"seed": 42, "rules": ["hang:3600s@3%"]}`. `rules` is required (`[]` clears all rules); `seed` is optional and keeps the current seed when left out. Each string is one rule; all are parsed together with `profile.Parse`, so the 100% total is enforced. State reports the rules exactly as given. Rebuilds the port-to-profile map off the hot path, then swaps. Manifest-pinned profiles (11th column) still win, as today.

### `PUT /admin/v1/behaviour`

Body: `{"delay_ms": {"min": 50, "max": 500}, "max_sessions": 5000}`. Either key may be omitted, but `delay_ms` must carry both `min` and `max`. `0 <= min <= max <= 3600000`, `max_sessions >= 1`.

### `PUT /admin/v1/active`

Body: `{"active_devices": 1200}`. Range `0..port_count`. A device is active when its port minus `port_start` is below the value, so devices are activated in port order. Gaps in the manifest's port range count towards the total.

### `GET /admin/v1/devices`

Compact per-device state for the heatmap:

```json
{ "port_start": 10000, "now": 1761814800, "state": "AQEBAgMB...", "last_seen": "base64 uint32 LE..." }
```

`state` is one byte per device (see the state codes in 6.1). `last_seen` is unix seconds as little-endian `uint32` per device, `0` for never. Base64 keeps 2,500 devices at a few kilobytes.

### `GET /admin/v1/events?sample=0.01`

Server-sent events. Each `data:` line is one JSON event:

```json
{ "t": 1761814800123, "host": "rtr-00042", "port": 10042, "kind": "session_end", "outcome": "auth_fail", "fault": "auth_fail", "bytes": 0, "ms": 412 }
```

`kind` is one of `session_start`, `session_end`, `fault`. `bytes` is bytes sent so far and `ms` is time since session start, for every kind. On `session_end`, `fault` is the session's last fault. `sample` is a probability in `(0,1]`, default `0.01`. Each stream (subscriber) is capped at 200 events per second so a 50k burst cannot flood the controller; the controller holds one stream per instance. Dropped events are counted and reported once a second as `{"kind":"dropped","n":123}`. The instance also keeps a ring of recent events (`Bus.Recent`), not exposed yet; Phase 4 may add an endpoint for the device side panel.

### 6.1 Device state codes

| Code | State | Set when |
|---|---|---|
| 0 | `never` | No session since start |
| 1 | `ok` | Session outcome `ok` |
| 2 | `auth_fail` | Outcome `auth_fail` |
| 3 | `disconnect` | Outcome `disconnect` |
| 4 | `error` | Outcome `error` |
| 5 | `hang` | Device has a `hang` profile and a session hung |
| 6 | `inactive` | Port index at or above `active_devices` (worked out at encode time from the live count) |
| 7 | `active` | A session is in progress. Wins over `inactive` |

Connections refused by the `refuse` profile or the session cap record `error`. Connections refused because the device is inactive update `last_seen` only.

---

## 7. Controller (`rcfg-sim-ui`)

### 7.1 Flags

| Flag | Default | Meaning |
|---|---|---|
| `--ui-addr` | `127.0.0.1:8080` | Listen address for the UI |
| `--ui-token` | empty, falls back to `UI_TOKEN` env | If set, required as a cookie (set via `/login?token=`) or `Authorization: Bearer` header |
| `--env-dir` | `/etc/rcfg-sim` | Where instance env files live; instances are discovered from `*.env` here |
| `--admin-token` | empty, falls back to `ADMIN_TOKEN` env | Token sent to instance admin APIs |
| `--gen-bin` | `/opt/rcfg-sim/bin/rcfg-sim-gen` | Generator binary used by build jobs |
| `--data-dir` | `/var/lib/rcfg-sim-ui` | Presets and job logs |
| `--poll` | `2s` | Instance poll interval |
| `--systemctl` | `systemctl` | Command used for unit control (overridable for tests) |
| `--log-level` | `info` | As `rcfg-sim` |

### 7.2 Instance discovery

Each `*.env` in `--env-dir` (excluding `*.sample`) is one instance. The controller reads `LISTEN_IP`, `PORT_START`, `PORT_COUNT` and `METRICS_ADDR` from it, applying the **same defaults as the systemd unit** for any key left out (`LISTEN_IP` defaults to the file name). When `METRICS_ADDR` binds `0.0.0.0` or `::`, the controller dials `127.0.0.1` on that port.

Instance card states, in order of precedence:

- **Bad env file**: the env file cannot be parsed, so there is no address to dial. Nothing is written to it.
- **Port clash**: two or more instances resolve to the same metrics port (for example both left on the default `0.0.0.0:9100`). Checked before Down because a clashing port's `/healthz` may answer for the wrong instance. Shown as a fault on each affected card.
- **Down**: `/healthz` does not answer. Unit state from `systemctl is-active`.
- **Admin API off**: `/healthz` answers but `/admin/v1/state` returns 404. Start, stop and restart still work. The card shows the line to add (`ADMIN=true`) and the restart needed. The controller does not edit `ADMIN` itself.
- **Token rejected**: `/admin/v1/state` returns 401.
- **Up**.

### 7.3 Env file handling

- The controller **only rewrites keys it owns**: `FAULT_RATES`, `DEVICE_PROFILES`, `PROFILE_SEED`, `RESPONSE_DELAY_MS_MIN`, `RESPONSE_DELAY_MS_MAX`, `MAX_CONCURRENT_SESSIONS`, `ACTIVE_DEVICES`. Comments, ordering and every other line are preserved.
- When writing `FAULT_RATES`, it sets `FAULT_RATE=0` and `FAULT_TYPES=` (empty). `FAULT_RATE` cannot be empty because it must parse as a number. D5 makes a non-zero legacy rate alongside `FAULT_RATES` an error.
- `make install` creates the `rcfgsim-ui` group (the setgid layout needs it). `make install-ui` creates the user.
- Writes are atomic: temp file in the same directory, `fsync`, rename. The temp file takes the original file's mode and group before the rename.

**Permissions.** systemd reads `EnvironmentFile=` as root before the service starts, so device processes need **no** access to `/etc/rcfg-sim`. The layout is:

| Path | Owner | Mode |
|---|---|---|
| `/etc/rcfg-sim/` | `root:rcfgsim-ui` | `2770` (setgid, so new files get group `rcfgsim-ui`) |
| `/etc/rcfg-sim/*.env` | `root:rcfgsim-ui` at install, `rcfgsim-ui:rcfgsim-ui` after the first UI write | `0660` |
| `/opt/rcfg-sim/host-keys/` | `rcfgsim:rcfgsim` | `0700` (unchanged) |

Device processes (user `rcfgsim`) cannot read or write env files, which also keeps `ADMIN_TOKEN` away from them. Installs without the UI keep the same layout with group `rcfgsim-ui` simply unused.

### 7.4 systemd control

Start, stop and restart call `systemctl <verb> rcfg-sim@<IP>.service`. The controller runs as its own system user, **`rcfgsim-ui`**, separate from the `rcfgsim` user the device processes run as. Permission comes from a polkit rule shipped in `deploy/polkit/` that allows `rcfgsim-ui` to manage units matching `rcfg-sim@*.service` and nothing else. A sudoers alternative is documented for hosts without polkit.

A **rolling restart** restarts one instance at a time, waits up to 60 s for `/healthz`, pauses (default 5 s, configurable) and moves on. It stops at the first instance that fails to come back.

### 7.5 Build jobs

Builds never rename or rewrite a live build. Each build lives in its own directory and a symlink says which one is live.

**Data layout (UI-managed installs only):**

```
/opt/rcfg-sim/data/                      root:rcfgsim-ui 2775
├── current -> builds/20261031-0915      symlink, swapped atomically
└── builds/
    ├── 20261031-0915/                   live build
    │   ├── configs/
    │   ├── manifest.csv                 config_file paths point into this directory
    │   └── build.json                   the settings this build was made with
    └── 20261030-1640/                   previous build, kept for rollback
```

Instances run with `MANIFEST=/opt/rcfg-sim/data/current/manifest.csv`. The generator runs with `--output-dir` set to the build's own absolute directory, so the paths it writes into the manifest stay valid for the life of the build. Nothing is ever renamed, so manifest paths never need rewriting. Running instances keep their mmaps of the old build until they restart.

The controller unit adds `/opt/rcfg-sim/data` to `ReadWritePaths=`. It never gets write access to the rest of `/opt/rcfg-sim`, so it cannot replace the simulator binaries. Build output must be readable by `rcfgsim` (files `0644`, directories `0755`), whatever the controller's own `UMask=`.

**Legacy installs are untouched.** The unit's `MANIFEST` and the generator's `--output-dir` defaults do not change, so this is not a breaking change. The Build screen only works when every instance's `MANIFEST` points at `data/current/manifest.csv`. Otherwise it shows the one-off adoption steps (`make adopt-data`, run by the operator with sudo), which:

1. create `/opt/rcfg-sim/data/builds/<timestamp>/`,
2. move the existing `configs/` and `manifest.csv` into it,
3. rewrite the `config_file` prefix in that one manifest (a one-off migration, never done by the controller),
4. write a `build.json` marked `"adopted": true` with whatever settings are known,
5. point `current` at it and set `MANIFEST` in every env file.

**A build:**

1. **Validate the layout.** The existing instances must be a layout the generator can reproduce: consecutive IPs, and the same `PORT_START`, `PORT_COUNT` and `MANIFEST` on every instance. Otherwise refuse with the reason.
2. **Validate the count.** Every instance must get at least one device: `(ip_count - 1) x devices_per_ip < count <= ip_count x devices_per_ip`. The generator fills IPs in order, and an instance with no devices refuses to start. Shrinking the fleet is what the active-devices slider is for; the Build screen says so beside the count field. A count needing more IPs is refused with the exact `deploy/ip-aliases.sh` and env-file steps.
3. **Settings.** Start from the live build's `build.json`, so options the UI does not expose (for example `--rne-dual-home-pct`) carry over. Credentials (username, password, enable password) are reused from the live manifest. An adopted build with unknown settings uses generator defaults, and the Build screen says so.
4. **Estimate** disk and RAM from per-model average sizes (the README size table for the Cisco buckets, a measured sample for TL1 models). Refuse if free space is below the estimate plus 10%.
5. **Generate** into `builds/<timestamp>/`. A failed run deletes that directory and leaves `current` alone.
6. **Swap** `current` atomically (create a temp symlink, then rename it over `current`).
7. **Rolling restart** (7.4). If it stops on a failure, the job offers rollback.
8. **Retention.** Keep the live build and the one before it. Delete older builds only after a successful build and restart. The disk check in step 4 assumes two builds on disk.

**Rollback** points `current` at the previous build and does a rolling restart.

Job progress (generator output, restart steps) streams to the browser over SSE.

---

## 8. Security model

The UI has no login by design. It is still a control plane that can stop the fleet and write gigabytes to disk, so:

- **Off unless installed and started.** `rcfg-sim-ui` is a separate unit, not enabled by `make install` unless asked (`make install-ui`).
- **Loopback by default.** `--ui-addr=127.0.0.1:8080`. Remote use is via SSH tunnel, or by setting the address deliberately.
- **Optional token** (`--ui-token`, `--admin-token`) for shared lab networks. Compared in constant time.
- **Tokens never go on a command line.** Command lines are world-readable via `ps` and `/proc/<pid>/cmdline`. Both binaries read the token from the environment (`ADMIN_TOKEN`, `UI_TOKEN`) when the flag is not set, and the systemd units pass it only through `EnvironmentFile=`, never in `ExecStart`. Env file permissions are in section 7.3; device processes cannot read them.
- **Admin API off by default** in `rcfg-sim` (`--admin=false`).
- **CSRF**: all state-changing requests are `PUT`/`POST` from HTMX with a custom header (`HX-Request`) checked server-side, and `SameSite=Strict` on the token cookie.
- **Narrow privilege**: the controller runs as `rcfgsim-ui`, the device processes as `rcfgsim`, and neither can do the other's job. The polkit rule allows `rcfg-sim@*.service` only. The controller never runs as root and never edits IP aliases.
- **Input validation** on every field server-side. The UI's own validation is a convenience only.
- A startup log line warns if `--ui-addr` is not loopback and no token is set.

---

## 9. Visual design

The UI follows the **Sim by rConfig** brand as used on [simdocs.rconfig.com](https://simdocs.rconfig.com): the Copper scale, Sora and Plus Jakarta Sans, zinc neutrals, dark by default.

Source of truth for tokens: `src/assets/css/tailwind.css` and `src/assets/css/custom.css` in [rconfig-sim-docs](https://github.com/rconfig/rconfig-sim-docs). If those change, this section follows them.

### 9.1 Colour

**Copper (accent) scale**, from Brand Guidelines 2026:

| Token | Hex | Use |
|---|---|---|
| `accent-50` | `#FBF4EF` | Light-mode tinted backgrounds |
| `accent-100` | `#F6E5DA` | Light-mode selected rows |
| `accent-200` | `#F2C6A2` | Gradient stop 3. Dark-mode links and highlights |
| `accent-300` | `#E8A27F` | Dark-mode hover |
| `accent-400` | `#D98B69` | |
| `accent-500` | `#CC7A5C` | Gradient stop 2. Focus rings, slider fill |
| `accent-600` | `#A8562F` | Primary buttons with white text (5.2:1, AA) |
| `accent-700` | `#8C4220` | Gradient stop 1. Light-mode links (7.2:1 on white) |
| `accent-800` | `#6E3317` | Pressed state |
| `accent-900` | `#4F2511` | Dark-mode tinted panels |
| `accent-950` | `#2E1509` | Dark-mode deepest tint |

**Neutrals**: Tailwind `zinc` scale, mapped to `gray` as on the docs site. Dark mode background `zinc-950`, panels `zinc-900`, borders `zinc-800`. Light mode background white, panels `zinc-50`, borders `zinc-200`.

**Brand gradient**: `linear-gradient(90deg, #8C4220, #CC7A5C, #F2C6A2)`. Used sparingly:

- A 3px hairline across the top of the app header (the docs site signature).
- The fill of the primary "Apply" bar when there are unapplied changes.
- Nowhere else. Data colours are never the brand gradient.

**Status and data colours** (distinct from copper so status never reads as "brand"):

| State | Dark mode | Light mode |
|---|---|---|
| ok | `emerald-400` | `emerald-600` |
| active | `sky-400` | `sky-600` |
| auth_fail | `amber-400` | `amber-600` |
| disconnect | `orange-400` | `orange-600` |
| error | `rose-500` | `rose-600` |
| hang | `violet-400` | `violet-600` |
| inactive | `zinc-700` | `zinc-300` |
| never | `zinc-800` | `zinc-200` |

Colour is never the only signal. The heatmap legend shows counts per state, hovering a cell shows its state in text, and fault charts are labelled directly.

### 9.2 Typography

- Headings: **Sora** (variable), weight 400, letter spacing `-0.01em`.
- Body and UI: **Plus Jakarta Sans** (variable).
- Numbers, hostnames, flags, specs: system monospace stack with `font-variant-numeric: tabular-nums`.
- Both fonts are SIL OFL. Vendor the `woff2` files with their licence files and embed them; never load from Google Fonts or a CDN.

### 9.3 Themes

- Dark is the default, as on the docs site. A header toggle offers Dark, Light and Auto (follows `prefers-color-scheme`), stored in `localStorage`.
- All colours are CSS variables on `:root`, redefined for `[data-theme="light"]` and `[data-theme="dark"]`.

### 9.4 Logo and favicon

- Dark mode: `sim-logo-white.svg`. Light mode: `sim-logo-gradient.svg`. Both from `rconfig-sim-docs/src/assets/`. Header height 2rem, as on the docs site.
- Favicon: the docs site `favicon.svg`.

### 9.5 Feel

Engineering tool, not marketing page. Dense but calm: cards with 1px borders, 8px radius, no heavy shadows, generous use of tabular numbers. Motion only where it carries meaning (a live counter ticking, a sparkline moving). Respect `prefers-reduced-motion`.

---

## 10. Screens

Navigation: a left rail with **Fleet**, **Failures**, **Activity**, **Build**, and a header with the logo, a fleet health pill (instances up / total), the theme toggle, and a global "pending changes" bar that appears whenever a form differs from live state.

### 10.1 Fleet (service control)

- One card per instance: IP, port range, card state (7.2), active sessions against cap, bytes/s sparkline (from `rcfgsim_bytes_sent_total` on the instance's `/metrics`, differenced per poll), generation, drift badge.
- Actions per instance and fleet-wide: start, stop, restart, rolling restart.
- **Active devices**: slider plus numeric input, fleet total, with per-instance split shown.
- **Response delay**: a min slider and a max slider, each with a numeric input, kept so min never exceeds max.
- **Session cap**: per-instance input, fleet total shown.

### 10.2 Failures

- **Fault rates**: one row per type (`auth_fail`, `disconnect_mid`, `slow_response`, `malformed`), each with a slider (0 to 10% by default, extendable to 100%), a numeric input, and a live estimate: "about N events per full poll of the fleet".
- **Device profiles**: rows of type, parameters, percent of fleet; seed input. A "who is affected" panel lists the hostnames the seed selects (from `/admin/v1/state` plus the manifest), with copy-to-clipboard so the list can be checked against rConfig's error logs.
- **Presets**: save the current faults and profiles under a name; apply a preset in one click. Ships with three read-only presets, embedded in the binary. Saved presets go to `--data-dir/presets/`.

| Preset | Fault rates | Device profiles (seed 42) |
|---|---|---|
| Flaky WAN | `disconnect_mid=0.03`, `slow_response=0.05` | `latency:300-1500ms@15%` |
| Auth storm | `auth_fail=0.15` | none |
| Slow core | `slow_response=0.02` | `latency:2000-8000ms@5%`, `hang:600s@1%` |

Values are starting points; tune them in review.
- Nothing applies until **Apply** is pressed. The pending bar shows a diff of what will change.

### 10.3 Activity

- **Heatmap**: every device as one cell on a canvas, coloured by state code (6.1), plus a controller-only value `255` ("unreachable") for cells of an instance that is not reporting. 50,000 cells fit as a 250 x 200 grid. The fleet frame is rebuilt every 2 s (fixed, not a flag). The page shows each frame's draw time beside the heatmap. Hover shows hostname, state and last seen; click opens a side panel with that device's profile and its recent events.
- **Event source**: the controller subscribes to each instance with `sample=1` and relies on the instance's 200/s per-stream cap, then caps each browser at 200/s. The device side panel reads the controller's own ring of the last 5,000 events; no instance endpoint for `Bus.Recent` was added.
- **Live charts** (uPlot, 10-minute window, one point per `--poll`): active sessions, sessions per second by outcome, bytes per second, faults per second by type.
- **Event tail**: sampled events, filter by hostname, outcome or fault, pause button.

### 10.4 Build

- Inputs: device count, seed, distribution editor (stacked weight bars across `sm` to `6xl` and the TL1 models, must total 100).
- Estimate panel: disk and RAM, time estimate, and a warning if the count needs more IPs than are configured.
- Run: job log streaming, then rolling restart progress per instance. Rollback button while a previous build exists.
- If the install is not adopted (7.5), the screen shows the adoption steps instead of the form.

---

## 11. Repository layout and build

### 11.1 New and changed paths

```
cmd/rcfg-sim-ui/main.go              new controller entry point
internal/runtime/                    new: live config holder (atomic swap)
internal/activity/                   new: event bus, device state table
internal/admin/                      new: /admin/v1 handlers
internal/fault/                      per-type rates
internal/sshsrv/                     read runtime.Live, publish activity
internal/ui/web/                     handlers, SSE hub, templ views
internal/ui/web/views/*.templ        templates (+ committed *_templ.go)
internal/ui/web/static/              vendored htmx, sse ext, uPlot, fonts, built CSS
internal/ui/web/static/src/app.css   Tailwind source with the @theme tokens
internal/ui/fleet/                   discovery, fan-out, aggregation
internal/ui/envfile/                 env file read and update
internal/ui/systemd/                 systemctl wrapper
internal/ui/jobs/                    build and rolling-restart jobs
internal/ui/presets/                 preset storage
deploy/systemd/rcfg-sim-ui.service   new unit
deploy/polkit/50-rcfg-sim-ui.rules   new polkit rule
docs/UI-ARCHITECTURE.md              this document
```

### 11.2 Toolchain

| Tool | Use | Pinned how |
|---|---|---|
| Go 1.26+ | Everything | `go.mod` (`go 1.26.0`). CI tests 1.26 and 1.27 |
| `templ` CLI and runtime | `templ generate` | Version in `Makefile`, run via `go run github.com/a-h/templ/cmd/templ@<version>`. Currently v0.3.1100. Pin the newest templ release whose `go` line is no higher than the `go` line in this repo's `go.mod`. **Never raise `go.mod` to suit templ**; a floor change is a maintainer decision |
| Tailwind v4 standalone CLI | Build `static/app.css` | Version and SHA-256 in `Makefile`; downloaded to `.tools/` |
| htmx, htmx-ext-sse, uPlot | Front end | Vendored files with version in filename and a `VERSIONS` file |

### 11.3 Makefile targets (new)

- `make ui-gen`: run `templ generate` and the Tailwind build.
- `make ui-check`: regenerate into a temp dir and fail if committed output differs (used by CI).
- `make build` also builds `bin/rcfg-sim-ui`.
- `make install-ui`: install the binary, unit and polkit rule. Not part of `make install`.

### 11.4 CI

Add a job that runs `make ui-check` and the import-boundary test (12.4). No Node, no browsers.

---

## 12. Testing strategy

All tests are Go tests. No browser-driven frameworks.

### 12.1 Unit

- `internal/fault`: per-type parsing, validation, `Roll` per type, legacy form equivalence (`--fault-rate 0.1 --fault-types a,b` equals `--fault-rates a=0.1,b=0.1`).
- `internal/runtime`: swap semantics, generation counter, concurrent `Load` during `Store` under `-race`.
- `internal/activity`: ring buffer wrap, sampling, rate cap, dropped counts, device state encode/decode.
- `internal/ui/envfile`: preserves comments and unknown keys, atomic write, clears legacy fault keys.
- `internal/ui/fleet`: active-device split rounds to the exact total; partial failure handling.

### 12.2 HTTP and templates

- Admin handlers with `httptest`: validation errors, token enforcement, state round trip.
- Controller handlers with `httptest` against fake instances (an `httptest.Server` per instance).
- templ components rendered to a buffer and checked for key elements and attributes (for example the HTMX target and the CSRF header hook).

### 12.3 Benchmarks (hot path guarantees)

- Session path with admin off: no new allocations versus `main` (extend the style of `internal/sshsrv/fault_bench_test.go`).
- Event publish with zero subscribers: `0 allocs/op`.
- Device state store: `0 allocs/op`.

### 12.4 Import boundary

A test that runs `go list -deps` for `./cmd/rcfg-sim` and `./cmd/rcfg-sim-gen` and fails if any `internal/ui/...` package or the templ module appears (D7).

### 12.5 Integration

Behind the existing `integration` build tag: start a real `rcfg-sim` on loopback with `--admin`, change faults via the API, open SSH sessions, assert the new rates take effect for new sessions and `/metrics` reflects them.

---

## 13. Public API changes and versioning

Everything below becomes public API under [CLAUDE.md](../CLAUDE.md) and needs README and CHANGELOG updates in the same phase that adds it. simdocs lives in a separate repo ([rconfig-sim-docs](https://github.com/rconfig/rconfig-sim-docs)): agents do not edit it, but each phase report must list the simdocs pages that need changing.

| Kind | Name | Default | Phase |
|---|---|---|---|
| `rcfg-sim` flag | `--fault-rates` | empty | 1 |
| `rcfg-sim` flag | `--active-devices` | `all` (accepts `all` or `0..port-count`) | 1 |
| `rcfg-sim` flag | `--admin` | `false` | 2 |
| `rcfg-sim` flag | `--admin-token` | empty | 2 |
| Env var | `FAULT_RATES`, `ACTIVE_DEVICES`, `DEVICE_PROFILES`, `PROFILE_SEED`, `SSH_AUTH`, `COMMANDS_ROOT`, `MAX_SESSION` | see unit | 1 |
| Env var | `ADMIN`, `ADMIN_TOKEN` | `false`, empty | 2 |
| HTTP | `/admin/v1/*` | off | 2 |
| Binary | `rcfg-sim-ui` and its flags (7.1) | n/a | 3 |
| Unit | `rcfg-sim-ui.service` | not enabled | 3 |
| Env file | `/etc/rcfg-sim-ui/rcfg-sim-ui.env`: `UI_ADDR`, `ENV_DIR`, `GEN_BIN`, `DATA_DIR`, `POLL`, `SYSTEMCTL`, `LOG_LEVEL`, `UI_TOKEN`, `ADMIN_TOKEN` | see sample | 3 |
| System user | `rcfgsim-ui` (group created by `make install`, user by `make install-ui`) | n/a | 3 |
| Makefile | `install-ui`, `uninstall-ui`, `ui-gen`, `ui-check` | n/a | 3 |
| **Breaking** | Unit `HOST_KEY` default moves to `/opt/rcfg-sim/host-keys/%i.key`; `/etc/rcfg-sim` becomes `root:rcfgsim-ui 2770` and is no longer writable by `rcfgsim` | n/a | 3 |

No existing flag, metric name or label changes. No new Prometheus labels; the device state and events are exposed via the admin API, not `/metrics`, to keep cardinality bounded.

Phases 1 and 2 are additive. Phase 3 has a breaking change (the host-key default and `/etc/rcfg-sim` permissions), recorded under `### Breaking` with a `#### Migration` snippet, which pre-1.0 allows in a patch or minor bump. Phase 3 also moves `golang.org/x/sys` from v0.38 to v0.48 (pulled in by templ); `rcfg-sim` links it, so note it in the CHANGELOG.

---

## 14. Phases

Each phase ends in a working, mergeable state. Do not start a phase until the previous one is merged.

### Phase 1: Runtime-swappable config, per-type fault rates, systemd gaps

**Goal.** Make every behaviour the UI will control changeable at runtime, safely, without a control plane yet. Fix the systemd unit so all existing flags can be set from env files.

**Scope.**

1. `internal/fault`: change `Set` to hold `rates [numTypes]float64`. Add `NewSetFromRates(map or CSV)` for `--fault-rates auth_fail=0.02,disconnect_mid=0.005`. Keep `NewSet(typesCSV, rate)` producing the equivalent per-type rates. `Enabled(t)` is `rates[t] > 0`. `Roll` uses `rates[t]`. Keep the nil-safe, no-RNG-when-disabled fast path. Add `Rates()` for reporting.
2. `cmd/rcfg-sim`: add `--fault-rates`. Error if it is set together with a non-zero `--fault-rate` or non-empty `--fault-types`. Add `--active-devices` (default: all devices in this instance).
3. `internal/runtime` (new): a `Live` struct with `Faults *fault.Set`, `Profiles map[int]*profile.Profile`, `DelayMin, DelayMax time.Duration`, `MaxSessions int64`, `ActiveDevices int`, `Generation uint64`. A `Holder` with `Load() *Live` and `Store(*Live)` backed by `atomic.Pointer[Live]`, bumping `Generation` on store. `Live` values are immutable once stored; changes build a new `Live`.
4. `internal/sshsrv`: read `s.runtime.Load()` once at the top of `handleConn` and use that snapshot for the whole session (faults, profile, delays). Replace `sem chan struct{}` with an `atomic.Int64` active counter checked against `live.MaxSessions` (increment, compare, decrement on reject), keeping the current log line and `error` outcome on rejection. Add the active-devices check before admission, behaving exactly like the `refuse` profile. Move `resolveProfiles` into a function the runtime can call again later.
5. `deploy/systemd/rcfg-sim@.service`: add `Environment=` defaults before `EnvironmentFile=` for every variable the unit uses, so existing env files keep working, and pass `--fault-rates`, `--active-devices`, `--device-profiles`, `--profile-seed`, `--ssh-auth`, `--commands-root`, `--max-session`. Update `rcfg-sim-instance.env.sample`.

**Out of scope.** Any HTTP endpoint. Any UI code.

**Acceptance criteria.**

- All existing command lines and env files behave identically (same faults, same profiles, same cap behaviour).
- `--fault-rates` works for each type independently.
- Swapping a `Live` in a test changes behaviour for new sessions and leaves in-flight sessions on their snapshot.
- Benchmarks show no new allocations on the session path versus `main`.
- README, CHANGELOG `[Unreleased]` and the env sample are updated.

**Tests.** `internal/fault` (new and legacy forms, validation), `internal/runtime` (swap, `-race`), the affected `internal/sshsrv` tests and a benchmark for the session admission path. Localised only.

---

### Phase 2: Admin API and activity in `rcfg-sim`

**Goal.** Expose and change live config over HTTP, and produce the activity data the UI needs, with no cost when unused.

**Scope.**

1. `internal/activity` (new):
   - `Bus`: fixed-size ring buffer of events, `atomic.Int32` subscriber count, `Publish` returns immediately when the count is zero. Per-subscriber channels with drop-on-full and a dropped counter. A token-bucket rate cap (default 200/s).
   - `DeviceTable`: one `atomic.Uint32` for `last_seen` and one `atomic.Uint32` packing state per device, indexed by port offset. Encoders for the base64 formats in section 6.
2. `internal/sshsrv`: set state `active` at session start, set the final state and `last_seen` at session end, publish `session_start`, `session_end` and `fault` events through the bus. Map outcomes to the state codes in 6.1.
3. `internal/runtime`: add `Holder.Update(func(*Live) error) (*Live, error)`, which holds the store mutex across load, change and store. Phase 1's `Store` is serialised but a handler doing `Load`, change, `Store` can still lose a concurrent change (two PUTs to different endpoints at once). All admin writes go through `Update`.
4. `internal/sshsrv`: expose the active-session count (`Server.ActiveSessions() int64`) for `/admin/v1/state`.
5. `internal/admin` (new): handlers for every endpoint in section 6, mounted on the existing metrics mux when `--admin` is set. Bearer token check when `--admin-token` is set. `PUT` handlers validate, then apply the change through `Holder.Update`. Profile rebuilds (`Server.ResolveProfiles`) happen in the handler goroutine, not the session path.
6. `cmd/rcfg-sim`: `--admin`, `--admin-token`. Unit and env sample updated (`ADMIN`, `ADMIN_TOKEN`).

**Out of scope.** The controller binary. Any HTML.

**Acceptance criteria.**

- With `--admin` unset, `/admin/v1/*` returns 404 and the benchmarks match phase 1.
- Every endpoint in section 6 works as documented, including validation errors.
- Two concurrent `PUT`s to different endpoints both survive (test under `-race`).
- `Publish` with zero subscribers and `DeviceTable` stores are `0 allocs/op`.
- An SSE client receives sampled events and periodic `dropped` counts under load.
- README section "Admin API" and CHANGELOG updated.

**Tests.** `internal/activity` unit tests and benchmarks, `internal/admin` `httptest` tests, one integration test behind the `integration` tag that changes faults over the API and observes the effect on new SSH sessions.

---

### Phase 3: Controller with Fleet and Failures screens

**Goal.** A usable UI that can see every instance and change service settings and failures fleet-wide, with changes persisted.

**Scope.**

0. **Phase 2 follow-ups, do first:**
   - `rcfg-sim` reads `ADMIN_TOKEN` from the environment when `--admin-token` is unset. Remove `--admin-token` from the unit's `ExecStart` (section 8). Update README, env sample and CHANGELOG.
   - A `PUT` that changes nothing no longer bumps `generation` (section 6). Add a test.
   - Permissions per section 7.3. `make install` and the README agree: `/etc/rcfg-sim` is `root:rcfgsim-ui 2770`, env files `0660`. `make install-ui` creates the `rcfgsim-ui` user. Fix the Makefile step that chowns `/etc/rcfg-sim` to `rcfgsim`.
   - The unit's `HOST_KEY` default becomes `/opt/rcfg-sim/host-keys/%i.key` (the README's recommended path) and `/etc/rcfg-sim` is removed from `ReadWritePaths=`. Anyone relying on the old `--host-key` default under `/etc/rcfg-sim` must move the key: record under `### Breaking` with a `#### Migration` snippet.
1. `cmd/rcfg-sim-ui` with the flags in 7.1.
2. `internal/ui/fleet`: discovery from env files, polling `/healthz` and `/admin/v1/state`, fan-out of changes (5.4), drift detection (5.3).
3. `internal/ui/envfile`: as 7.3.
4. `internal/ui/systemd`: start, stop, restart, rolling restart, `is-active`. The `systemctl` command is injectable for tests.
5. `internal/ui/presets`: JSON files in `--data-dir`, three shipped presets.
6. `internal/ui/web`: layout, header, nav, theme toggle, Fleet screen (10.1) and Failures screen (10.2), pending-changes bar, token handling (section 8).
7. Visual design per section 9: Tailwind v4 `@theme` with the copper scale, zinc neutrals, both themes, vendored fonts and logos.
8. Build plumbing per section 11: `make ui-gen`, `make ui-check`, CI job, import-boundary test, `deploy/systemd/rcfg-sim-ui.service`, `deploy/polkit/50-rcfg-sim-ui.rules`, `make install-ui`.

**Out of scope.** Heatmap, live charts, event tail, build jobs.

**Acceptance criteria.**

- With three local `rcfg-sim` instances on loopback, the UI shows all three, changes fault rates and profiles across all of them, and the env files reflect the change.
- Stopping one instance shows it as down; restarting it brings it back with the persisted settings.
- Drift is shown when an env file is edited by hand.
- Both themes match section 9. No external network requests from the page.
- `rcfg-sim` and `rcfg-sim-gen` gain no new dependencies (boundary test passes).

**Tests.** `httptest` tests for each handler against fake instances, `envfile` and `fleet` unit tests, templ render tests, the boundary test.

---

### Phase 4: Activity

**Goal.** Live observation of the fleet.

**Scope.**

0. **Phase 3 follow-ups, do first** (section 5.4): write the env file for down instances ("applies on next start"), and on a partial Apply write exactly the keys each instance accepted. Add tests for both.
0. **Then:** vendor htmx-ext-sse and uPlot at pinned versions (add to `VERSIONS`; the same-origin CSP already covers them), as section 11.2 requires.
1. Controller SSE hub: one upstream SSE connection per instance to `/admin/v1/events`, merged and fanned out to browsers. One shared poller for `/admin/v1/devices`, assembling a fleet frame every 2 seconds.
2. Rates for charts computed in the controller by scraping each instance's `/metrics` on the poll interval and differencing counters. No Prometheus server required.
3. Activity screen (10.3): canvas heatmap with legend counts, hover and click panel; uPlot charts; event tail with filters and pause.

**Out of scope.** Historical storage beyond the in-memory chart window.

**Acceptance criteria.**

- Heatmap renders 50,000 cells smoothly (frame updates under 16 ms on a typical laptop) and reflects device state within one poll interval.
- Charts and event tail update live and stop cleanly when the browser tab closes (no leaked upstream connections).
- With no browser connected, the controller holds no upstream event connections.

**Tests.** Hub fan-out and teardown tests, frame assembly tests, counter differencing tests (including counter resets on instance restart).

---

### Phase 5: Build jobs and rolling restart

**Goal.** Change device count, distribution and seed from the UI safely.

**Scope.**

0. **Phase 4 follow-up, do first** (section 5.4): include down instances in the active-device split, using `PORT_COUNT` from their env files, and write their `ACTIVE_DEVICES`. Add a test.

Then section 7.5 in full: the `data/` layout and symlink swap, `make adopt-data`, layout and count validation, `build.json` carry-over, disk and RAM estimate, generation, rolling restart, rollback, retention, streamed job log. Build screen (10.4). Add `/opt/rcfg-sim/data` to the controller unit's `ReadWritePaths=`.

**Out of scope.** Adding IP aliases or new instances (the UI shows the steps instead).

**Acceptance criteria.**

- A build that fits the current layout completes, the fleet comes back on the new manifest, and the previous build can be rolled back.
- A count that would leave an instance with no devices is refused before anything is written.
- A legacy (not adopted) install shows the adoption steps and builds nothing.
- Unit defaults and generator defaults are unchanged (no breaking change).
- A build that does not fit is refused before anything is written, with the exact manual steps shown.
- A build with insufficient disk is refused before generation starts.
- A failed generator run leaves the live configs untouched.

**Tests.** `jobs` tests with a fake generator binary and fake `systemctl`, estimate tests, symlink swap, rollback and retention tests on a temp directory, count-bounds tests, and a test for the adoption script against scratch files (never against `/opt`).

---

### Phase 6 (deferred): SLA-target mode

**Goal.** Let an operator state a target service level (for example "p95 command latency 800 ms, 1% session errors") and have the controller adjust delays, latency profiles and fault rates to hold the fleet at that level.

Not started until phases 1 to 5 are in use and the need is confirmed. Design to be added here before any work begins.

---

## 15. Open questions

1. **SLA-target mode**: worth building? Decide after phase 5.
2. **Logo SVGs**: the logo files in both repos use gradient stops `#7C3411`, `#CC785C`, `#F2C7A8`, while the docs CSS uses the Brand Guidelines 2026 values `#8C4220`, `#CC7A5C`, `#F2C6A2`. The UI uses the guideline values for CSS and the logo files as they are. Should the logo SVGs be updated to match?
3. **Docs claim**: simdocs says the project depends on three external libraries. After phase 3 that is true for `rcfg-sim` only. Reword on the docs site now that Phase 3 has landed.
4. **Go floor**: resolved. The maintainer raised the floor to Go 1.26; CI and the README match.
5. **Milestone**: tracked under rConfig V8 8.4.2 for now; rcfg-sim versions separately (0.0.x).

---

## 16. Prompt template for coding agents

Copy, fill in the phase number, and send.

```text
You are working in the rconfig-sim repository.

Read docs/UI-ARCHITECTURE.md sections 1 to 13 in full, then section 14,
"Phase <N>". Also read CLAUDE.md. Implement Phase <N> only, exactly as
specified. Do not start any other phase.

Rules:
- Do not commit, push or create branches.
- Do not use git worktrees.
- Never run sudo or change anything on the host outside the repo.
- Run only localised tests for the packages you change, for example
  go test ./internal/fault/... Do not run the full test suite.
- No browser-driven test frameworks (no Playwright, Cypress or Selenium).
- Run gofmt and go vet on the packages you change.
- Update README.md and the [Unreleased] section of CHANGELOG.md for any
  public API change, as section 13 requires.
- If the code and the document disagree, stop and report it rather than
  guessing.

When done, report: files changed, public API added, tests run and their
results, anything in the document that needs updating, and new items for
section 17 (manual checks, simdocs pages, follow-ups).
```

---

## 17. Tracked follow-ups

The running list of open items across all phases. Update it at the end of every phase. Agents add items here in their report; the maintainer ticks them off.

### Maintainer: git and review

- [ ] Commit Phase 3, Phase 4 and Phase 5 item 0 (all uncommitted as of the Phase 5 item 0 report). Commit before the rest of Phase 5.
- [ ] Check the dev host after the Phase 3 `sudo` incident: nothing new under `/etc`, no new units or polkit rules.
- [ ] Run the full test suite after Phases 3 and 4.

### Maintainer: manual checks agents cannot do

- [ ] Themes (Phase 3): Dark, Light and Auto. Logo swaps, gradient hairline on the header, copper buttons readable in both.
- [ ] Heatmap frame time (Phase 4): read the draw time shown beside the heatmap with a 50,000-device frame. Target under 16 ms.
- [ ] Polkit rule on a real lab host with the `rcfgsim-ui` user: start, stop and restart work for `rcfg-sim@<IP>.service` and nothing else.
- [ ] Host-key migration snippet run on a real install before release.
- [ ] `make adopt-data` run on a real install (copy of production layout) before release.

### Code follow-ups

- [x] Active-device split includes down instances (Phase 5 item 0).

### simdocs ([rconfig-sim-docs](https://github.com/rconfig/rconfig-sim-docs))

- [ ] Requirements: Go 1.26+.
- [ ] CLI reference, `rcfg-sim`: `--fault-rates`, `--active-devices` (P1), `--admin`, `--admin-token` and its `ADMIN_TOKEN` fallback (P2, P3).
- [ ] CLI reference: new `rcfg-sim-ui` binary and its flags, with `UI_TOKEN` and `ADMIN_TOKEN` fallbacks (P3).
- [ ] Install and env-file pages: `FAULT_RATES`, `ACTIVE_DEVICES`, `DEVICE_PROFILES`, `PROFILE_SEED`, `SSH_AUTH`, `COMMANDS_ROOT`, `MAX_SESSION` (P1); `ADMIN`, `ADMIN_TOKEN` (P2); unit defaults now apply to keys left out (P1).
- [ ] Install pages: `/etc/rcfg-sim` permissions, the `rcfgsim-ui` group and user, `make install-ui`, the new host-key default and its migration (P3).
- [ ] Fault injection page: per-type rates, `FAULT_RATE=0` alongside `FAULT_RATES`, link to runtime changes over the API (P1, P2).
- [ ] Active devices: new section matching the README (P1).
- [ ] New Admin API page matching the README, including "a PUT that changes nothing does not bump `generation`" (P2, P3).
- [ ] New Web UI page: Fleet, Failures, Activity, presets, and Apply behaviour (down instances apply on next start, partial apply) (P3, P4).
- [ ] Reword the "three external libraries" claim: true for `rcfg-sim` only (P3).
- [ ] Web UI page: the active-devices split includes down instances (weighted by `PORT_COUNT`) and saves their share for next start (P5).
- [ ] Build screen, the `data/` layout, `make adopt-data`, rollback and retention (after P5).

### Decisions pending

- [ ] Logo SVG gradient stops versus Brand Guidelines 2026 (section 15, Q2).
- [ ] SLA-target mode, after Phase 5 (section 15, Q1).
- [ ] Linear milestone: RCO-1901 sits in rConfig V8 8.4.2 (target 30 November); move if the work runs past it.
