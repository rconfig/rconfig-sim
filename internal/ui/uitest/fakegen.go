package uitest

// FakeGenScript is a stand-in rcfg-sim-gen: it writes one small config per device
// and a manifest in the generator's format, and records its arguments in
// the file named by $FAKE_GEN_ARGS.
// FAKE_GEN_FAIL=1 makes it write some output and then fail; FAKE_GEN_SLEEP
// delays it.
const FakeGenScript = `#!/usr/bin/env bash
set -eu
echo "$@" > "$FAKE_GEN_ARGS"
while [ $# -gt 0 ]; do
  case "$1" in
    --count) count=$2 ;; --output-dir) out=$2 ;; --manifest) man=$2 ;; --ip-base) base=$2 ;;
    --devices-per-ip) dpi=$2 ;; --port-start) ps=$2 ;; --username) u=$2 ;; --password) p=$2 ;;
    --enable-password) ep=$2 ;;
  esac
  shift 2
done
[ -n "${FAKE_GEN_SLEEP:-}" ] && sleep "$FAKE_GEN_SLEEP"
mkdir -p "$out"
prefix=${base%.*}; last=${base##*.}
echo "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket" > "$man"
for ((i = 0; i < count; i++)); do
  f="$out/device-$i.cfg"
  echo "hostname dev-$i" > "$f"
  echo "dev-$i,$prefix.$((last + i / dpi)),$((ps + i % dpi)),Cisco,cisco_ios,$u,$p,$ep,$f,sm" >> "$man"
  echo "wrote $f"
  if [ -n "${FAKE_GEN_FAIL:-}" ] && [ "$i" -ge 1 ]; then echo "boom" >&2; exit 3; fi
done
`
