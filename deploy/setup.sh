#!/usr/bin/env bash
#
# One-step setup of a simulator host (docs/UI-ARCHITECTURE.md, Phase 6):
# kernel tuning, the rcfgsim and rcfgsim-ui users, make install and
# make install-ui, persistent IP aliases, one env file per IP, a first build
# in the data/ layout, the UI login, and every unit enabled and started.
#
# Run once as root from the source checkout, usually as `sudo make setup`.
# Running it again changes nothing that is already correct. Existing installs
# with env files this script did not write keep using `make adopt-data`.
#
#   IPS=20 BASE_IP=10.50.0.1 DEVICES_PER_IP=2500 PORT_START=10000 deploy/setup.sh
#   deploy/setup.sh --dry-run      # print every step, change nothing
#   deploy/setup.sh --force        # take over env files it did not write
#
# Settings (environment variables):
#   IPS             instances, one per IP alias               (default 20)
#   BASE_IP         first IP; the rest follow on the last octet (default 10.50.0.1)
#   DEVICES_PER_IP  devices (SSH ports) per instance           (default 2500)
#   PORT_START      first SSH port on every IP                 (default 10000)
#   DEVICES         devices in the first build (default IPS x DEVICES_PER_IP;
#                   at least (IPS-1) x DEVICES_PER_IP + 1, so every instance
#                   gets one)
#   DISTRIBUTION    first build's model mix (default sm:100: small configs,
#                   quick to generate; start a testbench for anything else)
#   SEED            first build's seed                         (default 42)
#   INTERFACE       interface for the aliases                  (default lo)
#   METRICS_BIND    address /metrics listens on (default 127.0.0.1; 0.0.0.0
#                   for a remote Prometheus). The admin API stays on its own
#                   loopback port either way.
#   UI_USER         UI login name                              (default admin)
#   UI_PASSWORD     UI password (default: a random one, printed once at the end)
#
# Every directory and every host-changing command can be overridden (the
# Makefile passes its own), so tests run this against a temporary root with
# fakes that only record what they were asked to do: PREFIX, SYSTEMD_DIR,
# SYSCTL_DIR, LIMITS_DIR, ETC_DIR, UI_ETC_DIR, POLKIT_DIR, BIN_DIR, MAKE,
# SYSTEMCTL, GROUPADD, USERADD, CHOWN, GETENT, ID, SYSCTL, IPROUTE.

set -euo pipefail

MARKER="# managed by rcfg-sim setup (deploy/setup.sh); its keys are rewritten on every run"

IPS=${IPS:-20}
BASE_IP=${BASE_IP:-10.50.0.1}
DEVICES_PER_IP=${DEVICES_PER_IP:-2500}
PORT_START=${PORT_START:-10000}
DEVICES=${DEVICES:-}
DISTRIBUTION=${DISTRIBUTION:-sm:100}
SEED=${SEED:-42}
METRICS_BIND=${METRICS_BIND:-127.0.0.1}
INTERFACE=${INTERFACE:-lo}
UI_USER=${UI_USER:-admin}
UI_PASSWORD=${UI_PASSWORD:-}

PREFIX=${PREFIX:-/opt/rcfg-sim}
SYSTEMD_DIR=${SYSTEMD_DIR:-/etc/systemd/system}
SYSCTL_DIR=${SYSCTL_DIR:-/etc/sysctl.d}
LIMITS_DIR=${LIMITS_DIR:-/etc/security/limits.d}
ETC_DIR=${ETC_DIR:-/etc/rcfg-sim}
UI_ETC_DIR=${UI_ETC_DIR:-/etc/rcfg-sim-ui}
POLKIT_DIR=${POLKIT_DIR:-/etc/polkit-1/rules.d}
BIN_DIR=${BIN_DIR:-bin}
UI_GROUP=${UI_GROUP:-rcfgsim-ui}
UI_ADDR=${UI_ADDR:-127.0.0.1:8080}

MAKE=${MAKE:-make}
SYSTEMCTL=${SYSTEMCTL:-systemctl}
GROUPADD=${GROUPADD:-groupadd}
USERADD=${USERADD:-useradd}
CHOWN=${CHOWN:-chown}
GETENT=${GETENT:-getent}
ID=${ID:-id}
SYSCTL=${SYSCTL:-sysctl}
IPROUTE=${IPROUTE:-ip}

DRY_RUN=0
FORCE=0
for arg in "$@"; do
  case $arg in
    --dry-run) DRY_RUN=1 ;;
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,36p' "$0"; exit 0 ;;
    *) echo "setup: unknown argument $arg (try --help)" >&2; exit 2 ;;
  esac
done

die() { echo "setup: $*" >&2; exit 1; }
step() { echo; echo "== $*"; }
# run does a host-changing command, or prints it in a dry run.
run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would run: $*"
  else
    echo "+ $*"
    "$@"
  fi
}

# --- Checks ---------------------------------------------------------------

isnum() { [[ $1 =~ ^[0-9]+$ ]]; }
for v in IPS DEVICES_PER_IP PORT_START SEED; do
  isnum "${!v}" || die "$v must be a whole number (got '${!v}')"
done
[ "$IPS" -ge 1 ] || die "IPS must be at least 1"
[ "$DEVICES_PER_IP" -ge 1 ] || die "DEVICES_PER_IP must be at least 1"
min_devices=$(( (IPS - 1) * DEVICES_PER_IP + 1 ))
max_devices=$(( IPS * DEVICES_PER_IP ))
DEVICES=${DEVICES:-$max_devices}
isnum "$DEVICES" || die "DEVICES must be a whole number (got '$DEVICES')"
if [ "$DEVICES" -lt "$min_devices" ] || [ "$DEVICES" -gt "$max_devices" ]; then
  die "DEVICES must be $min_devices to $max_devices for $IPS instances of $DEVICES_PER_IP (every instance needs at least one device)"
fi
[ $(( PORT_START + DEVICES_PER_IP - 1 )) -le 65535 ] || die "PORT_START + DEVICES_PER_IP exceeds port 65535"
[[ $METRICS_BIND =~ ^[0-9.]+$ ]] || die "METRICS_BIND must be an IPv4 address such as 127.0.0.1 or 0.0.0.0 (got '$METRICS_BIND')"
[[ $UI_USER =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "UI_USER must be 1 to 64 letters, digits, '.', '_' or '-'"

IFS=. read -r o1 o2 o3 o4 <<<"$BASE_IP"
for o in "${o1:-}" "${o2:-}" "${o3:-}" "${o4:-}"; do
  { isnum "$o" && [ "$o" -le 255 ]; } || die "BASE_IP must be an IPv4 address (got '$BASE_IP')"
done
[ $(( o4 + IPS - 1 )) -le 255 ] || die "BASE_IP + IPS crosses the last octet (.255); pick a lower BASE_IP"
ip_n() { echo "$o1.$o2.$o3.$(( o4 + $1 - 1 ))"; }   # the nth IP, from 1

if [ "$DRY_RUN" -eq 0 ] && [ "$($ID -u)" -ne 0 ]; then
  die "must run as root (try: sudo make setup), or pass --dry-run"
fi

# Env files this script did not write mean an existing install.
foreign=()
if [ -d "$ETC_DIR" ]; then
  for f in "$ETC_DIR"/*.env; do
    [ -e "$f" ] || continue
    head -n 1 "$f" | grep -qxF "$MARKER" || foreign+=("$f")
  done
fi
if [ "${#foreign[@]}" -gt 0 ] && [ "$FORCE" -eq 0 ]; then
  die "$ETC_DIR has env files setup did not write (${foreign[*]}). This looks like an existing install: use 'sudo make adopt-data' instead, or pass --force to let setup take them over"
fi

echo "rcfg-sim setup: $IPS instances on $(ip_n 1) to $(ip_n "$IPS") ($INTERFACE), $DEVICES_PER_IP devices each from port $PORT_START"
[ "$DRY_RUN" -eq 1 ] && echo "dry run: nothing will be changed"

MAKEVARS=(PREFIX="$PREFIX" SYSTEMD_DIR="$SYSTEMD_DIR" SYSCTL_DIR="$SYSCTL_DIR" LIMITS_DIR="$LIMITS_DIR"
  ETC_DIR="$ETC_DIR" UI_ETC_DIR="$UI_ETC_DIR" POLKIT_DIR="$POLKIT_DIR" UI_GROUP="$UI_GROUP" BIN_DIR="$BIN_DIR"
  SYSTEMCTL="$SYSTEMCTL" GROUPADD="$GROUPADD" USERADD="$USERADD" CHOWN="$CHOWN" GETENT="$GETENT" ID="$ID")

# --- 1. Users ---------------------------------------------------------------

step "Users"
if $ID -u rcfgsim >/dev/null 2>&1; then
  echo "user rcfgsim exists"
else
  run "$USERADD" -r -s /sbin/nologin -d "$PREFIX" rcfgsim
fi
# rcfgsim-ui and its group come with make install-ui below.

# --- 2. Install -------------------------------------------------------------

step "make install and make install-ui (binaries, units, sysctl and limits drop-ins, polkit rule)"
run "$MAKE" --no-print-directory install install-ui "${MAKEVARS[@]}"

# --- 3. Kernel tuning -------------------------------------------------------

step "Kernel tuning (sysctl and limits)"
run "$SYSCTL" --quiet --system

# --- 4. IP aliases ----------------------------------------------------------

step "IP aliases, persistent across reboots"
aliases_unit="$SYSTEMD_DIR/rcfg-sim-aliases.service"
aliases_args="--interface $INTERFACE --base-ip $BASE_IP --count $IPS"
read -r -d '' aliases_content <<EOF || true
$MARKER
[Unit]
Description=rcfg-sim IP aliases ($IPS on $INTERFACE from $BASE_IP)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
Environment=IPROUTE=$IPROUTE
ExecStart=$PREFIX/deploy/ip-aliases.sh $aliases_args
ExecStop=$PREFIX/deploy/ip-aliases.sh $aliases_args --remove
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

# write_file writes content to path with mode, only when it differs, and
# returns 1 when it was already up to date.
write_file() {
  local path=$1 mode=$2 content=$3
  if [ -f "$path" ] && [ "$(cat "$path")" == "$content" ]; then
    echo "$path is up to date"
    return 1
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would write: $path (mode $mode)"
  else
    echo "+ write $path"
    mkdir -p "$(dirname "$path")" # not install -d: it resets an existing directory's mode
    printf '%s\n' "$content" > "$path.setup-tmp"
    chmod "$mode" "$path.setup-tmp"
    mv "$path.setup-tmp" "$path"
  fi
}

aliases_changed=0
write_file "$aliases_unit" 0644 "$aliases_content" && aliases_changed=1
if [ "$aliases_changed" -eq 1 ]; then
  run "$SYSTEMCTL" daemon-reload
fi

# set_keys prints content with each KEY=value of the remaining arguments
# set: replaced where the key is, appended where it is not.
set_keys() {
  local out=$1 kv k
  shift
  for kv in "$@"; do
    k=${kv%%=*}
    if grep -qxF -- "$kv" <<<"$out"; then
      continue
    elif grep -q "^$k=" <<<"$out"; then
      out=$(awk -v k="$k" -v kv="$kv" 'index($0, k"=") == 1 { print kv; next } { print }' <<<"$out")
    else
      out="$out
$kv"
    fi
  done
  printf '%s' "$out"
}

# --- 5. Env files -----------------------------------------------------------

step "One env file per instance in $ETC_DIR"
manifest="$PREFIX/data/current/manifest.csv"
declare -A env_changed=()
for ((n = 1; n <= IPS; n++)); do
  ip=$(ip_n "$n")
  f="$ETC_DIR/$ip.env"
  want=(
    "LISTEN_IP=$ip"
    "PORT_START=$PORT_START"
    "PORT_COUNT=$DEVICES_PER_IP"
    "MANIFEST=$manifest"
    "HOST_KEY=$PREFIX/host-keys/$ip.key"
    "METRICS_ADDR=$METRICS_BIND:$(( 9100 + n ))"
    "ADMIN_ADDR=127.0.0.1:$(( 19100 + n ))"
    "ALLOW_EMPTY=true"
  )
  if [ -f "$f" ]; then
    # Set setup's keys and keep everything else (the runtime settings the
    # UI wrote). A taken-over file (--force) gets the marker.
    next=$(cat "$f")
    head -n 1 "$f" | grep -qxF "$MARKER" || next="$MARKER
$next"
  else
    next="$MARKER
# Instance $ip. Setup owns the keys it wrote; the web UI adds and changes the
# runtime settings. See $PREFIX/deploy/systemd/rcfg-sim-instance.env.sample."
  fi
  if write_file "$f" 0660 "$(set_keys "$next" "${want[@]}")"; then
    env_changed[$ip]=1
    run "$CHOWN" "root:$UI_GROUP" "$f"
  fi
done

# --- 6. First build -----------------------------------------------------------

step "First build in $PREFIX/data"
data="$PREFIX/data"
build_ok=0
build_new=0
if [ -f "$data/current/build.json" ]; then
  b=$(cat "$data/current/build.json")
  if grep -q "\"ip_base\": \"$BASE_IP\"" <<<"$b" && grep -q "\"ip_count\": $IPS," <<<"$b" &&
     grep -q "\"port_start\": $PORT_START," <<<"$b" && grep -q "\"devices_per_ip\": $DEVICES_PER_IP" <<<"$b"; then
    build_ok=1
    echo "the live build ($(readlink "$data/current")) already fits this layout"
  else
    echo "the live build does not fit this layout (IPs or ports changed): making a new one"
  fi
fi
if [ "$build_ok" -eq 0 ]; then
  name=$(date -u +%Y%m%d-%H%M)
  i=2
  while [ -e "$data/builds/$name" ]; do name="$(date -u +%Y%m%d-%H%M)-$i"; i=$((i + 1)); done
  build="$data/builds/$name"
  run install -d -m 2775 "$data" "$data/builds" "$build"
  run "$PREFIX/bin/rcfg-sim-gen" --count "$DEVICES" --seed "$SEED" --distribution "$DISTRIBUTION" \
    --ip-base "$BASE_IP" --ip-count "$IPS" --port-start "$PORT_START" --devices-per-ip "$DEVICES_PER_IP" \
    --output-dir "$build/configs" --manifest "$build/manifest.csv"
  # The same settings_hash the UI computes, so a testbench with these
  # settings reuses this build instead of generating.
  if [ "$DRY_RUN" -eq 1 ]; then
    hash="(from $PREFIX/bin/rcfg-sim-ui settings-hash)"
  else
    hash=$("$PREFIX/bin/rcfg-sim-ui" settings-hash --gen-bin "$PREFIX/bin/rcfg-sim-gen" --count "$DEVICES" --seed "$SEED" --distribution "$DISTRIBUTION" \
      --ip-base "$BASE_IP" --ip-count "$IPS" --port-start "$PORT_START" --devices-per-ip "$DEVICES_PER_IP")
  fi
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  write_file "$build/build.json" 0644 "{
  \"name\": \"$name\",
  \"created\": \"$now\",
  \"count\": $DEVICES,
  \"seed\": $SEED,
  \"distribution\": \"$DISTRIBUTION\",
  \"ip_base\": \"$BASE_IP\",
  \"ip_count\": $IPS,
  \"port_start\": $PORT_START,
  \"devices_per_ip\": $DEVICES_PER_IP,
  \"settings_hash\": \"$hash\",
  \"last_used\": \"$now\"
}" || true
  build_new=1
  run "$CHOWN" -R "root:$UI_GROUP" "$data"
  run chmod 2775 "$data" "$data/builds" "$build" "$build/configs"
  run ln -sfn "builds/$name" "$data/current.setup-tmp"
  run mv -T "$data/current.setup-tmp" "$data/current"
fi

# --- 7. UI login ------------------------------------------------------------

step "UI login in $UI_ETC_DIR/rcfg-sim-ui.env"
ui_env="$UI_ETC_DIR/rcfg-sim-ui.env"
printed_password=""
ui_changed=0
have_hash=0
if [ -f "$ui_env" ] && grep -q '^UI_PASSWORD_HASH=..*' "$ui_env" && grep -qxF "UI_USERNAME=$UI_USER" "$ui_env"; then
  have_hash=1
fi
if [ "$have_hash" -eq 1 ] && [ -z "$UI_PASSWORD" ]; then
  echo "a login for $UI_USER is already set; left as it is (set UI_PASSWORD to change it)"
else
  pw=$UI_PASSWORD
  if [ -z "$pw" ]; then
    # Read a fixed amount first: tr on an endless /dev/urandom would die of
    # SIGPIPE, which pipefail turns into a failure.
    pw=$(head -c 1024 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
    pw=${pw:0:20}
    printed_password=$pw
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "would set UI_USERNAME=$UI_USER and UI_PASSWORD_HASH (bcrypt, from $PREFIX/bin/rcfg-sim-ui hash-password)"
    printed_password=""
  else
    hash=$(printf '%s\n' "$pw" | "$PREFIX/bin/rcfg-sim-ui" hash-password)
    content=$(cat "$ui_env" 2>/dev/null || true)
    write_file "$ui_env" 0640 "$(set_keys "$content" "UI_USERNAME=$UI_USER" "UI_PASSWORD_HASH=$hash")" || true
    run "$CHOWN" "root:$UI_GROUP" "$ui_env"
    ui_changed=1
  fi
fi

# --- 8. Units -----------------------------------------------------------------

step "Enable and start the units"
enable_now() {
  local unit=$1 restart=${2:-0}
  if [ "$($SYSTEMCTL is-enabled "$unit" 2>/dev/null || true)" = enabled ] &&
     [ "$($SYSTEMCTL is-active "$unit" 2>/dev/null || true)" = active ]; then
    if [ "$restart" -eq 1 ]; then
      run "$SYSTEMCTL" restart "$unit"
    else
      echo "$unit is enabled and running"
    fi
  else
    run "$SYSTEMCTL" enable --now "$unit"
  fi
}
enable_now rcfg-sim-aliases.service "$aliases_changed"
for ((n = 1; n <= IPS; n++)); do
  ip=$(ip_n "$n")
  enable_now "rcfg-sim@$ip.service" "$(( build_new || ${env_changed[$ip]:-0} ))"
done
enable_now rcfg-sim-ui.service "$ui_changed"

# --- Done ---------------------------------------------------------------------

echo
if [ "$DRY_RUN" -eq 1 ]; then
  echo "Dry run finished: the lines above starting 'would' are every change setup would make."
  exit 0
fi
echo "Setup finished."
echo "  Web UI: from your workstation run"
echo "    ssh -L 8080:$UI_ADDR <this host>"
echo "  then open http://127.0.0.1:8080/ and log in as $UI_USER."
if [ -n "$printed_password" ]; then
  echo "  Password (shown once, stored only as a hash): $printed_password"
fi
echo "  Devices: $(ip_n 1) to $(ip_n "$IPS"), ports $PORT_START to $(( PORT_START + DEVICES_PER_IP - 1 )); manifest $manifest"
echo "  SELinux and firewalld are left as they are; see README, Full deployment, step 1."
