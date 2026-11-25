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

# A race that always fails the build: a.o includes gen/g.h from a directory
# that does not exist yet (GCC drops it, so it never even probes for the
# header), and a.o is declared first, so -j1 runs it first. kiln-dep must
# rerun the build and report the race from the second pass.
mkdir -p "$WORK/forced"
printf '#include "g.h"\nint a() { return G; }\n' > "$WORK/forced/a.cpp"
printf '#define G 42\n' > "$WORK/forced/g.h.in"
cat > "$WORK/forced/build.ninja" <<'NINJA'
rule cc
  command = c++ -Igen -c $in -o $out -MD -MF $out.d
  deps = gcc
  depfile = $out.d
rule gen
  command = mkdir -p gen && cp $in $out
build a.o: cc a.cpp
build gen/g.h: gen g.h.in
NINJA
kiln-dep -C "$WORK/forced" -src "$WORK/forced" -j 1 -warn-only -json "$WORK/forced.json" > /dev/null 2>&1

python3 - "$WORK" <<'EOF'
import json, sys
work = sys.argv[1]
forced = json.load(open(f"{work}/forced.json"))["findings"]
want_forced = [{"kind": "undeclared-generated-input", "step": "a.o", "file": "gen/g.h", "producer": "gen/g.h"}]
print(f"forced race: {forced}")
if forced != want_forced:
    sys.exit("forced race: expected exactly one race finding for a.o reading gen/g.h")
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
