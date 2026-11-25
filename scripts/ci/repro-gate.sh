#!/usr/bin/env bash
# kiln-repro must (1) report all six injected causes, (2) leave only the two
# that need source changes once toolchain fixes are on, and (3) pass a fixture
# without those two.
#
#   scripts/ci/repro-gate.sh WORK_DIR
set -euo pipefail

WORK=${1:?usage: repro-gate.sh WORK_DIR}
KILN=$(cd "$(dirname "$0")/../.." && pwd)
export PATH="$KILN/bin:$PATH"
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
GEN="-libs 25 -modules 3 -test-reps 10"

# shellcheck disable=SC2086
kiln-gen -out "$WORK/all/src" $GEN -nondet all > /dev/null
# shellcheck disable=SC2086
kiln-gen -out "$WORK/fixed/src" $GEN -nondet date-time,configure-timestamp,file-macro > /dev/null

expect() { # name want-exit json [kiln-repro args...]
  local name=$1 want=$2 json=$3; shift 3
  set +e
  kiln-repro -work "$WORK/$name-work" -json "$json" "$@" > "$WORK/$name.txt"
  local got=$?
  set -e
  [ "$got" -eq "$want" ] || { cat "$WORK/$name.txt"; echo "$name: exit $got, want $want"; exit 1; }
}
expect plain 1 "$WORK/plain.json" -src "$WORK/all/src"
expect toolchain 1 "$WORK/toolchain.json" -src "$WORK/all/src" -toolchain-fixes
expect fixed 0 "$WORK/fixed.json" -src "$WORK/fixed/src" -toolchain-fixes

python3 - "$WORK" <<'EOF'
import json, sys
work = sys.argv[1]
def kinds(name):
    rep = json.load(open(f"{work}/{name}.json"))
    return sorted({c["kind"] for d in rep["diffs"] if d["root"] for c in d.get("causes") or []})
plain, toolchain = kinds("plain"), kinds("toolchain")
want_plain = sorted(["absolute-path-in-debug-info", "build-date-time", "build-path-in-program-data",
                     "embedded-timestamp", "source-path-in-program-data", "unstable-generator-order"])
want_toolchain = ["build-path-in-program-data", "unstable-generator-order"]
print("plain:", plain)
print("toolchain fixes:", toolchain)
if plain != want_plain or toolchain != want_toolchain:
    sys.exit("unexpected causes")
print("fixed fixture: bit-for-bit identical")
EOF
