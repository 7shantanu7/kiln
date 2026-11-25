#!/usr/bin/env bash
# kiln-dep must find exactly the injected undeclared dependencies, and nothing
# in a fixture without them.
#
#   scripts/ci/dep-gate.sh WORK_DIR
set -euo pipefail

WORK=${1:?usage: dep-gate.sh WORK_DIR}
KILN=$(cd "$(dirname "$0")/../.." && pwd)
export PATH="$KILN/bin:$PATH"
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

for case in buggy clean; do
  inject=$([ "$case" = buggy ] && echo 6 || echo 0)
  kiln-gen -out "$WORK/$case/src" -libs 40 -modules 3 -test-reps 10 -missing-deps "$inject" > /dev/null
  cmake -S "$WORK/$case/src" -B "$WORK/$case/build" -G Ninja -DCMAKE_BUILD_TYPE=Release > /dev/null
  # Injected races may make the build fail; kiln-dep keeps going (-k 0) and
  # still reports them, so ignore the build's own exit status here.
  kiln-dep -C "$WORK/$case/build" -src "$WORK/$case/src" -j 1 -warn-only -json "$WORK/$case.json" 2> /dev/null
done

python3 - "$WORK" <<'EOF'
import json, sys
work = sys.argv[1]
want = json.load(open(f"{work}/buggy/src/kiln-fixture.json"))["injected_missing_deps"]
got = json.load(open(f"{work}/buggy.json"))["findings"]
# Each injection names the consuming library and the producer (library or file).
def matches(inj, f):
    if inj["kind"] == "generated-header-race":
        return f["kind"] == "undeclared-generated-input" and f"/{inj['consumer']}/" in "/" + f["step"] and f"/{inj['producer']}/gen" in "/" + f["file"]
    return f["kind"] == "undeclared-source-input" and f["file"].endswith(inj["producer"])
missed = [i for i in want if not any(matches(i, f) for f in got)]
extra = [f for f in got if not any(matches(i, f) for i in want)]
clean = json.load(open(f"{work}/clean.json"))["findings"]
print(f"injected {len(want)}, found {len(got)}, missed {len(missed)}, false positives {len(extra)}, clean fixture findings {len(clean)}")
if missed or extra or clean:
    sys.exit(json.dumps({"missed": missed, "false_positives": extra, "clean": clean}, indent=2))
EOF
