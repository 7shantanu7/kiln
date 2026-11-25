# kiln

Build and test infrastructure for C++ monorepos on CMake + Ninja.

kiln adds Bazel-style guarantees to a CMake + Ninja build without touching its
build files: a shared remote cache, a dependency graph you can trust,
reproducible outputs, and test selection. It is not a build system and does
not do remote execution.

| tool | what it does |
|---|---|
| `kiln-cache` | Remote cache server: Bazel Remote Execution API v2 (CAS, ActionCache, Capabilities, ByteStream) over gRPC, with a content-addressed disk store and LRU eviction |
| `kiln-cc` | Compiler launcher that caches object files in any REAPI cache (kiln-cache, bazel-remote, buildbarn, ...) and shares them across checkouts |
| `kiln-dep` | Runs Ninja under a seccomp/ptrace tracer and reports files that build steps read without declaring them |
| `kiln-tia` | Test impact analysis: maps changed files to the tests they can affect, through the Ninja graph and recorded depfiles |
| `kiln-trace` | Critical path from `.ninja_log`, Clang `-ftime-trace` breakdowns, and a CI gate on build-time regressions |
| `kiln-repro` | Builds twice in different directories and time zones, diffs every output, and classifies each root cause of nondeterminism |
| `kiln-gen` | Generates the synthetic monorepo everything above is tested and benchmarked on |

How each one works: [docs/design.md](docs/design.md).

## Quick start

Requires Go 1.27+, CMake 3.20+, Ninja, and GCC or Clang on Linux.

```bash
make build                                   # binaries in ./bin
./bin/kiln-cache -dir /var/cache/kiln &      # gRPC on 127.0.0.1:8980
```

Add kiln to any CMake project from outside:

```bash
cmake -S . -B build -G Ninja \
  -DCMAKE_PROJECT_INCLUDE=/path/to/kiln/cmake/kiln.cmake \
  -DKILN_CACHE_ADDR=127.0.0.1:8980
ninja -C build
```

`kiln.cmake` maps absolute paths with `-ffile-prefix-map` (so objects match
across checkouts, debug builds included) and sets `kiln-cc` as the compiler
launcher. Set `-DKILN_CACHE_READONLY=ON` for builds that should never write to
the cache, such as pull requests from forks.

The other tools work on any configured Ninja build directory:

```bash
kiln-dep   -C build -src . -clean        # trace a full build, report undeclared inputs
kiln-tia   -C build -src . -git-diff origin/main -format regex   # pass to ctest -R
kiln-trace path -C build                 # critical path of the last clean build
kiln-trace snapshot -C build -o now.json && kiln-trace compare base.json now.json
kiln-repro -src . -toolchain-fixes       # two builds, explained diff
```

## Results

Measured with [bench/](bench/) on a synthetic monorepo from `kiln-gen`
([docs/fixture.md](docs/fixture.md)): **500 targets, 4,250 translation units,
147,806 lines of C++**. Machine: 4-core arm64 (Neoverse-N1), GCC 13.3,
CMake 3.28, Ninja 1.11, Release builds at `-j4`, cache on the same machine.
Raw data is in [bench/results/](bench/results/); method and caveats in
[docs/benchmarks.md](docs/benchmarks.md).

### Remote cache (kiln-cache + kiln-cc)

| clean build | wall time | cache |
|---|---|---|
| no cache | 13m 15s (795 s) | |
| empty cache (every compile a miss) | 15m 34s (934 s), **+17%** | 4,250 misses |
| second checkout in another directory, warm cache | **2m 24s (144 s), 5.5× faster** | **4,250 / 4,250 hits** |

All 250 tests pass on the cached build. The cache held 170 MB. The build
uses all 4 cores (parallelism 3.99) and its critical path is 20.7 s, so it is
throughput-bound. A warm build still runs the preprocessor once per file to
compute the cache key, which is what limits the speedup to 5.5×.

### What CI sees: pull request builds against a cache warmed from main

10 seeded pull requests, each editing 1 to 3 files (sources or headers), each
built clean in a fresh checkout in a new directory with the cache read-only
(`bench/pr-clean.sh`):

| | hit rate | clean build |
|---|---|---|
| median | **99.4%** | **129 s** (6.2× faster than no cache) |
| worst (3 widely included headers) | **98.0%** (86 of 4,250 missed) | 140 s |
| best (one `.cpp`) | 99.98% (1 missed) | 124 s |
| all 10 pooled | 99.3% | |

Every miss is a file the change really affects. All tests passed on every build.

### Test impact analysis (kiln-tia)

20 seeded one-file pull requests (half `.cpp`, half header edits) on the warm
checkout:

| median per PR | all tests | kiln-tia selected |
|---|---|---|
| tests run | 250 | 27.5 |
| test time | 4.05 s | **0.49 s** |
| kiln-tia analysis | | 1.60 s |
| incremental build + analysis + tests | 8.73 s | **6.65 s** |

Selections ranged from 2 tests (a header in a leaf library) to 200 (a `.cpp`
in a library almost everything links). The fixture's whole suite takes 4 s,
so the absolute saving is small; it grows with test suite cost. (kiln-tia was
re-timed after a change that batches its Ninja queries: 3.76 s before, with
identical selections; `bench/results/prs-tia-v2.jsonl`.)

### Undeclared dependencies (kiln-dep)

On a 60-library fixture (1,020 translation units, 1,146 build steps):

| | result |
|---|---|
| tracing overhead, clean build at `-j4` | 186 s → 222 s, **+19%** |
| fixture with no injected bugs | 33,421 reads checked, **0 findings** |
| fixture with 8 injected undeclared dependencies | **8 found, 8 matched, 0 false positives** |

### Reproducibility (kiln-repro)

Same 60-library shape, `RelWithDebInfo`, all reproducibility bugs injected:

| run | files compared | differ | root causes | cause kinds |
|---|---|---|---|---|
| plain | 1,147 | 1,147 | 1,020 | all 6: debug-info paths (1,013), `__FILE__` (12), generator order (6), `__DATE__`/`__TIME__` (2), configure timestamp (1), configured build path (1) |
| `-toolchain-fixes` | 1,147 | 79 | 7 | the 2 that need source changes: generator order (6), configured build path (1) |
| source bugs fixed + `-toolchain-fixes` | 1,147 | **0** | 0 | bit-for-bit identical |

The 72 non-root differences with toolchain fixes on are archives and
executables containing those 7 files.

## Testing

`make test` runs the Go unit tests, including the tracer against real
processes. CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) also runs,
on generated fixtures:

- a cache round trip for GCC and Clang × Debug, Release, RelWithDebInfo: the
  second checkout must be 100% cache hits with byte-identical objects and
  passing tests;
- the same round trip against bazel-remote, to check REAPI interoperability;
- `kiln-dep` must find exactly the injected undeclared dependencies, and none
  in a clean fixture;
- `kiln-repro` must report all six injected causes, leave only the two that
  need source changes once toolchain fixes are on, and pass a fixed fixture.

## Limitations

- Linux only. `kiln-dep` supports x86-64 and arm64.
- `kiln-cc` hashes preprocessor output, which costs a preprocessor run per
  compile (see the cold-cache numbers). It does not cache links.
- The cache speaks plaintext gRPC with no authentication; put it behind a
  TLS-terminating proxy for anything beyond a trusted network.
- `kiln-tia` is only as sound as the graph. Run `kiln-dep` in CI so undeclared
  inputs do not hide affected tests.
- Every number above comes from a synthetic fixture
  ([docs/fixture.md](docs/fixture.md)), not a production codebase.

## License

Apache-2.0
