# Security Policy

## Supported versions

| Version | Supported |
|---|---|
| v1 (current) | ✅ |
| Pre-release / unreleased branches | ❌ |

Fixes land on the `main` branch; there is no long-term-support branch for v1 at this time.

## Reporting a vulnerability

Please **do not** file security issues as public GitHub issues or discussions.

Email **security@rconfig.com** with:

- A clear description of the vulnerability and its impact
- Steps to reproduce (minimal test case preferred)
- Affected version / commit hash
- Your name and any attribution preference

You can expect an acknowledgement within **5 business days**. We will share our assessment of severity, a planned remediation timeline, and any mitigations we recommend in the interim.

## Scope

**In scope:**

- The SSH server (`rcfg-sim`) — authentication flow, session handling, command dispatch, resource exhaustion, response-generation paths
- The metrics endpoint (`/metrics`, `/healthz`) — exposure, cardinality, unbounded label input
- The fault injector — unintended behaviour outside declared fault types, cross-session bleed
- The config generator (`rcfg-sim-gen`) — template-driven output, path traversal, manifest integrity
- The deployment artifacts (systemd unit, `ip-aliases.sh`, sysctl drop-in) — privilege escalation, unsafe defaults, sandbox escapes

**Out of scope:**

- **Intentional fault-injection behaviour.** By design, `--fault-types auth_fail,disconnect_mid,slow_response,malformed` instructs the simulator to misbehave. Reports along the lines of "enabling `auth_fail` causes authentication to fail" will be closed with reference to this policy.
- **Security issues in rConfig itself.** This repo is the load-test simulator, not the product. rConfig security reports belong on the rConfig project's own channels.
- **Test credentials.** The documented `admin` / `admin` / `enable123` defaults are test values intended for lab use. Reports that these are weak will be closed — they are not production credentials.
- **Operator misconfiguration.** Running the simulator on a public network, skipping the sysctl tuning, or disabling the systemd sandbox are operator decisions, not simulator vulnerabilities.

## Disclosure

We prefer **coordinated disclosure**. Our target timeline:

- Acknowledgement within 5 business days of the initial report
- A remediation plan communicated within 30 days
- Public disclosure once a fix is released and deployed, **or 90 days after the initial report, whichever comes first** — whichever path we're on will be agreed with the reporter in writing before any public disclosure happens

Credit for the discovery will be included in the release notes unless the reporter requests otherwise.
