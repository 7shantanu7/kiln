# Benchmarks

`bench/bench.sh WORK_DIR [PRS]` (or `make bench`) produces every number in the
README. It runs on one machine, with nothing else running, in this order:

1. **Fixture.** `kiln-gen` with defaults: 500 targets, 4,250 translation units.
2. **No cache.** Configure (Release) and build clean with `ninja -j$(nproc)`.
   `kiln-trace snapshot` records CPU time, wall time and the critical path.
   `ctest -j$(nproc)` runs every test once.
3. **Cold cache.** Start an empty `kiln-cache`, configure checkout A with
   `kiln.cmake`, build clean. Every compile is a miss: this measures kiln-cc's
   overhead (a preprocessor run and an upload per file).
4. **Warm cache.** Configure checkout B, a copy of the sources in a different
   directory, and build clean. The hit rate comes from kiln-cc's per-invocation
   log, not from the server.
5. **Pull requests.** On checkout B, `PRS` (default 20) seeded one-file edits:
   half to a module's `.cpp`, half to a module's header, each adding a real
   definition so the file recompiles. For each:
   - incremental build through the cache;
   - `kiln-tia` on the edited file;
   - all tests (`ctest -j`) versus only the selected ones (`ctest -j -R`);
   - revert and rebuild (not timed).

   Medians are reported. Edits to a header used by many libraries select many
   tests; edits to a leaf `.cpp` select one.

Every run must pass all tests (the script stops otherwise), so a cache that
returned wrong objects would fail the benchmark rather than speed it up.

## Pull request clean builds (`bench/pr-clean.sh`)

The cache hit rate a CI system sees: after `bench.sh`, the cache is warm from
main. For each of 10 seeded pull requests, editing 1 to 3 files (module
sources or headers, each a real change):

- copy main into a fresh checkout in a new directory, as CI workspaces are;
- configure with `kiln.cmake` and `KILN_CACHE_READONLY=ON`, the setting for
  pull request builds, so one PR never warms the cache for the next;
- build clean, run every test, and count hits and misses from kiln-cc's log.

Every miss is a translation unit the edit really changed (or one that
includes a changed header); everything else is served from main's entries.

## kiln-dep and kiln-repro (`bench/analysis.sh`)

On a 60-library fixture (1,020 translation units):

- **Overhead.** The same clean build, plain and under kiln-dep's tracer, on a
  fixture without injected bugs, so both do identical work.
- **Accuracy.** kiln-dep on the same shape with 8 injected undeclared
  dependencies; findings are matched against `kiln-fixture.json`.
- **Reproducibility.** kiln-repro with every bug injected, then with
  `-toolchain-fixes`, then on a fixture whose two source bugs are fixed.

## Raw data

`bench/results/` holds the JSON each script wrote for the numbers in the
README.

## What the numbers do not show

- One fixture, one machine. A real codebase has different header fan-out,
  compile times and test runtimes.
- The cache and the build share the machine over loopback. A remote cache adds
  network latency to every lookup and download.
- Test times depend on the fixture's `-test-reps`; they are long enough to
  matter but are not modelled on any real test suite.
