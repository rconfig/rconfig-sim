# CLAUDE.md — rcfg-sim project rules

Project-specific guidance for Claude Code (and human contributors). Read this before making changes.

## What this project is

`rcfg-sim` is a high-density Cisco IOS SSH simulator: one host hosts 50k+ concurrent listeners by `mmap`'ing pre-generated config files and streaming them on demand. Two binaries:

- `rcfg-sim-gen` — deterministic config generator, writes `.cfg` files + manifest CSV.
- `rcfg-sim` — the SSH server, one systemd instance per bound IP alias.

Public repo. People copy-paste from the README. **Treat CLI flags, distribution strings, bucket names, and metric labels as part of the public API.**

## Versioning

The project follows [Semantic Versioning 2.0.0](https://semver.org/) and [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), with one wrinkle: **we are currently pre-1.0**.

### While pre-1.0 (0.x.y) — current phase

- Every release is a **PATCH** bump: `0.0.1 → 0.0.2 → 0.0.3 → ...`
- Breaking changes are allowed at any patch bump. There are no stability guarantees yet. Document the break clearly in the CHANGELOG under a `### Breaking` heading and provide a `#### Migration` snippet.
- Bump to `0.1.0` only when the project reaches a "first stable feature set" milestone the maintainer explicitly declares — not automatically.
- Bump to `1.0.0` when the maintainer explicitly declares API stability and is ready to commit to strict SemVer.

### Post-1.0 — once we declare 1.0.0

| Bump | When |
|---|---|
| **MAJOR** | Any change a downstream user has to react to: renamed/removed CLI flags, renamed bucket labels, renamed metric names or labels, renamed env-file variables, renamed systemd unit names, manifest CSV schema changes, removed commands, host-key path defaults, breaking SSH dispatch behaviour. |
| **MINOR** | New buckets, new commands, new fault types, new metrics, new flags with safe defaults, new env vars, additional CSV columns appended at end. |
| **PATCH** | Bug fixes, doc fixes, dependency bumps that don't change behaviour, performance work with identical outputs. |

### Breaking-change surface (assume external users depend on these)

- Model names (the `--distribution` / `size_bucket` keys in the `registry`, [internal/configs/generator.go](internal/configs/generator.go)): the Cisco size labels `sm`, `md`, `lg`, `xl`, `2xl`, `3xl`, `4xl`, `5xl`, `6xl`, plus `ciena-6500-tl1`
- Driver/template ids in the manifest `template` column (`cisco_ios`, `ciena_tl1`) — the runtime resolves the per-device driver from these (see `driverFor`, [internal/sshsrv/driver.go](internal/sshsrv/driver.go))
- `--distribution` string syntax (`model:weight,...`)
- All CLI flag names and defaults on both binaries
- Manifest CSV header order
- Prometheus metric names and label keys (cardinality is asserted by test — don't add new labels casually)
- Systemd unit name `rcfg-sim@<IP>.service` and the env-file variable names it consumes
- Default paths: `/etc/rcfg-sim/`, `/opt/rcfg-sim/`, host-key path
- The set of recognised commands per driver and their abbreviations (Cisco `show ...`; Ciena TL1 `RTRV-*` / `ACT-USER`)

## Commit style — Conventional Commits

Format: `<type>[(scope)][!]: <subject>`

Allowed types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `perf`, `build`, `ci`. Append `!` *and* include a `BREAKING CHANGE:` footer when the change bumps major.

Examples:
```
feat: add fault injection for slow_response variance
fix(sshsrv): close listener on graceful shutdown to release port
feat!: rename size buckets to clothing-size labels

BREAKING CHANGE: small/medium/large/huge renamed to sm/md/lg/xl.
--distribution strings, manifest size_bucket column values, and template
filenames all changed. Users must update --distribution invocations.
```

Subjects: lowercase, imperative mood, no trailing period, < 72 chars.

## Release / tagging workflow

Run from `main`, with a clean tree.

1. **Decide the bump.** Read every change since the last tag and classify it against the table above. If anything is breaking, the bump is MAJOR.
2. **Update [CHANGELOG.md](CHANGELOG.md):**
   - Move items from `[Unreleased]` into a new `## [X.Y.Z] — YYYY-MM-DD` section.
   - Sub-sections in this order: `Breaking`, `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, `Security`.
   - Add a link reference at the bottom: `[X.Y.Z]: https://github.com/rconfig/rconfig-sim/releases/tag/vX.Y.Z`.
   - Leave an empty `[Unreleased]` section above for the next cycle.
3. **Run the gate** — must all pass:
   ```bash
   go fmt ./... && go vet ./... && go test ./...
   go build ./...
   ```
4. **Commit** with a conventional commit message that names the version (e.g. `chore(release): v2.0.0`).
5. **Tag** as an annotated tag with the changelog excerpt as the body:
   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z" -m "$(awk '/^## \['"X.Y.Z"'\]/,/^## \[/' CHANGELOG.md | sed '$d')"
   ```
6. **Push** main and tags together so CI sees them atomically:
   ```bash
   git push origin main
   git push origin vX.Y.Z
   ```
7. **Cut a GitHub release** from the tag (`gh release create vX.Y.Z --notes-from-tag`) so users get release notifications.

Never delete or move a published tag. If a tag is wrong, fix-forward with a new patch tag.

## Pre-merge gate (also CI)

Every PR must pass:

- `go fmt ./...` — no diff after run.
- `go vet ./...` — no warnings.
- `go test ./...` — unit tests including the deterministic-output test (`TestRunDeterministic`).
- `go build ./...` — both binaries compile.

Integration tests live behind build tag `integration` and run separately — they require generating configs and spinning up a real SSH server.

## Don't-commit list

- Compiled binaries (`bin/`, `build/`, `dist/` — already gitignored).
- Generated configs (`configs/device-*.cfg`, `manifest.csv` — gitignored).
- Host keys (`host-keys/` — gitignored).
- `.claude/settings.local.json` — gitignored.
- **Anything with a credential**, including `git remote` URLs that embed PATs. If you find one, redact it and tell the user to rotate.
- Files you only modified to test something. Use `git add <specific files>` rather than `git add -A`.

## Working with the generator

- The generator is driven by a `registry` of `model` entries ([internal/configs/generator.go](internal/configs/generator.go)). Each model carries its manifest `vendor`/`template` strings, the template file to render, and a per-vendor data-builder. The Cisco size buckets are derived mechanically from `profiles` + `templateAliases`; `modelOrder` is the canonical iteration order (Cisco buckets first, unchanged, then non-Cisco models appended).
- Adding a Cisco size bucket: append to `bucketOrder`, add a `profiles` entry, and either create a `templates/<name>.tmpl` or register an alias in `templateAliases`. The registry picks it up automatically.
- Adding a new vendor/model: add one `registry` entry (vendor, driver/template id, template file, builder) and a `templates/<name>.tmpl`; on the runtime side add one `Driver` implementation registered via `init()` in `internal/sshsrv/driver_<vendor>.go`. Model names and driver ids are user-facing — see [README § Configuration templates](README.md#configuration-templates).
- Increasing a profile's counts is non-breaking (file sizes drift); renaming or removing a model is breaking.
- The deterministic test (`TestRunDeterministic`, and `TestCienaDeterministic` for Ciena) hashes outputs across two runs with the same seed. Any change that alters template output for a given seed will fail this test — bump the test fixture only when the diff is intentional.
- Cisco output must stay byte-identical when refactoring shared machinery. The integration characterization tests ([internal/sshsrv/characterization_test.go](internal/sshsrv/characterization_test.go)) pin the greeting, enable-mode flow, and session close — run `go test -tags integration ./...` before and after.

## When in doubt

If the change touches anything in the breaking-change surface list above, stop and confirm the version bump with the maintainer before committing. It is always cheaper to bump a version on purpose than to issue an apology + revert.
