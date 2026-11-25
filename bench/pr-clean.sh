#!/usr/bin/env bash
# Measures the cache hit rate CI would see: clean builds of pull request
# branches against a cache warmed from main.
#
#   bench/pr-clean.sh WORK_DIR [PRS]
#
# Run bench/bench.sh WORK_DIR first: it leaves the main checkout in
# WORK_DIR/b/src and a cache warmed from main in WORK_DIR/cache.
#
# For each of PRS (default 10) seeded pull requests:
#   - copy main into a fresh checkout in a new directory (CI workspaces differ),
#   - edit 1 to 3 files: module sources or headers, each edit a real change,
#   - configure and build clean through kiln.cmake with KILN_CACHE_READONLY=ON,
#     the setting for pull request builds, so no PR warms the cache for the next,
#   - run every test, and record hits, misses and build time.
#
# Results go to WORK_DIR/pr-clean.json.
set -euo pipefail
command -v go > /dev/null || export PATH="/usr/local/go/bin:$PATH"

WORK=${1:?usage: bench/pr-clean.sh WORK_DIR [PRS]}
PRS=${2:-10}
KILN=$(cd "$(dirname "$0")/.." && pwd)
JOBS=$(nproc)
PORT=${KILN_BENCH_PORT:-19980}
WORK=$(cd "$WORK" && pwd)
MAIN="$WORK/b/src"
[ -d "$MAIN/.git" ] && [ -d "$WORK/cache" ] || { echo "run bench/bench.sh $WORK first" >&2; exit 2; }
log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
now() { date +%s.%N; }
since() { python3 -c "import sys; print(round(float(sys.argv[2]) - float(sys.argv[1]), 2))" "$1" "$(now)"; }

(cd "$KILN" && go build -o bin/ ./cmd/...)
export PATH="$KILN/bin:$PATH"
git -C "$MAIN" diff --quiet || { echo "$MAIN has uncommitted changes" >&2; exit 2; }

kiln-cache -dir "$WORK/cache" -listen "127.0.0.1:$PORT" -metrics "" > "$WORK/pr-clean-cache.log" 2>&1 &
CACHE_PID=$!
trap 'kill $CACHE_PID 2>/dev/null || true' EXIT
sleep 1

# One line per PR: space-separated files to edit.
python3 - "$MAIN" "$PRS" > "$WORK/pr-clean-plan.txt" <<'EOF'
import os, random, sys
src, n = sys.argv[1], int(sys.argv[2])
rng = random.Random(11)
libs = sorted(d for d in os.listdir(os.path.join(src, "libs")) if d.startswith("lib_"))
for _ in range(n):
    files = set()
    for _ in range(rng.choice([1, 1, 2, 3])):
        lib = rng.choice(libs)
        mod = rng.choice(sorted(os.listdir(os.path.join(src, "libs", lib, "src"))))[:-4]
        files.add(f"libs/{lib}/src/{mod}.cpp" if rng.random() < 0.5 else f"libs/{lib}/include/{lib}/{mod}.h")
    print(" ".join(sorted(files)))
EOF

: > "$WORK/pr-clean.jsonl"
i=0
while read -r -a files; do
  i=$((i + 1))
  pr="$WORK/pr-$i"
  rm -rf "$pr"
  mkdir -p "$pr"
  cp -a "$MAIN" "$pr/src"
  for f in "${files[@]}"; do
    if [[ $f == *.cpp ]]; then
      printf '\nnamespace { [[maybe_unused]] int kiln_pr_edit_%d = %d; }\n' "$i" "$i" >> "$pr/src/$f"
    else
      printf '\ninline int kiln_pr_edit_%d() { return %d; }\n' "$i" "$i" >> "$pr/src/$f"
    fi
  done
  cmake -S "$pr/src" -B "$pr/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_PROJECT_INCLUDE="$KILN/cmake/kiln.cmake" \
    -DKILN_CACHE_ADDR="127.0.0.1:$PORT" -DKILN_CACHE_READONLY=ON > /dev/null
  t=$(now); KILN_CC_LOG="$pr/kiln-cc.log" ninja -C "$pr/build" -j "$JOBS" > /dev/null; build_s=$(since "$t")
  (cd "$pr/build" && ctest -j "$JOBS" | grep -q "100% tests passed") || { log "tests failed for PR $i"; exit 1; }
  python3 - "$pr/kiln-cc.log" "$build_s" "${files[@]}" >> "$WORK/pr-clean.jsonl" <<'EOF'
import collections, json, sys
log, build_s, files = sys.argv[1], float(sys.argv[2]), sys.argv[3:]
c = collections.Counter(json.loads(l)["outcome"] for l in open(log))
total = c["hit"] + c["miss"]
print(json.dumps({"files": files, "hits": c["hit"], "misses": c["miss"], "errors": c["error"],
                  "hit_rate": round(100 * c["hit"] / total, 2), "build_s": build_s}))
EOF
  log "PR $i (${files[*]}): $(tail -1 "$WORK/pr-clean.jsonl")"
  rm -rf "$pr"
done < "$WORK/pr-clean-plan.txt"

python3 - "$WORK/pr-clean.jsonl" "$WORK/pr-clean.json" <<'EOF'
import json, statistics, sys
rows = [json.loads(l) for l in open(sys.argv[1])]
rates = [r["hit_rate"] for r in rows]
hits, misses = sum(r["hits"] for r in rows), sum(r["misses"] for r in rows)
res = {
    "prs": len(rows),
    "median_hit_rate": statistics.median(rates),
    "worst_hit_rate": min(rates),
    "best_hit_rate": max(rates),
    "pooled_hit_rate": round(100 * hits / (hits + misses), 2),
    "median_build_s": statistics.median(r["build_s"] for r in rows),
    "worst_build_s": max(r["build_s"] for r in rows),
    "per_pr": rows,
}
json.dump(res, open(sys.argv[2], "w"), indent=2)
print(json.dumps({k: v for k, v in res.items() if k != "per_pr"}, indent=2))
EOF
