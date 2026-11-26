#!/usr/bin/env bash
# The build-time regression gate, tested both ways on a Clang -ftime-trace
# build:
#   1. a second clean build of the same tree must pass `kiln-trace compare`
#      (run-to-run noise must not trip the gate);
#   2. adding <regex> and a regex instantiation to the most-included header,
#      a common real-world compile-time regression, must fail it;
#   3. `kiln-trace headers` on the regressed build must name <regex> and its
#      template instantiations as the cost.
#
#   scripts/ci/time-gate.sh WORK_DIR
set -euo pipefail

WORK=${1:?usage: time-gate.sh WORK_DIR}
KILN=$(cd "$(dirname "$0")/../.." && pwd)
export PATH="$KILN/bin:$PATH"
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
JOBS=$(nproc)
MAX=${KILN_GATE_MAX_REGRESSION:-15}

kiln-gen -out "$WORK/src" -libs 12 > /dev/null
CXX=${CXX:-clang++} cmake -S "$WORK/src" -B "$WORK/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_CXX_FLAGS=-ftime-trace > /dev/null

clean_build() { # name
  ninja -C "$WORK/build" -t clean > /dev/null
  ninja -C "$WORK/build" -j "$JOBS" > /dev/null
  kiln-trace snapshot -C "$WORK/build" -o "$WORK/$1.json"
}

clean_build base
clean_build rerun
echo "== unchanged tree must pass"
kiln-trace compare -max-regression "$MAX" "$WORK/base.json" "$WORK/rerun.json"

# The header the most translation units include, from the recorded depfiles.
header=$(ninja -C "$WORK/build" -t deps | python3 -c '
import collections, sys
src = sys.argv[1] + "/libs/"
c = collections.Counter(l.strip() for l in sys.stdin if l.startswith("    ") and l.strip().startswith(src) and l.strip().endswith(".h"))
print(c.most_common(1)[0][0])' "$WORK/src")
echo "== injecting a regex into $header"
cat >> "$header" <<'EOF'

#include <regex>
#include <string>
inline bool kiln_slow_name_check(const std::string& s) {
  static const std::regex re("[a-z_]+[0-9]*(::[a-z_]+)*", std::regex::ECMAScript | std::regex::icase);
  return std::regex_match(s, re);
}
EOF
clean_build slow
echo "== regressed tree must fail"
set +e
kiln-trace compare -max-regression "$MAX" "$WORK/base.json" "$WORK/slow.json" | tee "$WORK/compare.txt"
got=${PIPESTATUS[0]}
set -e
[ "$got" -eq 1 ] || { echo "kiln-trace compare exited $got on the regressed build, want 1"; exit 1; }
grep -q '^REGRESSION cpu_ms.compile' "$WORK/compare.txt" || { echo "compile time regression not reported"; exit 1; }

echo "== kiln-trace headers must blame <regex>"
kiln-trace headers -C "$WORK/build" -top 5 | tee "$WORK/headers.txt"
# <regex> is cheap to parse but expensive to instantiate, so it shows up in
# the inclusive header table and at the top of the template table.
awk '/^Headers by total/{f=1; next} /^$/{f=0} f' "$WORK/headers.txt" | grep -q '/regex$' \
  || { echo "<regex> not among the top headers by total parse time"; exit 1; }
awk '/^Template/{f=1; next} /^$/{f=0} f' "$WORK/headers.txt" | head -1 | grep -q 'regex' \
  || { echo "no regex template among the top instantiations"; exit 1; }
echo "ok: the gate passed the unchanged build, failed the regression and named its cause"
