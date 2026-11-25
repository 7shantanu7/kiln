#!/usr/bin/env bash
# Benchmarks kiln-cache, kiln-cc and kiln-tia on a kiln-gen fixture.
#
#   bench/bench.sh WORK_DIR [PRS]
#
# Measures, on this machine:
#   1. a clean build with no cache
#   2. a clean build through kiln-cc with an empty cache (cold)
#   3. a clean build of a second checkout in another directory (warm cache)
#   4. PRS simulated pull requests (default 20): one-file edits, timing the
#      incremental build plus all tests versus only the tests kiln-tia selects
#
# Results go to WORK_DIR/results.json and are printed as a table. Every number
# comes from this run; nothing is estimated.
set -euo pipefail

WORK=${1:?usage: bench/bench.sh WORK_DIR [PRS]}
PRS=${2:-20}
KILN=$(cd "$(dirname "$0")/.." && pwd)
JOBS=$(nproc)
PORT=${KILN_BENCH_PORT:-19980}
GEN_ARGS=${KILN_BENCH_GEN_ARGS:-}

mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
RESULTS="$WORK/results.json"
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
now() { date +%s.%N; }
since() { python3 -c "import sys; print(round(float(sys.argv[2]) - float(sys.argv[1]), 2))" "$1" "$(now)"; }
record() { python3 - "$RESULTS" "$1" "$2" <<'EOF'
import json, sys, os
path, key, value = sys.argv[1:]
data = json.load(open(path)) if os.path.exists(path) else {}
try:
    value = json.loads(value)
except ValueError:
    pass
data[key] = value
json.dump(data, open(path, "w"), indent=2)
EOF
}
outcomes() { # summarise a kiln-cc JSON-lines log
  python3 - "$1" <<'EOF'
import json, sys, collections
c = collections.Counter(json.loads(l)["outcome"] for l in open(sys.argv[1]))
total = c["hit"] + c["miss"]
print(json.dumps({"hit": c["hit"], "miss": c["miss"], "skip": c["skip"], "error": c["error"],
                  "hit_rate": round(100 * c["hit"] / total, 1) if total else 0}))
EOF
}

rm -f "$RESULTS"
log "building kiln"
(cd "$KILN" && go build -o bin/ ./cmd/...)
export PATH="$KILN/bin:$PATH"

record machine "$(python3 -c "
import json, platform, subprocess, os
cpu = subprocess.run(['sh','-c','lscpu | sed -n \"s/^Model name: *//p\" | head -1'], capture_output=True, text=True).stdout.strip()
cc = subprocess.run(['c++','--version'], capture_output=True, text=True).stdout.splitlines()[0]
print(json.dumps({'arch': platform.machine(), 'cpu': cpu, 'cores': os.cpu_count(), 'compiler': cc,
                  'cmake': subprocess.run(['cmake','--version'], capture_output=True, text=True).stdout.split()[2],
                  'ninja': subprocess.run(['ninja','--version'], capture_output=True, text=True).stdout.strip()}))")"
record jobs "$JOBS"

log "generating fixture"
rm -rf "$WORK/a" "$WORK/b" "$WORK/cache"
# shellcheck disable=SC2086
kiln-gen -out "$WORK/a/src" $GEN_ARGS > "$WORK/gen.txt"
record fixture "$(cat "$WORK/a/src/kiln-fixture.json" | python3 -c 'import json,sys; m=json.load(sys.stdin); print(json.dumps({k: m[k] for k in ("targets","translation_units")}))')"
record fixture_loc "$(find "$WORK/a/src" -name '*.cpp' -o -name '*.h' | xargs cat | wc -l)"
(cd "$WORK/a/src" && git init -q && git add -A && git -c user.name=bench -c user.email=bench@localhost commit -qm fixture)
mkdir -p "$WORK/b" && cp -a "$WORK/a/src" "$WORK/b/src"

configure() { # src build [extra cmake args...]
  local src=$1 build=$2; shift 2
  cmake -S "$src" -B "$build" -G Ninja -DCMAKE_BUILD_TYPE=Release "$@" > /dev/null
}

# 1. No cache.
log "clean build without cache"
configure "$WORK/a/src" "$WORK/a/build-nocache"
t=$(now); ninja -C "$WORK/a/build-nocache" -j "$JOBS" > /dev/null; record clean_build_nocache_s "$(since "$t")"
kiln-trace snapshot -C "$WORK/a/build-nocache" -o "$WORK/trace-nocache.json" > /dev/null
record trace_nocache "$(python3 -c "import json; s=json.load(open('$WORK/trace-nocache.json')); print(json.dumps({k: s[k] for k in ('steps','wall_ms','cpu_ms','critical_path_ms','parallelism')}))")"
t=$(now); (cd "$WORK/a/build-nocache" && ctest -j "$JOBS" > "$WORK/ctest-all.txt"); record test_all_s "$(since "$t")"
grep -q "100% tests passed" "$WORK/ctest-all.txt" || { log "tests failed"; exit 1; }
rm -rf "$WORK/a/build-nocache"

# 2 and 3. Through the cache.
log "starting kiln-cache on port $PORT"
kiln-cache -dir "$WORK/cache" -listen "127.0.0.1:$PORT" -metrics "127.0.0.1:$((PORT + 1))" > "$WORK/cache.log" 2>&1 &
CACHE_PID=$!
trap 'kill $CACHE_PID 2>/dev/null || true' EXIT
sleep 1
export KILN_CACHE_ADDR="127.0.0.1:$PORT"

for side in a b; do
  label=$([ "$side" = a ] && echo cold || echo warm)
  log "clean build, $label cache"
  rm -f "$WORK/kiln-cc-$side.log"
  configure "$WORK/$side/src" "$WORK/$side/build" -DCMAKE_PROJECT_INCLUDE="$KILN/cmake/kiln.cmake"
  t=$(now); KILN_CC_LOG="$WORK/kiln-cc-$side.log" ninja -C "$WORK/$side/build" -j "$JOBS" > /dev/null
  record "clean_build_${label}_cache_s" "$(since "$t")"
  record "cache_${label}" "$(outcomes "$WORK/kiln-cc-$side.log")"
done
record cache_size_mb "$(du -sm "$WORK/cache" | cut -f1)"
(cd "$WORK/b/build" && ctest -j "$JOBS" | grep -q "100% tests passed") || { log "tests failed on cached build"; exit 1; }
rm -rf "$WORK/a/build"

# 4. Simulated pull requests on the warm checkout.
log "simulating $PRS pull requests"
python3 - "$WORK/b/src" "$PRS" > "$WORK/prs.txt" <<'EOF'
import os, random, sys
src, n = sys.argv[1], int(sys.argv[2])
rng = random.Random(7)
libs = sorted(d for d in os.listdir(os.path.join(src, "libs")) if d.startswith("lib_"))
for i in range(n):
    lib = rng.choice(libs)
    mods = sorted(os.listdir(os.path.join(src, "libs", lib, "src")))
    mod = rng.choice(mods)[:-4]
    if rng.random() < 0.5:
        print(f"libs/{lib}/src/{mod}.cpp")
    else:
        print(f"libs/{lib}/include/{lib}/{mod}.h")
EOF
: > "$WORK/prs.jsonl"
i=0
while read -r file; do
  i=$((i + 1))
  # A real code change: add a definition, so the edited file recompiles.
  if [[ $file == *.cpp ]]; then
    printf '\nnamespace { [[maybe_unused]] int kiln_bench_edit_%d = %d; }\n' "$i" "$i" >> "$WORK/b/src/$file"
  else
    printf '\ninline int kiln_bench_edit_%d() { return %d; }\n' "$i" "$i" >> "$WORK/b/src/$file"
  fi
  t=$(now); KILN_CC_LOG="$WORK/kiln-cc-pr.log" ninja -C "$WORK/b/build" -j "$JOBS" > /dev/null; build_s=$(since "$t")
  t=$(now); regex=$(kiln-tia -C "$WORK/b/build" -src "$WORK/b/src" -format regex "$file" 2>/dev/null); tia_s=$(since "$t")
  selected=$(kiln-tia -C "$WORK/b/build" -src "$WORK/b/src" "$file" 2>/dev/null | wc -l)
  t=$(now); (cd "$WORK/b/build" && ctest -j "$JOBS" > "$WORK/ctest-pr.txt"); all_s=$(since "$t")
  grep -q "100% tests passed" "$WORK/ctest-pr.txt" || { log "tests failed after editing $file"; exit 1; }
  t=$(now); (cd "$WORK/b/build" && ctest -j "$JOBS" -R "$regex" > "$WORK/ctest-pr.txt"); sel_s=$(since "$t")
  printf '{"file": "%s", "build_s": %s, "tia_s": %s, "selected": %d, "test_all_s": %s, "test_selected_s": %s}\n' \
    "$file" "$build_s" "$tia_s" "$selected" "$all_s" "$sel_s" >> "$WORK/prs.jsonl"
  log "PR $i: $file: build ${build_s}s, $selected tests selected, all ${all_s}s vs selected ${sel_s}s"
  (cd "$WORK/b/src" && git checkout -q -- "$file")
  KILN_CC_LOG=/dev/null ninja -C "$WORK/b/build" -j "$JOBS" > /dev/null
done < "$WORK/prs.txt"

record prs "$(python3 - "$WORK/prs.jsonl" <<'EOF'
import json, statistics, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
med = lambda k: round(statistics.median(r[k] for r in rows), 2)
print(json.dumps({
    "count": len(rows),
    "median_build_s": med("build_s"),
    "median_tia_s": med("tia_s"),
    "median_selected_tests": statistics.median(r["selected"] for r in rows),
    "median_test_all_s": med("test_all_s"),
    "median_test_selected_s": med("test_selected_s"),
    "median_pr_all_s": round(statistics.median(r["build_s"] + r["test_all_s"] for r in rows), 2),
    "median_pr_tia_s": round(statistics.median(r["build_s"] + r["tia_s"] + r["test_selected_s"] for r in rows), 2),
}))
EOF
)"
kill $CACHE_PID 2>/dev/null || true
log "done: $RESULTS"
cat "$RESULTS"
