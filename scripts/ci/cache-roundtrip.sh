#!/usr/bin/env bash
# Builds a fixture twice, from two checkouts in different directories, through
# kiln.cmake and a REAPI cache. Fails unless the second build is 100% cache
# hits, all found in direct mode (without running the preprocessor), tests
# pass in both, and every object file matches.
#
#   scripts/ci/cache-roundtrip.sh WORK_DIR [cmake args...]
#
# CACHE_ADDR set: use that cache (e.g. bazel-remote) instead of starting kiln-cache.
set -euo pipefail

WORK=${1:?usage: cache-roundtrip.sh WORK_DIR [cmake args...]}
shift
KILN=$(cd "$(dirname "$0")/../.." && pwd)
export PATH="$KILN/bin:$PATH"
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

kiln-gen -out "$WORK/a/src" -libs 40 -modules 4 -test-reps 50 > /dev/null
mkdir -p "$WORK/bb"
cp -a "$WORK/a/src" "$WORK/bb/src"

if [ -z "${CACHE_ADDR:-}" ]; then
  kiln-cache -dir "$WORK/cache" -listen 127.0.0.1:18980 -metrics "" > "$WORK/kiln-cache.log" 2>&1 &
  CACHE_PID=$!
  trap 'kill $CACHE_PID 2>/dev/null || true' EXIT
  CACHE_ADDR=127.0.0.1:18980
  sleep 1
fi
echo "cache: $CACHE_ADDR"

for side in a bb; do
  cmake -S "$WORK/$side/src" -B "$WORK/$side/build" -G Ninja \
    -DCMAKE_PROJECT_INCLUDE="$KILN/cmake/kiln.cmake" -DKILN_CACHE_ADDR="$CACHE_ADDR" "$@" > /dev/null
  KILN_CC_LOG="$WORK/$side.log" ninja -C "$WORK/$side/build" > /dev/null
  (cd "$WORK/$side/build" && ctest -j "$(nproc)" --output-on-failure > /dev/null)
done

python3 - "$WORK/a.log" "$WORK/bb.log" <<'EOF'
import collections, json, sys
for path, want in zip(sys.argv[1:], ("miss", "hit")):
    c = collections.Counter(json.loads(l)["outcome"] for l in open(path))
    print(f"{path}: {dict(c)}")
    if set(c) != {want}:
        sys.exit(f"expected only '{want}' outcomes in {path}")
# Hits in the second checkout must come from direct mode (no preprocessor run).
modes = collections.Counter(json.loads(l).get("mode") for l in open(sys.argv[2]))
print(f"hit modes: {dict(modes)}")
if set(modes) != {"direct"}:
    sys.exit("expected every hit in the second checkout to be a direct-mode hit")
EOF

diffs=0
while IFS= read -r obj; do
  rel=${obj#"$WORK/a/build/"}
  cmp -s "$obj" "$WORK/bb/build/$rel" || { echo "differs: $rel"; diffs=$((diffs + 1)); }
done < <(find "$WORK/a/build" -name '*.o')
[ "$diffs" -eq 0 ] || { echo "$diffs object files differ between checkouts"; exit 1; }
echo "ok: second checkout was 100% cache hits with identical objects"
