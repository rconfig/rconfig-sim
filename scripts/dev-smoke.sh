#!/usr/bin/env bash
# Dev smoke test for rcfg-sim and rcfg-sim-ui. No root and no systemd: a
# throwaway fleet of 3 instances on 127.0.0.1-3 (3 devices each, SSH ports
# 23000-23002) under $S, with a stand-in systemctl that starts and stops the
# processes the way the rcfg-sim@.service unit would, and the UI on
# $UI_ADDR. See CONTRIBUTING.md, "Smoke test".
#
#   scripts/dev-smoke.sh start     build the fleet in $S if needed, then start
#                                  the instances and the UI; prints the login
#   scripts/dev-smoke.sh traffic   open SSH sessions to every device in a loop
#                                  (foreground; Ctrl-C, or stop, ends it)
#   scripts/dev-smoke.sh status    what is running
#   scripts/dev-smoke.sh stop      stop everything this script started; keeps $S
#   scripts/dev-smoke.sh clean     stop, then delete $S
#
# Settings: S (default /tmp/rcfg-smoke), UI_ADDR (default 127.0.0.1:18080;
# set a private address such as 10.1.1.2:18080 to reach it from another
# machine). Build the binaries first: make build.
set -euo pipefail

REPO=${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}
S=${S:-/tmp/rcfg-smoke}
UI_ADDR=${UI_ADDR:-127.0.0.1:18080}
IPS=(127.0.0.1 127.0.0.2 127.0.0.3)
PER_IP=3
SSH_PORT=23000

# The processes of this smoke test, and nothing else: matched on $S.
instance_pat() { echo "rcfg-sim --listen-ip=$1 --port-start=[0-9]+ --port-count=[0-9]+ --manifest=$S/"; }
ui_pat() { echo "rcfg-sim-ui --ui-addr=[^ ]+ --env-dir=$S/etc "; }

write_helpers() {
  # run.sh: start one instance as rcfg-sim@<IP>.service would (unit
  # defaults, then the env file).
  cat > "$S/run.sh" <<EOF
#!/bin/bash
S=$S; IP=\$1
LISTEN_IP=\$IP PORT_START=10000 PORT_COUNT=2500 MANIFEST=/opt/rcfg-sim/manifest.csv HOST_KEY=\$S/keys/\$IP.key
USERNAME=admin PASSWORD=admin ENABLE_PASSWORD=enable123 SSH_AUTH=password METRICS_ADDR=0.0.0.0:9100
RESPONSE_DELAY_MS_MIN=50 RESPONSE_DELAY_MS_MAX=500 MAX_CONCURRENT_SESSIONS=5000 MAX_SESSION=5m ACTIVE_DEVICES=all
COMMANDS_ROOT= FAULT_RATE=0.0 FAULT_TYPES= FAULT_RATES= DEVICE_PROFILES= PROFILE_SEED=42 LOG_LEVEL=info ADMIN_ADDR=
set -a; . "\$S/etc/\$IP.env"; set +a
exec $REPO/bin/rcfg-sim --listen-ip=\$LISTEN_IP --port-start=\$PORT_START --port-count=\$PORT_COUNT \\
  --manifest=\$MANIFEST --host-key=\$HOST_KEY --username=\$USERNAME --password=\$PASSWORD --enable-password=\$ENABLE_PASSWORD \\
  --ssh-auth=\$SSH_AUTH --metrics-addr=\$METRICS_ADDR --response-delay-ms-min=\$RESPONSE_DELAY_MS_MIN \\
  --response-delay-ms-max=\$RESPONSE_DELAY_MS_MAX --max-concurrent-sessions=\$MAX_CONCURRENT_SESSIONS --max-session=\$MAX_SESSION \\
  --active-devices=\$ACTIVE_DEVICES --commands-root=\$COMMANDS_ROOT --fault-rate=\$FAULT_RATE --fault-types=\$FAULT_TYPES \\
  --fault-rates=\$FAULT_RATES --device-profiles=\$DEVICE_PROFILES --profile-seed=\$PROFILE_SEED --admin-addr=\$ADMIN_ADDR --log-level=\$LOG_LEVEL \\
  >>"\$S/logs/\$IP.log" 2>&1
EOF
  # systemctl: stand-in for the UI's --systemctl, for this fleet only.
  cat > "$S/systemctl" <<EOF
#!/bin/bash
S=$S; verb=\$1; unit=\$2; ip=\${unit#rcfg-sim@}; ip=\${ip%.service}
pat="rcfg-sim --listen-ip=\$ip --port-start=[0-9]+ --port-count=[0-9]+ --manifest=\$S/"
case \$verb in
  is-active) if pgrep -f "\$pat" >/dev/null; then echo active; else echo inactive; exit 3; fi ;;
  start) pgrep -f "\$pat" >/dev/null || setsid -f "\$S/run.sh" "\$ip" </dev/null >/dev/null 2>&1 ;;
  stop) pkill -f "\$pat" || true; for i in \$(seq 50); do pgrep -f "\$pat" >/dev/null || break; sleep 0.1; done ;;
  restart) "\$0" stop "\$unit"; "\$0" start "\$unit" ;;
  *) echo "unsupported \$verb" >&2; exit 1 ;;
esac
EOF
  # askpass: lets ssh log in with the simulator's password non-interactively.
  printf '#!/bin/sh\necho admin\n' > "$S/askpass"
  chmod +x "$S/run.sh" "$S/systemctl" "$S/askpass"
}

# setup generates the fleet (data/ layout, adopted), its env files and two
# testbenches that fit it.
setup() {
  mkdir -p "$S"/{etc,opt,keys,logs,ui}
  "$REPO/bin/rcfg-sim-gen" --count $(( ${#IPS[@]} * PER_IP )) --ip-base "${IPS[0]}" --ip-count ${#IPS[@]} \
    --devices-per-ip $PER_IP --port-start $SSH_PORT --seed 1 --distribution sm:70,md:30 \
    --output-dir "$S/opt/configs" --manifest "$S/opt/manifest.csv" >/dev/null
  for ip in "${IPS[@]}"; do
    cat > "$S/etc/$ip.env" <<EOF
# smoke-test instance $ip
LISTEN_IP=$ip
PORT_START=$SSH_PORT
PORT_COUNT=$PER_IP
MANIFEST=$S/opt/manifest.csv
HOST_KEY=$S/keys/$ip.key
METRICS_ADDR=$ip:19200
RESPONSE_DELAY_MS_MIN=0
RESPONSE_DELAY_MS_MAX=50
ADMIN_ADDR=$ip:19300
EOF
  done
  bash "$REPO/deploy/adopt-data.sh" --data-dir "$S/opt/data" --manifest "$S/opt/manifest.csv" \
    --configs-dir "$S/opt/configs" --env-dir "$S/etc" --group "$(id -gn)" >/dev/null
  mkdir -p "$S/ui/testbenches"
  cat > "$S/ui/testbenches/smoke-a.json" <<'TB'
{
  "name": "Smoke A",
  "description": "9 devices, 5% auth failures fleet-wide",
  "version": 1,
  "build": { "devices": 9, "distribution": "sm:70,md:30", "seed": 1 },
  "runtime": {
    "faults": { "auth_fail": 0.05 },
    "profiles": { "seed": 42, "rules": [] },
    "delay_ms": { "min": 0, "max": 50 },
    "max_sessions": 100,
    "active_devices": "all"
  }
}
TB
  cat > "$S/ui/testbenches/smoke-b.json" <<'TB'
{
  "name": "Smoke B",
  "description": "8 devices, 20% disconnects on 127.0.0.2 only, stops after 10 minutes",
  "version": 1,
  "build": { "devices": 8, "distribution": "sm:100", "seed": 2 },
  "runtime": {
    "faults": {},
    "profiles": { "seed": 42, "rules": [] },
    "delay_ms": { "min": 0, "max": 50 },
    "max_sessions": 100,
    "active_devices": "all"
  },
  "overrides": { "127.0.0.2": { "faults": { "disconnect_mid": 0.2 } } },
  "duration": "10m"
}
TB
  echo "Set up the smoke fleet in $S"
}

start() {
  for b in rcfg-sim rcfg-sim-gen rcfg-sim-ui; do
    [ -x "$REPO/bin/$b" ] || { echo "$REPO/bin/$b missing: run make build first"; exit 1; }
  done
  [ -d "$S/etc" ] || setup
  write_helpers # pick up this checkout's binaries
  for ip in "${IPS[@]}"; do "$S/systemctl" start "rcfg-sim@$ip.service"; done
  # The UI login: made once, kept in $S/ui-login (user, password).
  if [ ! -f "$S/ui-login" ]; then
    pw=$(head -c 512 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
    (umask 077; printf 'admin %s\n' "${pw:0:16}" > "$S/ui-login")
  fi
  read -r ui_user ui_pw < "$S/ui-login"
  if ! pgrep -f "$(ui_pat)" >/dev/null; then
    ui_hash=$(printf '%s\n' "$ui_pw" | "$REPO/bin/rcfg-sim-ui" hash-password)
    (cd "$S" && UI_USERNAME=$ui_user UI_PASSWORD_HASH=$ui_hash setsid -f "$REPO/bin/rcfg-sim-ui" \
      --ui-addr="$UI_ADDR" --env-dir="$S/etc" --data-dir="$S/ui" --poll=2s --systemctl="$S/systemctl" \
      --gen-bin="$REPO/bin/rcfg-sim-gen" </dev/null >>"$S/logs/ui.log" 2>&1)
  fi
  sleep 2
  status
  echo "Open http://$UI_ADDR/ and log in as $ui_user / $ui_pw"
}

traffic() {
  echo $$ > "$S/traffic.pid"
  trap 'rm -f "$S/traffic.pid"' EXIT
  echo "SSH sessions to every device, round and round. Ctrl-C (or $0 stop) ends it."
  while true; do
    for ip in "${IPS[@]}"; do
      for ((p = SSH_PORT; p < SSH_PORT + PER_IP; p++)); do
        printf 'terminal length 0\nshow running-config\nexit\n' | SSH_ASKPASS="$S/askpass" SSH_ASKPASS_REQUIRE=force DISPLAY=x \
          timeout 10 ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
          -o PreferredAuthentications=password -o PubkeyAuthentication=no -p "$p" "admin@$ip" >/dev/null 2>&1 || true
      done
    done
  done
}

status() {
  for ip in "${IPS[@]}"; do printf '%-10s %s\n' "$ip" "$("$S/systemctl" is-active "rcfg-sim@$ip.service" || true)"; done
  if pgrep -f "$(ui_pat)" >/dev/null; then echo "ui         running"; else echo "ui         stopped"; fi
  if [ -f "$S/traffic.pid" ] && kill -0 "$(cat "$S/traffic.pid")" 2>/dev/null; then echo "traffic    running"; else echo "traffic    stopped"; fi
}

stop() {
  if [ -f "$S/traffic.pid" ]; then
    # The loop runs in its own process group when started with setsid; take
    # its ssh children too.
    pid=$(cat "$S/traffic.pid")
    kill -- "-$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
    rm -f "$S/traffic.pid"
  fi
  pkill -f "$(ui_pat)" 2>/dev/null || true
  if [ -x "$S/systemctl" ]; then
    for ip in "${IPS[@]}"; do "$S/systemctl" stop "rcfg-sim@$ip.service"; done
  fi
  echo "stopped (files in $S kept)"
}

case "${1:-}" in
  start) start ;; traffic) traffic ;; status) [ -d "$S" ] && status || echo "nothing set up in $S" ;; stop) stop ;;
  clean) stop; rm -rf "$S"; echo "removed $S" ;;
  *) sed -n '2,19p' "$0"; exit 2 ;;
esac
