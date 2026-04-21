# TEST-SCENARIOS.md

Progressive load testing scenarios for rconfig-sim. Each scenario is self-contained: setup, run, verify, teardown. Work through them in order — each builds on skills from the previous one.

> ## ⚠️ Resource requirements scale aggressively
>
> These scenarios are **intentionally progressive**. Scenario 1 runs on a laptop in under a minute; Scenario 9 needs a dedicated host and can saturate a 1 Gbps link. **Do not skip ahead** — the early scenarios teach the operational patterns you'll need to recover cleanly when a later scenario exposes a bottleneck.
>
> | Scenario | Min RAM | Min vCPU | Disk | Network | FD limit | Notes |
> |---|---|---|---|---|---|---|
> | 1 — Loopback smoke | 1 GB free | 1 | <50 MB | Loopback only | default | Any dev laptop |
> | 2 — Single-IP small | 2 GB free | 2 | ~100 MB | Loopback only | default | Any dev laptop |
> | 3 — Small systemd | 4 GB free | 2 | ~200 MB | Any | 200k | Dedicated VM preferred |
> | 4 — Medium fleet | 8 GB free | 4 | ~2 GB | Any | 200k | Dedicated VM required |
> | 5 — Full 50k steady | **48 GB** | **12** | **~19 GB** | 1 Gbps+ | **200k** | Reference spec |
> | 6 — Burst 50k | **48 GB** | **12** | ~19 GB | **1 Gbps+ sustained** | 200k | Same host as #5, transient 1–2 Gbps bursts |
> | 7 — Fault injection, fleet-wide | 48 GB | 12 | ~19 GB | 1 Gbps+ | 200k | Same host as #5 |
> | 8 — Localised fault | 48 GB | 12 | ~19 GB | 1 Gbps+ | 200k | Same host as #5 |
> | 9 — Heavy-config stress | 48 GB | 12 | **~60–70 GB** | 1 Gbps+ | 200k | **Disk footprint ~3.5× default** |
>
> **Before running Scenarios 5 through 9**, confirm you've completed the [Full deployment](README.md#full-deployment) prerequisites — sysctl tuning, FD limits, and the `rcfgsim` user. Scenarios 1–2 tolerate a default dev environment; Scenarios 3–4 need sysctl + limits applied; Scenarios 5–9 will fail noisily (port bind errors, FD exhaustion, OOM kills) on an untuned host.
>
> **Co-location warning:** if rConfig runs on the same physical host as the simulator, halve the RAM estimates above and add rConfig's own requirements. For meaningful performance numbers in Scenarios 5–9, put rConfig on a separate host — same-host co-location produces noisy measurements where simulator CPU contention looks like rConfig slowness.

| # | Scenario | Devices | Runtime | Concurrency | Mode |
|---|---|---|---|---|---|
| [1](#scenario-1--loopback-smoke-test) | Loopback smoke test | 10 | Foreground | 1 | Dev |
| [2](#scenario-2--small-lab-single-ip) | Small lab, single IP | 500 | Foreground | 5–10 | Dev |
| [3](#scenario-3--small-lab-systemd) | Small lab, systemd-managed | 500 | systemd | 10–20 | Staging |
| [4](#scenario-4--medium-fleet-multi-ip) | Medium fleet, 5 IPs | 5,000 | systemd | 50–100 | Staging |
| [5](#scenario-5--full-fleet-50k-steady-state) | Full fleet, 50k, steady state | 50,000 | systemd | 100–500 | Production |
| [6](#scenario-6--burst-load-all-devices-now) | Burst load, "all devices now" | 50,000 | systemd | 2,000+ | Production |
| [7](#scenario-7--fault-injection-2-fleet-wide) | Fault injection, 2% fleet-wide | 50,000 | systemd | 100–500 | Chaos |
| [8](#scenario-8--localised-fault-injection) | Localised fault on one IP | 50,000 | systemd | 100–500 | Chaos |
| [9](#scenario-9--heavy-config-stress-huge-skew) | Heavy-config stress (huge skew) | 50,000 | systemd | 100–500 | Stress |

**Conventions used throughout:**

- Commands assume you're running from the source tree at `/opt/src/rconfig-sim` for dev scenarios, or with the installed binaries at `/opt/rcfg-sim/bin/` for production scenarios.
- All scenarios use the `rcfgsim` service user where appropriate.
- `${SIM_HOST}` refers to the simulator host's reachable IP (or `127.0.0.1` for local testing).
- rConfig-side configuration is out of scope — these scenarios cover the simulator. Point rConfig at the generated manifest.

**Prerequisites by scenario class:**

- **Dev scenarios (1–2):** just Go 1.22+ and a built binary.
- **Staging scenarios (3–4):** completed [Full deployment](README.md#full-deployment) steps 1–3 (user, build, install, sysctl).
- **Production scenarios (5–9):** completed full deployment including IP aliases and env files.

---

## Scenario 1 — Loopback smoke test

**Goal.** Prove the binary works end-to-end in under a minute. The simplest possible test: 10 fake devices on loopback, one SSH session, scrape the metrics.

**When to use.** First run after building. After pulling a new version. When debugging a build issue. Never as part of actual load testing — this is a sanity check.

**Setup.**

```bash
cd /opt/src/rconfig-sim
make build

mkdir -p /tmp/rcfg-s1/configs
./bin/rcfg-sim-gen \
  --count 10 \
  --output-dir /tmp/rcfg-s1/configs \
  --manifest /tmp/rcfg-s1/manifest.csv \
  --ip-base 127.0.0.1 \
  --ip-count 1 \
  --port-start 12000 \
  --devices-per-ip 10 \
  --seed 1
```

**Run (foreground, one terminal).**

```bash
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 \
  --port-start 12000 \
  --port-count 10 \
  --manifest /tmp/rcfg-s1/manifest.csv \
  --host-key /tmp/rcfg-s1/hostkey \
  --metrics-addr 127.0.0.1:9100
```

**Verify (second terminal).**

```bash
# Health
curl -s http://127.0.0.1:9100/healthz
# → ok

# One SSH round-trip using sshpass (avoids the pty password-prompt hang)
sudo dnf install -y sshpass
sshpass -p x ssh -T \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  admin@127.0.0.1 -p 12000 > /tmp/rcfg-s1/out.txt 2>&1 <<'EOF'
terminal length 0
enable
enable123
show version
show running-config
exit
exit
EOF

# Output should be clean (no staircase), last lines = ! / end / #exit / >
tail -5 /tmp/rcfg-s1/out.txt

# Metrics reflect the session
curl -s http://127.0.0.1:9100/metrics | grep -E 'sessions_total|bytes_sent' | grep -v '^#'
```

**Pass criteria.** `sessions_total{result="ok"}` is 1, `bytes_sent_total` matches the config size, output ends correctly.

**Teardown.**

```bash
# In the first terminal: Ctrl-C
rm -rf /tmp/rcfg-s1
```

---

## Scenario 2 — Small lab, single IP

**Goal.** Exercise a realistic device count (500) with multiple concurrent sessions. Still foreground, still loopback, but now large enough to shake out obvious concurrency issues.

**When to use.** After a code change that touches the SSH path. When you want to see metrics move in real time under a light concurrent load. First rConfig integration test (point rConfig at a 500-device fleet on your dev box).

**Setup.**

```bash
mkdir -p /tmp/rcfg-s2/configs
./bin/rcfg-sim-gen \
  --count 500 \
  --output-dir /tmp/rcfg-s2/configs \
  --manifest /tmp/rcfg-s2/manifest.csv \
  --ip-base 127.0.0.1 \
  --ip-count 1 \
  --port-start 12000 \
  --devices-per-ip 500 \
  --seed 2
```

**Run (foreground).**

```bash
./bin/rcfg-sim \
  --listen-ip 127.0.0.1 \
  --port-start 12000 \
  --port-count 500 \
  --manifest /tmp/rcfg-s2/manifest.csv \
  --host-key /tmp/rcfg-s2/hostkey \
  --metrics-addr 127.0.0.1:9100 \
  --max-concurrent-sessions 50
```

**Verify with concurrent sessions (second terminal).**

```bash
# Fire 10 parallel SSH sessions, each pulling running-config
for i in $(seq 0 9); do
  PORT=$((12000 + i))
  sshpass -p x ssh -T \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 -p $PORT > /tmp/rcfg-s2/out-$i.txt 2>&1 <<'EOF' &
enable
enable123
show running-config
exit
exit
EOF
done
wait

# All 10 outputs end cleanly
for i in $(seq 0 9); do tail -1 /tmp/rcfg-s2/out-$i.txt; done
# → 10 × "rtr-xxx-xxx-XXXX>"

# Metrics
curl -s http://127.0.0.1:9100/metrics | grep -E 'sessions_total|active_sessions|bytes_sent' | grep -v '^#'
```

**Pass criteria.** 10 clean sessions, `sessions_total{result="ok"}` ≥ 10, no error counter increments, active_sessions returned to 0 after the bursts.

**Teardown.**

```bash
# Ctrl-C in the simulator terminal
rm -rf /tmp/rcfg-s2
```

---

## Scenario 3 — Small lab, systemd-managed

**Goal.** First scenario using the production service shape. 500 devices on a dedicated IP alias, managed by systemd, with env file driving configuration. Same scale as Scenario 2 but production-shaped.

**When to use.** Validating your systemd setup. First deployment to a non-dev box. After `make install` to confirm the unit file and env template produce a working service.

**Prerequisites.** Deployment steps 1–3 complete (user, build, install, sysctl).

**Setup — IP alias + configs + env file.**

```bash
# Single alias on loopback
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 1

# Generate 500 configs pinned to 10.50.0.1
sudo -u rcfgsim /opt/rcfg-sim/bin/rcfg-sim-gen \
  --count 500 \
  --output-dir /opt/rcfg-sim/configs \
  --manifest /opt/rcfg-sim/manifest.csv \
  --ip-base 10.50.0.1 \
  --ip-count 1 \
  --port-start 10000 \
  --devices-per-ip 500 \
  --seed 3

# Env file
sudo tee /etc/rcfg-sim/10.50.0.1.env > /dev/null <<'EOF'
LISTEN_IP=10.50.0.1
PORT_START=10000
PORT_COUNT=500
MANIFEST=/opt/rcfg-sim/manifest.csv
HOST_KEY=/opt/rcfg-sim/host-keys/10.50.0.1.key
USERNAME=admin
PASSWORD=
ENABLE_PASSWORD=enable123
METRICS_ADDR=10.50.0.1:9100
RESPONSE_DELAY_MS_MIN=50
RESPONSE_DELAY_MS_MAX=500
FAULT_RATE=0.0
FAULT_TYPES=
MAX_CONCURRENT_SESSIONS=100
LOG_LEVEL=info
EOF
sudo chown root:rcfgsim /etc/rcfg-sim/10.50.0.1.env
sudo chmod 640 /etc/rcfg-sim/10.50.0.1.env
```

**Run.**

```bash
sudo systemctl start rcfg-sim@10.50.0.1
sleep 2
systemctl is-active rcfg-sim@10.50.0.1
# → active

sudo journalctl -u rcfg-sim@10.50.0.1 -n 5 --no-pager
# Look for: "rcfg-sim ready","devices_loaded":500
```

**Verify.**

```bash
# 20 parallel SSH sessions
for i in $(seq 0 19); do
  PORT=$((10000 + i))
  sshpass -p x ssh -T \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@10.50.0.1 -p $PORT > /dev/null 2>&1 <<'EOF' &
enable
enable123
show running-config
exit
exit
EOF
done
wait

# Scrape
curl -s http://10.50.0.1:9100/metrics | grep -E 'sessions_total|bytes_sent' | grep -v '^#'
```

**Pass criteria.** 20 ok sessions, bytes_sent_total ≈ 20 × average config size, service still active, no restart events in journal.

**Teardown.**

```bash
sudo systemctl stop rcfg-sim@10.50.0.1
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 1 --remove
# Leave configs/manifest/env for reuse in next scenarios
```

---

## Scenario 4 — Medium fleet, 5 IPs

**Goal.** Multi-instance deployment at medium scale. 5,000 devices across 5 IPs, 1,000 devices per IP. First real test of fault-isolation boundaries and per-IP metrics distribution.

**When to use.** Before committing to the full 50k deployment. When you want to test rConfig's behaviour against a realistic mid-market customer size. Validates the per-IP env file workflow and multi-instance systemd management.

**Setup.**

```bash
# 5 IP aliases
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 5

# 5,000 configs spread across the 5 IPs
sudo -u rcfgsim /opt/rcfg-sim/bin/rcfg-sim-gen \
  --count 5000 \
  --output-dir /opt/rcfg-sim/configs \
  --manifest /opt/rcfg-sim/manifest.csv \
  --ip-base 10.50.0.1 \
  --ip-count 5 \
  --port-start 10000 \
  --devices-per-ip 1000 \
  --seed 4

# 5 env files
for i in $(seq 1 5); do
  IP="10.50.0.$i"
  sudo tee /etc/rcfg-sim/${IP}.env > /dev/null <<EOF
LISTEN_IP=${IP}
PORT_START=10000
PORT_COUNT=1000
MANIFEST=/opt/rcfg-sim/manifest.csv
HOST_KEY=/opt/rcfg-sim/host-keys/${IP}.key
USERNAME=admin
PASSWORD=
ENABLE_PASSWORD=enable123
METRICS_ADDR=${IP}:9100
RESPONSE_DELAY_MS_MIN=50
RESPONSE_DELAY_MS_MAX=500
FAULT_RATE=0.0
FAULT_TYPES=
MAX_CONCURRENT_SESSIONS=500
LOG_LEVEL=info
EOF
done
sudo chown root:rcfgsim /etc/rcfg-sim/*.env
sudo chmod 640 /etc/rcfg-sim/*.env
```

**Run (staggered start).**

```bash
for i in $(seq 1 5); do
  sudo systemctl start rcfg-sim@10.50.0.$i
  sleep 2
done

systemctl list-units 'rcfg-sim@*' --no-pager --state=running
# → 5 active (running)
```

**Verify.**

```bash
# Each IP responds
for i in $(seq 1 5); do
  PORT=$((10000 + RANDOM % 1000))
  echo -n "10.50.0.$i:$PORT → "
  timeout 5 sshpass -p x ssh -T \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@10.50.0.$i -p $PORT 'show version' 2>/dev/null | grep -m1 uptime
done

# Aggregate healthz
for i in $(seq 1 5); do
  curl -s -o /dev/null -w "%{http_code} 10.50.0.$i\n" http://10.50.0.$i:9100/healthz
done
# → 5 × "200 10.50.0.X"

# Import the manifest into rConfig and kick off a polling cycle
# Then watch aggregate throughput:
for i in $(seq 1 5); do
  curl -s http://10.50.0.$i:9100/metrics | grep '^rcfgsim_active_sessions '
done | awk '{sum += $2} END {print "total active:", sum}'
```

**Pass criteria.** 5 instances healthy, SSH round-trips succeed against all IPs, rConfig successfully polls 5,000 devices with expected throughput (depends on rConfig worker count).

**Teardown.**

```bash
sudo systemctl stop 'rcfg-sim@*'
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 5 --remove
```

---

## Scenario 5 — Full fleet, 50k, steady state

**Goal.** The headline scenario. 50,000 devices across 20 IPs, steady-state polling concurrency of 100–500. This is what rConfig will see in a realistic large-customer deployment.

**When to use.** Primary load test. Scheduler fairness testing. Database and diff engine scaling validation. Anything where you want to answer "how does rConfig behave at 50k?"

**Setup.**

```bash
# 20 IP aliases
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 20

# Full 50k generation (~8 minutes)
sudo -u rcfgsim /opt/rcfg-sim/bin/rcfg-sim-gen \
  --count 50000 \
  --output-dir /opt/rcfg-sim/configs \
  --manifest /opt/rcfg-sim/manifest.csv \
  --ip-base 10.50.0.1 \
  --ip-count 20 \
  --port-start 10000 \
  --devices-per-ip 2500 \
  --seed 5 \
  --distribution "small:40,medium:40,large:15,huge:5"

# 20 env files
for i in $(seq 1 20); do
  IP="10.50.0.$i"
  sudo tee /etc/rcfg-sim/${IP}.env > /dev/null <<EOF
LISTEN_IP=${IP}
PORT_START=10000
PORT_COUNT=2500
MANIFEST=/opt/rcfg-sim/manifest.csv
HOST_KEY=/opt/rcfg-sim/host-keys/${IP}.key
USERNAME=admin
PASSWORD=
ENABLE_PASSWORD=enable123
METRICS_ADDR=${IP}:9100
RESPONSE_DELAY_MS_MIN=50
RESPONSE_DELAY_MS_MAX=500
FAULT_RATE=0.0
FAULT_TYPES=
MAX_CONCURRENT_SESSIONS=5000
LOG_LEVEL=info
EOF
done
sudo chown root:rcfgsim /etc/rcfg-sim/*.env
sudo chmod 640 /etc/rcfg-sim/*.env
```

**Run (staggered start).**

```bash
for i in $(seq 1 20); do
  sudo systemctl enable --now rcfg-sim@10.50.0.$i
  sleep 2
done

# Aggregate listener count
ss -tlnp | awk '/10\.50\.0\./ {print $4}' | wc -l
# → ~50020 (50000 SSH + 20 metrics)
```

**Verify.**

```bash
# All 20 healthy
for i in $(seq 1 20); do
  curl -s -o /dev/null -w "%{http_code} " http://10.50.0.$i:9100/healthz
done
echo
# → 20 × 200

# Import /opt/rcfg-sim/manifest.csv into rConfig.
# Configure rConfig scheduler with target concurrency (100-500).
# Kick off a full fleet poll.

# During the poll, watch aggregate load:
watch -n 5 'for i in $(seq 1 20); do
  curl -s http://10.50.0.$i:9100/metrics | grep "^rcfgsim_active_sessions "
done | awk "{sum += \$2} END {print \"active:\", sum}"'
```

**Pass criteria.** rConfig completes a full fleet poll with expected success rate (>99%). Aggregate active sessions tracks rConfig's worker concurrency. No simulator instance crashes or restarts. `sessions_total{result="error"}` remains near 0.

**What to measure.** Time to complete a full poll cycle. P95/P99 session duration. rConfig queue depth over time. Any worker starvation or retry spikes on rConfig side.

**Teardown.**

```bash
sudo systemctl stop 'rcfg-sim@*'
# Keep aliases and configs if you're moving to scenarios 6-9
```

---

## Scenario 6 — Burst load, "all devices now"

**Goal.** Test rConfig's behaviour when an operator clicks "refresh all" and the scheduler tries to fan out thousands of concurrent sessions at once. Real failure mode: queue starvation, timeouts, connection refused spikes.

**When to use.** Validating burst handling. Before shipping a customer-visible "full sync" feature. When investigating user reports of "rConfig falls over when I hit refresh-all."

**Setup.** Same as Scenario 5. Keep the aliases and env files in place.

**Run.**

```bash
# Ensure all 20 instances running
for i in $(seq 1 20); do sudo systemctl start rcfg-sim@10.50.0.$i; done
sleep 5
```

On the rConfig side: trigger a full-fleet ad-hoc poll (not a scheduled one). Exact mechanism depends on your rConfig version — typically "Poll All" in the UI or a bulk API call.

**Verify — watch the simulator side.**

```bash
# Live aggregate active sessions, updated every 2s
watch -n 2 'for i in $(seq 1 20); do
  curl -s http://10.50.0.$i:9100/metrics 2>/dev/null | grep "^rcfgsim_active_sessions " | awk "{print \$2}"
done | paste -sd+ | bc'

# Separately, watch session outcomes accumulate
for i in $(seq 1 20); do
  curl -s http://10.50.0.$i:9100/metrics | grep 'sessions_total'
done | grep -v '^#' | awk '{print $1, $2}' | \
  awk -F'{' '{key=$2; sub(/}.*/, "", key); sum[key] += $NF} END {for (k in sum) print k, sum[k]}'
```

**Pass criteria.** Aggregate active_sessions spikes to rConfig's scheduler maximum (2000+ common), then drains smoothly. No simulator errors. Final `sessions_total{result="ok"}` ≈ 50,000 × number of polls triggered. If simulator reports `error` or `disconnect` on more than ~1% of sessions, rConfig's client-side timeouts need tuning.

**What to measure on rConfig side.**
- How many workers actually fan out concurrently vs scheduler target?
- Do jobs time out, retry, or silently drop?
- Queue depth peak and time-to-drain
- Database write latency during the burst

**Teardown.**

```bash
sudo systemctl stop 'rcfg-sim@*'
```

---

## Scenario 7 — Fault injection, 2% fleet-wide

**Goal.** 50,000 devices with a 2% fault rate across all fault types, simulating a realistic production environment where devices occasionally fail. Tests rConfig's error handling, retry logic, and alerting thresholds.

**When to use.** Validating rConfig's resilience. Before a release that changes retry behaviour. When customers report "flapping devices cause cascade failures."

**Setup.** Same base as Scenario 5. Modify env files to enable faults.

```bash
# Regenerate env files with fault injection enabled on ALL 20 instances
for i in $(seq 1 20); do
  IP="10.50.0.$i"
  sudo tee /etc/rcfg-sim/${IP}.env > /dev/null <<EOF
LISTEN_IP=${IP}
PORT_START=10000
PORT_COUNT=2500
MANIFEST=/opt/rcfg-sim/manifest.csv
HOST_KEY=/opt/rcfg-sim/host-keys/${IP}.key
USERNAME=admin
PASSWORD=
ENABLE_PASSWORD=enable123
METRICS_ADDR=${IP}:9100
RESPONSE_DELAY_MS_MIN=50
RESPONSE_DELAY_MS_MAX=500
FAULT_RATE=0.02
FAULT_TYPES=auth_fail,disconnect_mid,slow_response,malformed
MAX_CONCURRENT_SESSIONS=5000
LOG_LEVEL=info
EOF
done
sudo chown root:rcfgsim /etc/rcfg-sim/*.env
sudo chmod 640 /etc/rcfg-sim/*.env
```

**Run.**

```bash
for i in $(seq 1 20); do
  sudo systemctl restart rcfg-sim@10.50.0.$i
  sleep 2
done
```

Trigger a full poll on rConfig side.

**Verify — fault counters moving.**

```bash
# Aggregate fault activations by type
for i in $(seq 1 20); do
  curl -s http://10.50.0.$i:9100/metrics | grep '^rcfgsim_faults_injected_total'
done | grep -v '^#' | \
  awk -F'"' '{type=$2; sum[type] += $NF} END {for (t in sum) print t, sum[t]}'

# Session outcome breakdown
for i in $(seq 1 20); do
  curl -s http://10.50.0.$i:9100/metrics | grep '^rcfgsim_sessions_total'
done | grep -v '^#' | \
  awk -F'"' '{result=$2; sum[result] += $NF} END {for (r in sum) print r, sum[r]}'
```

**Pass criteria.** Fault counters show approximately 2% of command events (total commands per poll × 0.02) across each fault type. `sessions_total{result="auth_fail"}` + `{disconnect}` ≈ 2% each of total sessions per poll. rConfig continues polling successfully; failed devices retry per configured policy; no simulator crashes.

**What to watch on rConfig side.**
- Does the retry queue grow unboundedly or bound correctly?
- Are failed devices flagged with appropriate state (not "deleted" or "disabled")?
- Does the scheduler keep polling healthy devices or does it back off the entire fleet?

**Teardown — revert fault config.**

```bash
sudo systemctl stop 'rcfg-sim@*'

# Set fault rate back to 0 in all env files
sudo sed -i 's/^FAULT_RATE=.*/FAULT_RATE=0.0/' /etc/rcfg-sim/*.env
sudo sed -i 's/^FAULT_TYPES=.*/FAULT_TYPES=/' /etc/rcfg-sim/*.env
```

---

## Scenario 8 — Localised fault injection

**Goal.** Only one IP has faults enabled, the other 19 are clean. Simulates a single "bad site" or misconfigured region where devices fail while the rest of the fleet is healthy.

**When to use.** Testing rConfig's ability to isolate failures to a subset of devices. Validating alerting thresholds that should only fire for a localised failure. Checking whether a single site's problems cascade into fleet-wide degradation.

**Setup.** Based on Scenario 5 (all 20 IPs up, no faults). Modify only `10.50.0.3`'s env file.

```bash
# Enable high fault rate on just 10.50.0.3 (aggressive — 10% to make patterns visible)
sudo sed -i 's/^FAULT_RATE=.*/FAULT_RATE=0.10/' /etc/rcfg-sim/10.50.0.3.env
sudo sed -i 's/^FAULT_TYPES=.*/FAULT_TYPES=auth_fail,slow_response/' /etc/rcfg-sim/10.50.0.3.env

sudo systemctl restart rcfg-sim@10.50.0.3
```

**Run.** Trigger a full poll on rConfig.

**Verify — faults only on one IP.**

```bash
# 10.50.0.3 should show fault activity
curl -s http://10.50.0.3:9100/metrics | grep '^rcfgsim_faults_injected_total' | grep -v '^#'

# Other IPs should show zero faults
for i in 1 2 4 5 10 15 20; do
  echo -n "10.50.0.$i faults: "
  curl -s http://10.50.0.$i:9100/metrics | grep 'faults_injected_total' | \
    grep -v '^#' | awk '{sum += $NF} END {print sum+0}'
done

# Per-IP session failure rate
for i in $(seq 1 20); do
  OK=$(curl -s http://10.50.0.$i:9100/metrics | grep 'sessions_total{result="ok"}' | awk '{print $NF}')
  FAIL=$(curl -s http://10.50.0.$i:9100/metrics | grep 'sessions_total{result="auth_fail"}' | awk '{print $NF}')
  echo "10.50.0.$i: ok=$OK auth_fail=$FAIL"
done
```

**Pass criteria.** `10.50.0.3` shows fault activity and ~10% auth failures. Every other IP shows zero faults and zero auth failures. rConfig flags the 2,500 devices on `10.50.0.3` as problematic without affecting the remaining 47,500.

**Teardown.**

```bash
sudo sed -i 's/^FAULT_RATE=.*/FAULT_RATE=0.0/' /etc/rcfg-sim/10.50.0.3.env
sudo sed -i 's/^FAULT_TYPES=.*/FAULT_TYPES=/' /etc/rcfg-sim/10.50.0.3.env
sudo systemctl restart rcfg-sim@10.50.0.3
```

---

## Scenario 9 — Heavy-config stress (huge skew)

**Goal.** 50,000 devices with a skewed distribution favouring huge configs. Stress the rConfig diff engine, snapshot storage, and transfer pipeline with worst-case data volume.

**When to use.** Storage sizing exercises. Diff engine performance tuning. Before deploying to a customer with known giant-configuration devices (large firewalls, DC cores). Validating DB growth projections.

**Setup — regenerate with huge-heavy distribution.**

```bash
# Stop everything first
sudo systemctl stop 'rcfg-sim@*' 2>/dev/null

# Regenerate with 40% huge configs (vs default 5%)
# Warning: this produces ~60-70 GB on disk, not 19 GB
sudo -u rcfgsim /opt/rcfg-sim/bin/rcfg-sim-gen \
  --count 50000 \
  --output-dir /opt/rcfg-sim/configs \
  --manifest /opt/rcfg-sim/manifest.csv \
  --ip-base 10.50.0.1 \
  --ip-count 20 \
  --port-start 10000 \
  --devices-per-ip 2500 \
  --seed 9 \
  --distribution "small:10,medium:30,large:20,huge:40"

# Check disk usage
du -sh /opt/rcfg-sim/configs/
```

Env files from Scenario 5 can be reused as-is (no changes needed).

**Run.**

```bash
for i in $(seq 1 20); do
  sudo systemctl start rcfg-sim@10.50.0.$i
  sleep 2
done
```

Trigger a full rConfig poll.

**Verify.**

```bash
# Bytes-per-second throughput during the poll
for j in 1 2 3; do
  BEFORE=$(for i in $(seq 1 20); do
    curl -s http://10.50.0.$i:9100/metrics | grep '^rcfgsim_bytes_sent_total '
  done | awk '{sum += $2} END {print sum}')
  sleep 10
  AFTER=$(for i in $(seq 1 20); do
    curl -s http://10.50.0.$i:9100/metrics | grep '^rcfgsim_bytes_sent_total '
  done | awk '{sum += $2} END {print sum}')
  echo "Throughput: $(( (AFTER - BEFORE) / 10 / 1024 / 1024 )) MB/sec"
done
```

**Pass criteria.** rConfig completes the full poll (will take significantly longer than Scenario 5). Database growth is within projected envelope. Diff engine handles multi-MB configs without timing out. No simulator resource pressure (huge configs are mmap'd, not loaded per request — simulator overhead should be identical to Scenario 5).

**What to measure on rConfig side.**
- Storage growth after one poll vs projection
- Diff computation time per device, bucketed by config size
- Database write latency — huge configs are the stress case
- Memory usage during diff operations

**Teardown.**

```bash
sudo systemctl stop 'rcfg-sim@*'

# Optionally regenerate with default distribution to reclaim disk
# sudo -u rcfgsim /opt/rcfg-sim/bin/rcfg-sim-gen --count 50000 ... (as Scenario 5)
```

---

## Full teardown (after any scenario)

Remove everything added by the scenarios, keep the installed binaries and system state:

```bash
# Stop all instances
sudo systemctl stop 'rcfg-sim@*'
sudo systemctl disable 'rcfg-sim@*'

# Remove IP aliases
sudo /opt/rcfg-sim/deploy/ip-aliases.sh --interface lo --base-ip 10.50.0.1 --count 20 --remove

# Remove generated configs and manifest (large — reclaims 20+ GB)
sudo rm -rf /opt/rcfg-sim/configs/*
sudo rm -f /opt/rcfg-sim/manifest.csv

# Remove env files
sudo rm -f /etc/rcfg-sim/10.50.0.*.env

# /tmp scratch (dev scenarios)
rm -rf /tmp/rcfg-s*
```

To also remove the binaries, unit file, sysctl, and user: `sudo make uninstall` from the source tree.

---

## Choosing a scenario

**"I just built it, does it work?"** → Scenario 1

**"I want to dev-test against a realistic small fleet."** → Scenario 2

**"I want to validate my systemd deployment."** → Scenario 3

**"I want to test rConfig against a mid-market customer size."** → Scenario 4

**"I want real load testing numbers."** → Scenario 5

**"What happens when someone clicks Refresh All?"** → Scenario 6

**"How does rConfig handle intermittent device failures?"** → Scenario 7

**"Does a single bad site affect fleet-wide polling?"** → Scenario 8

**"Can rConfig handle really big configs?"** → Scenario 9

---

For detailed manual test samples of individual features (command dispatch, metric labels, fault behaviour unit tests), see [FEATURE-TESTS.md](FEATURE-TESTS.md).