#!/usr/bin/env bash
# Measures kiln-dep and kiln-repro on a mid-size kiln-gen fixture.
#
#   bench/analysis.sh WORK_DIR [LIBS]
#
#   1. kiln-dep overhead: a clean build with plain Ninja versus the same clean
#      build traced by kiln-dep, on a fixture without injected bugs (so both
#      builds succeed and do identical work).
#   2. kiln-dep findings on the same shape with injected undeclared
#      dependencies, compared against the fixture's ground truth.
#   3. kiln-repro on the same shape with all nondeterminism injected: plain,
#      then with -toolchain-fixes, then with the source bugs fixed.
#
# Results go to WORK_DIR/analysis.json.
set -euo pipefail

WORK=${1:?usage: bench/analysis.sh WORK_DIR [LIBS]}
LIBS=${2:-60}
KILN=$(cd "$(dirname "$0")/.." && pwd)
JOBS=$(nproc)
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
OUT="$WORK/analysis.json"
export PATH="$KILN/bin:$PATH"
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
now() { date +%s.%N; }
since() { python3 -c "import sys; print(round(float(sys.argv[2]) - float(sys.argv[1]), 2))" "$1" "$(now)"; }
GEN="-libs $LIBS -modules 16 -test-reps 10"

(cd "$KILN" && go build -o bin/ ./cmd/...)
rm -rf "$WORK/clean" "$WORK/buggy" "$WORK/nondet" "$WORK/fixed"

log "kiln-dep overhead on $LIBS libraries"
# shellcheck disable=SC2086
kiln-gen -out "$WORK/clean/src" $GEN > /dev/null
cmake -S "$WORK/clean/src" -B "$WORK/clean/build" -G Ninja -DCMAKE_BUILD_TYPE=Release > /dev/null
t=$(now); ninja -C "$WORK/clean/build" -j "$JOBS" > /dev/null; plain=$(since "$t")
ninja -C "$WORK/clean/build" -t clean > /dev/null
t=$(now); kiln-dep -C "$WORK/clean/build" -src "$WORK/clean/src" -j "$JOBS" -json "$WORK/dep-clean.json" > "$WORK/dep-clean.txt" 2> /dev/null; traced=$(since "$t")
log "plain ${plain}s, traced ${traced}s"

log "kiln-dep findings with injected bugs"
# shellcheck disable=SC2086
kiln-gen -out "$WORK/buggy/src" $GEN -missing-deps 8 > /dev/null
cmake -S "$WORK/buggy/src" -B "$WORK/buggy/build" -G Ninja -DCMAKE_BUILD_TYPE=Release > /dev/null
kiln-dep -C "$WORK/buggy/build" -src "$WORK/buggy/src" -j "$JOBS" -warn-only -json "$WORK/dep-buggy.json" > "$WORK/dep-buggy.txt" 2> /dev/null

log "kiln-repro"
# shellcheck disable=SC2086
kiln-gen -out "$WORK/nondet/src" $GEN -nondet all > /dev/null
# shellcheck disable=SC2086
kiln-gen -out "$WORK/fixed/src" $GEN -nondet date-time,configure-timestamp,file-macro > /dev/null
set +e
kiln-repro -src "$WORK/nondet/src" -work "$WORK/repro-plain" -j "$JOBS" -json "$WORK/repro-plain.json" > "$WORK/repro-plain.txt"
kiln-repro -src "$WORK/nondet/src" -work "$WORK/repro-toolchain" -j "$JOBS" -toolchain-fixes -json "$WORK/repro-toolchain.json" > "$WORK/repro-toolchain.txt"
kiln-repro -src "$WORK/fixed/src" -work "$WORK/repro-fixed" -j "$JOBS" -toolchain-fixes -json "$WORK/repro-fixed.json" > "$WORK/repro-fixed.txt"
set -e
rm -rf "$WORK/repro-plain" "$WORK/repro-toolchain" "$WORK/repro-fixed"

python3 - "$WORK" "$plain" "$traced" "$OUT" <<'EOF'
import json, sys
work, plain, traced, out = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), sys.argv[4]
fixture = json.load(open(f"{work}/clean/src/kiln-fixture.json"))
clean = json.load(open(f"{work}/dep-clean.json"))
buggy = json.load(open(f"{work}/dep-buggy.json"))
injected = json.load(open(f"{work}/buggy/src/kiln-fixture.json"))["injected_missing_deps"]
def matches(inj, f):
    if inj["kind"] == "generated-header-race":
        return f["kind"] == "undeclared-generated-input" and f"/{inj['consumer']}/" in "/" + f["step"] and f"/{inj['producer']}/gen" in "/" + f["file"]
    return f["kind"] == "undeclared-source-input" and f["file"].endswith(inj["producer"])
def repro(name):
    r = json.load(open(f"{work}/repro-{name}.json"))
    roots = [d for d in r["diffs"] if d["root"]]
    return {"compared": r["compared"], "differ": len(r["diffs"]), "root_causes": len(roots),
            "cause_kinds": sorted({c["kind"] for d in roots for c in d.get("causes") or []})}
res = {
    "fixture": {"targets": fixture["targets"], "translation_units": fixture["translation_units"]},
    "dep_overhead": {"plain_build_s": plain, "traced_build_s": traced,
                     "overhead_pct": round(100 * (traced - plain) / plain, 1),
                     "steps_traced": clean["steps_traced"], "reads_checked": clean["reads_checked"],
                     "findings_on_clean_fixture": len(clean["findings"])},
    "dep_findings": {"injected": len(injected), "found": len(buggy["findings"]),
                     "matched": sum(any(matches(i, f) for f in buggy["findings"]) for i in injected),
                     "false_positives": sum(not any(matches(i, f) for i in injected) for f in buggy["findings"]),
                     "steps_traced": buggy["steps_traced"]},
    "repro": {"plain": repro("plain"), "toolchain_fixes": repro("toolchain"), "source_and_toolchain_fixes": repro("fixed")},
}
json.dump(res, open(out, "w"), indent=2)
print(json.dumps(res, indent=2))
EOF
