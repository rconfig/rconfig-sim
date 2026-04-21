#!/usr/bin/env bash
#
# Batch-apply (or tear down) rcfg-sim IP aliases on a single interface.
# Uses `ip -batch` so 20 aliases go up in one kernel round-trip (~200 ms)
# instead of 20 × fork-exec of `ip addr add`.
#
# Idempotent: existing addresses are skipped on add, missing addresses are
# skipped on remove. Safe to re-run.
#
#   $0 --interface ens192 --base-ip 10.50.0.1 --count 20
#   $0 --interface ens192 --base-ip 10.50.0.1 --count 20 --remove
#   $0 --interface ens192 --base-ip 10.50.0.1 --count 20 --dry-run

set -euo pipefail

usage() {
    cat <<EOF >&2
Usage: $0 --interface IFACE --base-ip IP --count N [--remove] [--dry-run]

Required:
  --interface IFACE    interface to attach aliases to (e.g. ens192)
  --base-ip IP         first IP in range (e.g. 10.50.0.1)
  --count N            number of aliases to create/remove

Optional:
  --remove             tear aliases down instead of adding
  --dry-run            print the batch file, don't apply (no root needed)

Aliases are added as /32 host routes. The range walks the last octet;
cross-octet ranges (e.g. .250 .. .260) are rejected with an explicit
error.

Must be run as root unless --dry-run is passed.
EOF
    exit 1
}

INTERFACE=""
BASE_IP=""
COUNT=0
REMOVE=0
DRYRUN=0

while [[ $# -gt 0 ]]; do
    case $1 in
        --interface) INTERFACE=${2:-}; shift 2 ;;
        --base-ip)   BASE_IP=${2:-};   shift 2 ;;
        --count)     COUNT=${2:-0};    shift 2 ;;
        --remove)    REMOVE=1;         shift ;;
        --dry-run)   DRYRUN=1;         shift ;;
        -h|--help)   usage ;;
        *) echo "unknown flag: $1" >&2; usage ;;
    esac
done

if [[ -z "$INTERFACE" || -z "$BASE_IP" || "$COUNT" -le 0 ]]; then
    usage
fi

if [[ $DRYRUN -eq 0 && $EUID -ne 0 ]]; then
    echo "error: must run as root (use sudo); or pass --dry-run to preview" >&2
    exit 2
fi

if ! ip link show "$INTERFACE" >/dev/null 2>&1; then
    echo "error: interface '$INTERFACE' not found" >&2
    exit 3
fi

# Parse the IPv4 dotted-quad. Shell arithmetic keeps things simple and
# avoids pulling in python/python3 just for IP math.
IFS='.' read -r a b c d <<< "$BASE_IP"
if [[ -z "${a:-}" || -z "${b:-}" || -z "${c:-}" || -z "${d:-}" ]]; then
    echo "error: --base-ip must be IPv4 dotted-quad (got: '$BASE_IP')" >&2
    exit 4
fi
for part in "$a" "$b" "$c" "$d"; do
    if ! [[ "$part" =~ ^[0-9]+$ ]] || [[ $part -lt 0 || $part -gt 255 ]]; then
        echo "error: --base-ip octet out of range (got: '$part')" >&2
        exit 4
    fi
done

# Existing addresses on the interface — used for idempotency.
existing=$(ip -o addr show dev "$INTERFACE" | awk '{print $4}' | cut -d/ -f1 | sort -u)

batchfile=$(mktemp)
trap 'rm -f "$batchfile"' EXIT

added=0
skipped=0

for ((i=0; i<COUNT; i++)); do
    new_d=$((d + i))
    if (( new_d > 255 )); then
        echo "error: range $BASE_IP + $COUNT exceeds .255 on last octet; split into multiple invocations" >&2
        exit 5
    fi
    ip_addr="$a.$b.$c.$new_d"

    if [[ $REMOVE -eq 1 ]]; then
        if echo "$existing" | grep -qxF "$ip_addr"; then
            echo "addr del $ip_addr/32 dev $INTERFACE" >> "$batchfile"
            added=$((added + 1))
        else
            skipped=$((skipped + 1))
        fi
    else
        if echo "$existing" | grep -qxF "$ip_addr"; then
            skipped=$((skipped + 1))
        else
            echo "addr add $ip_addr/32 dev $INTERFACE" >> "$batchfile"
            added=$((added + 1))
        fi
    fi
done

verb="add"
already="present"
if [[ $REMOVE -eq 1 ]]; then
    verb="remove"
    already="absent"
fi

echo "plan: $verb $added aliases on $INTERFACE ($skipped already $already)"

if [[ $DRYRUN -eq 1 ]]; then
    echo "--- batch file (would pass to: ip -batch -) ---"
    if [[ -s "$batchfile" ]]; then
        cat "$batchfile"
    else
        echo "(empty — nothing to do)"
    fi
    exit 0
fi

if [[ ! -s "$batchfile" ]]; then
    echo "nothing to do"
    exit 0
fi

ip -batch "$batchfile"
echo "done"
