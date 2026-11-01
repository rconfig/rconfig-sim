#!/usr/bin/env bash
# adopt-data.sh — one-off move of a pre-UI install to the data/ build layout
# the web UI's Build screen needs (docs/UI-ARCHITECTURE.md 7.5):
#
#   <data>/current -> builds/<timestamp>
#   <data>/builds/<timestamp>/{configs/,manifest.csv,build.json}
#
# It moves the existing configs/ and manifest.csv into the first build,
# rewrites the manifest's config_file paths to their new place (the only
# time a manifest is ever rewritten), writes build.json marked adopted,
# points current at the build and sets MANIFEST in every instance env file.
#
# Run as root (make adopt-data) with the instances stopped or about to be
# restarted: running instances keep their open configs until they restart,
# and on restart read <data>/current/manifest.csv.
set -euo pipefail

DATA_DIR=/opt/rcfg-sim/data
MANIFEST=/opt/rcfg-sim/manifest.csv
CONFIGS_DIR=/opt/rcfg-sim/configs
ENV_DIR=/etc/rcfg-sim
GROUP=rcfgsim-ui
DRY_RUN=0

usage() {
  cat <<EOF
Usage: $0 [--data-dir DIR] [--manifest FILE] [--configs-dir DIR] [--env-dir DIR] [--group NAME] [--dry-run]

  --data-dir DIR      the new data directory (default $DATA_DIR)
  --manifest FILE     the current manifest (default $MANIFEST)
  --configs-dir DIR   the current configs directory (default $CONFIGS_DIR)
  --env-dir DIR       instance env files (default $ENV_DIR)
  --group NAME        group that may write the data directory: the UI's (default $GROUP)
  --dry-run           print what would happen, change nothing
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --data-dir) DATA_DIR=$2; shift 2 ;;
    --manifest) MANIFEST=$2; shift 2 ;;
    --configs-dir) CONFIGS_DIR=$2; shift 2 ;;
    --env-dir) ENV_DIR=$2; shift 2 ;;
    --group) GROUP=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

die() { echo "adopt-data: $*" >&2; exit 1; }
run() { if [ "$DRY_RUN" = 1 ]; then echo "would: $*"; else "$@"; fi; }

DATA_DIR=${DATA_DIR%/}
CONFIGS_DIR=${CONFIGS_DIR%/}
NEW_MANIFEST=$DATA_DIR/current/manifest.csv

# --- Checks: refuse before changing anything. ---
[ -e "$DATA_DIR/current" ] || [ -L "$DATA_DIR/current" ] && die "$DATA_DIR/current already exists: this install is already adopted"
[ -f "$MANIFEST" ] || die "no manifest at $MANIFEST"
[ -d "$CONFIGS_DIR" ] || die "no configs directory at $CONFIGS_DIR"
getent group "$GROUP" >/dev/null || die "group $GROUP does not exist (make install creates it)"
shopt -s nullglob
envs=("$ENV_DIR"/*.env)
[ ${#envs[@]} -gt 0 ] || die "no instance env files in $ENV_DIR"
for env in "${envs[@]}"; do
  m=$(sed -n 's/^[[:space:]]*MANIFEST=//p' "$env" | tail -n1 | tr -d '"'"'")
  m=${m:-/opt/rcfg-sim/manifest.csv} # the unit's default
  [ "$m" = "$MANIFEST" ] || die "$env uses MANIFEST=$m, not $MANIFEST; every instance must share the manifest being adopted"
done
# Every config_file must live in the configs directory being moved.
bad=$(awk -F, -v dir="$CONFIGS_DIR/" 'NR > 1 && NF >= 9 && index($9, dir) != 1 { print $9; exit }' "$MANIFEST")
[ -z "$bad" ] || die "manifest row points outside $CONFIGS_DIR: $bad"
rows=$(awk 'NR > 1 && NF > 0' "$MANIFEST" | wc -l | tr -d ' ')
[ "$rows" -gt 0 ] || die "$MANIFEST has no device rows"
same_fs() { [ "$(stat -c %d "$1")" = "$(stat -c %d "$2")" ]; }
mkdir_parent=$(dirname "$DATA_DIR")
[ -d "$mkdir_parent" ] || die "$mkdir_parent does not exist"
same_fs "$CONFIGS_DIR" "$mkdir_parent" || die "$CONFIGS_DIR and $DATA_DIR are on different filesystems; the move must be a rename"

NAME=$(date -u +%Y%m%d-%H%M)
BUILD=$DATA_DIR/builds/$NAME

# --- 1. The data directory and the build directory. ---
run install -d -m 2775 -g "$GROUP" "$DATA_DIR" "$DATA_DIR/builds"
run chgrp "$GROUP" "$DATA_DIR" "$DATA_DIR/builds"
run chmod 2775 "$DATA_DIR" "$DATA_DIR/builds"
run install -d -m 2775 -g "$GROUP" "$BUILD"

# --- 2. Move configs/ and manifest.csv into it. ---
run mv "$CONFIGS_DIR" "$BUILD/configs"

# --- 3. Rewrite the config_file prefix in this one manifest. ---
if [ "$DRY_RUN" = 1 ]; then
  echo "would: rewrite config_file paths $CONFIGS_DIR/ -> $BUILD/configs/ in $MANIFEST, saved as $BUILD/manifest.csv"
else
  awk -F, -v OFS=, -v old="$CONFIGS_DIR/" -v new="$BUILD/configs/" \
    'NR > 1 && NF >= 9 && index($9, old) == 1 { $9 = new substr($9, length(old) + 1) } { print }' \
    "$MANIFEST" > "$BUILD/manifest.csv"
  rm -f "$MANIFEST"
fi

# --- 4. build.json, marked adopted: only the device count is known. ---
if [ "$DRY_RUN" = 1 ]; then
  echo "would: write $BUILD/build.json (adopted, $rows devices)"
else
  cat > "$BUILD/build.json" <<EOF
{
  "name": "$NAME",
  "created": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "adopted": true,
  "count": $rows
}
EOF
fi

# Readable by rcfg-sim; the build and its configs directory writable by the
# UI's group, so the UI can delete the build when retention retires it.
run chgrp -R "$GROUP" "$BUILD"
run chmod 2775 "$BUILD" "$BUILD/configs"
if [ "$DRY_RUN" = 0 ]; then
  find "$BUILD" -type f -exec chmod 0644 {} +
fi

# --- 5. current -> the build, and MANIFEST in every env file. ---
run ln -s "builds/$NAME" "$DATA_DIR/current"
for env in "${envs[@]}"; do
  if [ "$DRY_RUN" = 1 ]; then
    echo "would: set MANIFEST=$NEW_MANIFEST in $env"
  elif grep -q '^[[:space:]]*MANIFEST=' "$env"; then
    sed -i "s|^[[:space:]]*MANIFEST=.*|MANIFEST=$NEW_MANIFEST|" "$env"
  else
    printf 'MANIFEST=%s\n' "$NEW_MANIFEST" >> "$env"
  fi
done

echo "adopt-data: $rows devices adopted as $BUILD"
echo "Next: restart the instances so they read $NEW_MANIFEST (or use Rolling restart on the Fleet screen)."
