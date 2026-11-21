package uitest

import (
	"bytes"
	"os"
	"testing"

	"github.com/rcfg-sim/rcfg-sim/internal/configs"
)

// FakeGenScript is a stand-in rcfg-sim-gen: it writes one small config per device
// and a manifest in the generator's format, and records its arguments in
// the file named by $FAKE_GEN_ARGS. `catalogue --json` prints the file
// named by $FAKE_GEN_CATALOGUE. With --counts, device i gets the i-th model
// of the counts in order (no shuffle), and --count defaults to their sum.
// FAKE_GEN_FAIL=1 makes it write some output and then fail; FAKE_GEN_SLEEP
// delays it.
const FakeGenScript = `#!/usr/bin/env bash
set -eu
if [ "${1:-}" = catalogue ]; then cat "$FAKE_GEN_CATALOGUE"; exit 0; fi
echo "$@" > "$FAKE_GEN_ARGS"
count=; counts=
while [ $# -gt 0 ]; do
  case "$1" in
    --count) count=$2 ;; --counts) counts=$2 ;; --output-dir) out=$2 ;; --manifest) man=$2 ;; --ip-base) base=$2 ;;
    --devices-per-ip) dpi=$2 ;; --port-start) ps=$2 ;; --username) u=$2 ;; --password) p=$2 ;;
    --enable-password) ep=$2 ;;
  esac
  shift 2
done
models=()
if [ -n "$counts" ]; then
  IFS=, read -ra pairs <<< "$counts"
  for pr in "${pairs[@]}"; do
    for ((k = 0; k < ${pr##*:}; k++)); do models+=("${pr%%:*}"); done
  done
  count=${count:-${#models[@]}}
fi
[ -n "${FAKE_GEN_SLEEP:-}" ] && sleep "$FAKE_GEN_SLEEP"
mkdir -p "$out"
prefix=${base%.*}; last=${base##*.}
echo "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket" > "$man"
for ((i = 0; i < count; i++)); do
  f="$out/device-$i.cfg"
  model=${models[$i]:-sm}; vendor=Cisco; tmpl=cisco_ios
  case "$model" in
    ciena-*) vendor=Ciena; tmpl=ciena_tl1 ;; infinera-*) vendor=Infinera; tmpl=infinera_tl1 ;; cisco-ons*) tmpl=cisco_ons_tl1 ;;
  esac
  echo "hostname dev-$i" > "$f"
  echo "dev-$i,$prefix.$((last + i / dpi)),$((ps + i % dpi)),$vendor,$tmpl,$u,$p,$ep,$f,$model" >> "$man"
  echo "wrote $f"
  if [ -n "${FAKE_GEN_FAIL:-}" ] && [ "$i" -ge 1 ]; then echo "boom" >&2; exit 3; fi
done
`

// CatalogueJSON is the real generator's catalogue, as rcfg-sim-gen
// catalogue --json prints it.
func CatalogueJSON(t testing.TB) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := configs.WriteCatalogueJSON(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// WriteFakeGen writes FakeGenScript to path and the real catalogue beside
// it, and points the script at both (FAKE_GEN_ARGS is path + ".args").
// It returns the args file.
func WriteFakeGen(t testing.TB, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(FakeGenScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".catalogue.json", CatalogueJSON(t), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_GEN_CATALOGUE", path+".catalogue.json")
	return path + ".args"
}
