# Contributing to rcfg-sim

Thanks for considering a contribution. `rcfg-sim` is a focused tool — a load-test simulator for [rConfig](https://www.rconfig.com) — so the scope of what makes sense as a contribution is narrower than a general-purpose library. This page covers what fits, how to build, and the PR workflow.

## Scope

**In scope:**

- Fixes to SSH handshake, command dispatch, session lifecycle, or drain behaviour
- Generator improvements: realism of Cisco IOS output, performance, determinism
- Metrics additions that follow the bounded-cardinality rule
- New fault-injection types that exercise a specific rConfig failure mode
- Deployment artifact improvements (systemd, sysctl, IP aliasing)
- Documentation, test coverage, CI

**Out of scope:**

- Full Cisco IOS emulation (config mode, routing protocols, etc.) — see [README § Known limitations](README.md#known-limitations)
- Multi-vendor emulation (JunOS, EOS, NX-OS) — fork the project if you need this
- Features aimed at production use rather than load testing
- Changes that couple `rcfg-sim` to a specific rConfig version

If you're not sure whether a change fits, open an issue before writing code.

## Reporting issues

- **Security vulnerabilities**: do NOT open a public issue. Follow [SECURITY.md](SECURITY.md).
- **Bugs**: open a GitHub issue with reproduction steps, the commit hash, and the relevant log lines from `journalctl -u rcfg-sim@<IP>`.
- **Feature requests**: open an issue describing the rConfig behaviour you want to exercise. Features without a concrete load-test use case are unlikely to land.

## Development setup

```bash
git clone https://github.com/rconfig/rconfig-sim.git
cd rconfig-sim
make build               # → bin/rcfg-sim, bin/rcfg-sim-gen
make test                # unit tests
make integration         # integration suite (spawns real SSH listeners on loopback)
make bench               # hot-path benchmarks
make vet                 # go vet with and without the integration build tag
make fmt                 # gofmt -s -w
```

Go 1.22+ is required; CI matrixes 1.22 / 1.23 / 1.24. See [.github/workflows/ci.yml](.github/workflows/ci.yml) for exactly what CI runs.

## Pull request workflow

1. **Open an issue first** for anything beyond a typo or a one-line bug fix. This lets us agree on direction before you spend time.
2. **Fork and branch**. Branch names like `fix/handshake-deadline` or `feat/new-fault-type` are helpful but not mandatory.
3. **Keep the change focused**. One logical change per PR. Mixed refactor + feature PRs will be asked to split.
4. **Add or update tests**. Unit tests for pure functions, integration tests (`//go:build integration`) for anything that crosses the SSH or metrics HTTP boundary. See [FEATURE-TESTS.md](FEATURE-TESTS.md) for the manual test catalogue.
5. **Run the gates before pushing**:
   ```bash
   make vet && make fmt && make test && make integration
   ```
   CI runs the same commands. Failing CI blocks merge.
6. **Write a clear commit message**. We use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`. Example:
   ```
   fix(dispatch): reject "en" with Ambiguous, not CmdEnable

   "en" is a prefix of both "enable" and "end". Prior logic silently
   picked the first table entry, which violated spec. Fix: require
   exact canonical match for single-token ambiguous prefixes.

   Closes #42.
   ```
7. **Open the PR** with a summary covering what, why, and how it was tested. Link the originating issue.

## Style

- Code: `gofmt -s` clean, `go vet` clean, covered by the CI gate.
- No new third-party dependencies without discussion — the current set (`golang.org/x/crypto`, `golang.org/x/sys`, `prometheus/client_golang`) is intentionally small. See [README § Known limitations](README.md#known-limitations).
- Zero-copy hot path is load-bearing. Config-serving code paths MUST NOT allocate per-session. If a fault handler has to allocate, see the `malformed` fault implementation for the pattern.
- Metric labels MUST come from a closed enum, never raw user input. The integration test (`TestMetrics_Cardinality`) enforces this.
- Graceful shutdown: any new goroutine must be accounted for in `Server.Shutdown` or via the `sessionWG` / `acceptWG`.

## Releasing

Maintainer-only. Rough process:

1. Update `CHANGELOG.md` under a new `## [X.Y.Z] — YYYY-MM-DD` heading.
2. Tag: `git tag -s vX.Y.Z -m "vX.Y.Z"` on `main` after CI is green.
3. Push the tag; GitHub Actions can be extended to draft a release — not currently automated.

## Code of Conduct

Participation in this project is governed by [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

By contributing, you agree your contributions are licensed under the project's [MIT License](LICENSE).
