<div align="center">

<img src="images/rconfig_sim_logo_and_icon_with_strapline_gradient.svg" alt="Sim by rConfig" width="280" />

# rcfg-sim web UI: architecture and build plan

</div>

| | |
|---|---|
| **Status** | Phase 1 implemented, in review. Phases 2 to 5 not started |
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

---

## 1. How to use this document

This is the single source of truth for the UI work. Every coding-agent prompt for this work points at it.

**Before starting any phase, read sections 1 to 13 in full.** They hold the shared rules, the architecture, and the contracts between components. Then read only the phase section you are working on. Each phase section in [section 14](#14-phases) is self-contained: goal, scope, files, design detail, acceptance criteria and tests.

Rules for anyone (human or agent) working from this document:

- **Check the baseline first.** Run `git merge-base --is-ancestor f55abe2 HEAD`. If it fails, the branch is stale: stop and report, do not write code. `internal/profile` and `internal/sshsrv/fault_bench_test.go` must both exist.

- Follow [CLAUDE.md](../CLAUDE.md). Everything there still applies, especially the breaking-change surface and the CHANGELOG rules.
- **Do not commit, push or create branches.** Git is managed by the maintainer.
- **Do not use git worktrees.**
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

**Drift.** On every poll the controller compares each instance's live config (from `/admin/v1/state`) with its env file. A mismatch is shown as a drift badge on the instance card with a "re-apply" action. This catches manual edits and failed writes.

### 5.4 Fleet-wide versus per-instance changes

The UI works at fleet level by default. The controller turns a fleet change into per-instance calls:

- Faults, profiles, delays: the same values are sent to every instance.
- Session cap: entered per instance (that is how the flag works) with a fleet total shown.
- Active devices: the fleet target is split across instances in proportion to each instance's device count, rounded so the total is exact.
- Profile percentages stay fleet-correct because selection is by seed and hostname, not by instance (see `internal/profile`).

Apply order is parallel with a short timeout per instance. The result screen lists any instance that failed, and the env file is only updated for instances that accepted the change.

---

## 6. Admin API (inside `rcfg-sim`)

Enabled by `--admin` (default `false`). Mounted on the `--metrics-addr` mux under `/admin/v1/`. If `--admin-token` is set, every admin request must carry `Authorization: Bearer <token>`; `/metrics` and `/healthz` are unaffected. All bodies are JSON. Errors return `4xx` with `{"error": "..."}`. Every successful `PUT` returns the full new state and bumps `generation`.

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

Body: `{"seed": 42, "rules": ["hang:3600s@3%"]}`. Rebuilds the port-to-profile map off the hot path, then swaps. Manifest-pinned profiles (11th column) still win, as today.

### `PUT /admin/v1/behaviour`

Body: `{"delay_ms": {"min": 50, "max": 500}, "max_sessions": 5000}`. Either key may be omitted. `min <= max`, `max_sessions >= 1`.

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

`kind` is one of `session_start`, `session_end`, `fault`. `sample` is a probability in `(0,1]`, default `0.01`, capped by the instance at a maximum event rate (default 200 per second) so a 50k burst cannot flood the controller. Dropped events are counted and reported in a periodic `{"kind":"dropped","n":123}` event.

### 6.1 Device state codes

| Code | State | Set when |
|---|---|---|
| 0 | `never` | No session since start |
| 1 | `ok` | Session outcome `ok` |
| 2 | `auth_fail` | Outcome `auth_fail` |
| 3 | `disconnect` | Outcome `disconnect` |
| 4 | `error` | Outcome `error` |
| 5 | `hang` | Device has a `hang` profile and a session hung |
| 6 | `inactive` | Port index at or above `active_devices` |
| 7 | `active` | A session is in progress |

---

## 7. Controller (`rcfg-sim-ui`)

### 7.1 Flags

| Flag | Default | Meaning |
|---|---|---|
| `--ui-addr` | `127.0.0.1:8080` | Listen address for the UI |
| `--ui-token` | empty | If set, required as a cookie (set via `/login?token=`) or `Authorization: Bearer` header |
| `--env-dir` | `/etc/rcfg-sim` | Where instance env files live; instances are discovered from `*.env` here |
| `--admin-token` | empty | Token sent to instance admin APIs |
| `--gen-bin` | `/opt/rcfg-sim/bin/rcfg-sim-gen` | Generator binary used by build jobs |
| `--data-dir` | `/var/lib/rcfg-sim-ui` | Presets and job logs |
| `--poll` | `2s` | Instance poll interval |
| `--systemctl` | `systemctl` | Command used for unit control (overridable for tests) |
| `--log-level` | `info` | As `rcfg-sim` |

### 7.2 Instance discovery

Each `*.env` in `--env-dir` (excluding `*.sample`) is one instance. The controller reads `LISTEN_IP`, `PORT_START`, `PORT_COUNT` and `METRICS_ADDR` from it. An instance whose `/healthz` does not answer is shown as down, with its unit state from `systemctl is-active`.

### 7.3 Env file handling

- The controller **only rewrites keys it owns**: `FAULT_RATES`, `DEVICE_PROFILES`, `PROFILE_SEED`, `RESPONSE_DELAY_MS_MIN`, `RESPONSE_DELAY_MS_MAX`, `MAX_CONCURRENT_SESSIONS`, `ACTIVE_DEVICES`. Comments, ordering and every other line are preserved.
- When writing `FAULT_RATES`, it clears `FAULT_RATE` and `FAULT_TYPES` (D5 makes setting both an error).
- Writes are atomic: temp file in the same directory, `fsync`, rename.

### 7.4 systemd control

Start, stop and restart call `systemctl <verb> rcfg-sim@<IP>.service`. The controller runs as the `rcfgsim` user. Permission comes from a polkit rule shipped in `deploy/polkit/` that allows `rcfgsim` to manage units matching `rcfg-sim@*.service` and nothing else. A sudoers alternative is documented for hosts without polkit.

A **rolling restart** restarts one instance at a time and waits for `/healthz` before moving on, with a configurable pause.

### 7.5 Build jobs

A build runs `rcfg-sim-gen` into a staging directory, then swaps directories and does a rolling restart:

1. Validate: `count <= ip_count x devices_per_ip` for the **existing** IP layout. A layout change (more IPs) is refused with the exact `deploy/ip-aliases.sh` and env-file steps needed, because adding aliases needs root.
2. Estimate disk and RAM from per-model average sizes before running, and refuse if free space on the target filesystem is below the estimate plus 10%.
3. Generate into `configs.new` and `manifest.csv.new`.
4. Rename `configs` to `configs.prev`, `configs.new` to `configs`, same for the manifest. Running instances keep their existing mmaps, which stay valid after the rename.
5. Rolling restart.
6. Keep `configs.prev` until the next successful build, so a rollback is a rename.

Job progress (generator output, restart steps) streams to the browser over SSE.

---

## 8. Security model

The UI has no login by design. It is still a control plane that can stop the fleet and write gigabytes to disk, so:

- **Off unless installed and started.** `rcfg-sim-ui` is a separate unit, not enabled by `make install` unless asked (`make install-ui`).
- **Loopback by default.** `--ui-addr=127.0.0.1:8080`. Remote use is via SSH tunnel, or by setting the address deliberately.
- **Optional token** (`--ui-token`, `--admin-token`) for shared lab networks. Compared in constant time.
- **Admin API off by default** in `rcfg-sim` (`--admin=false`).
- **CSRF**: all state-changing requests are `PUT`/`POST` from HTMX with a custom header (`HX-Request`) checked server-side, and `SameSite=Strict` on the token cookie.
- **Narrow privilege**: the polkit rule allows `rcfg-sim@*.service` only. The controller never runs as root and never edits IP aliases.
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

- One card per instance: IP, port range, unit state, active sessions against cap, bytes/s sparkline, generation, drift badge.
- Actions per instance and fleet-wide: start, stop, restart, rolling restart.
- **Active devices**: slider plus numeric input, fleet total, with per-instance split shown.
- **Response delay**: dual-handle range slider (min/max ms) plus inputs.
- **Session cap**: per-instance input, fleet total shown.

### 10.2 Failures

- **Fault rates**: one row per type (`auth_fail`, `disconnect_mid`, `slow_response`, `malformed`), each with a slider (0 to 10% by default, extendable to 100%), a numeric input, and a live estimate: "about N events per full poll of the fleet".
- **Device profiles**: rows of type, parameters, percent of fleet; seed input. A "who is affected" panel lists the hostnames the seed selects (from `/admin/v1/state` plus the manifest), with copy-to-clipboard so the list can be checked against rConfig's error logs.
- **Presets**: save the current faults and profiles under a name; apply a preset in one click. Ships with three: "Flaky WAN", "Auth storm", "Slow core".
- Nothing applies until **Apply** is pressed. The pending bar shows a diff of what will change.

### 10.3 Activity

- **Heatmap**: every device as one cell on a canvas, coloured by state code (6.1). 50,000 cells fit as a 250 x 200 grid. Hover shows hostname, state and last seen; click opens a side panel with that device's profile and its recent events.
- **Live charts** (uPlot, 10-minute window): active sessions, sessions per second by outcome, bytes per second, faults per second by type.
- **Event tail**: sampled events, filter by hostname, outcome or fault, pause button.

### 10.4 Build

- Inputs: device count, seed, distribution editor (stacked weight bars across `sm` to `6xl` and the TL1 models, must total 100).
- Estimate panel: disk and RAM, time estimate, and a warning if the count needs more IPs than are configured.
- Run: job log streaming, then rolling restart progress per instance. Rollback button while `configs.prev` exists.

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
| Go 1.24+ | Everything | `go.mod` |
| `templ` CLI | `templ generate` | Version in `Makefile`, run via `go run github.com/a-h/templ/cmd/templ@<version>` |
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

No existing flag, metric name or label changes. No new Prometheus labels; the device state and events are exposed via the admin API, not `/metrics`, to keep cardinality bounded.

All of this is additive, so under the pre-1.0 rules each phase is a patch bump.

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

**Scope.** Section 7.5 in full: validation against the current IP layout, disk and RAM estimate, staged generation, directory swap, rolling restart, rollback, streamed job log. Build screen (10.4).

**Out of scope.** Adding IP aliases or new instances (the UI shows the steps instead).

**Acceptance criteria.**

- A build that fits the current layout completes, the fleet comes back on the new manifest, and the previous build can be rolled back.
- A build that does not fit is refused before anything is written, with the exact manual steps shown.
- A build with insufficient disk is refused before generation starts.
- A failed generator run leaves the live configs untouched.

**Tests.** `jobs` tests with a fake generator binary and fake `systemctl`, estimate tests, swap and rollback tests on a temp directory.

---

### Phase 6 (deferred): SLA-target mode

**Goal.** Let an operator state a target service level (for example "p95 command latency 800 ms, 1% session errors") and have the controller adjust delays, latency profiles and fault rates to hold the fleet at that level.

Not started until phases 1 to 5 are in use and the need is confirmed. Design to be added here before any work begins.

---

## 15. Open questions

1. **SLA-target mode**: worth building? Decide after phase 5.
2. **Logo SVGs**: the logo files in both repos use gradient stops `#7C3411`, `#CC785C`, `#F2C7A8`, while the docs CSS uses the Brand Guidelines 2026 values `#8C4220`, `#CC7A5C`, `#F2C6A2`. The UI uses the guideline values for CSS and the logo files as they are. Should the logo SVGs be updated to match?
3. **Docs claim**: simdocs says the project depends on three external libraries. After phase 3 that is true for `rcfg-sim` only. Reword on the docs site at phase 3.
4. **Milestone**: tracked under rConfig V8 8.4.2 for now; rcfg-sim versions separately (0.0.x).

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
- Run only localised tests for the packages you change, for example
  go test ./internal/fault/... Do not run the full test suite.
- No browser-driven test frameworks (no Playwright, Cypress or Selenium).
- Run gofmt and go vet on the packages you change.
- Update README.md and the [Unreleased] section of CHANGELOG.md for any
  public API change, as section 13 requires.
- If the code and the document disagree, stop and report it rather than
  guessing.

When done, report: files changed, public API added, tests run and their
results, and anything in the document that needs updating.
```