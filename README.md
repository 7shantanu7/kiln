# kiln

Build and test infrastructure for C++ monorepos on CMake + Ninja.

kiln adds Bazel-style guarantees to a CMake + Ninja build without touching its
build files: a shared remote cache, a dependency graph you can trust,
reproducible outputs, and test selection. It is not a build system and does
not do remote execution.

| tool | what it does |
|---|---|
| `kiln-cache` | Remote cache server: Bazel Remote Execution API v2 (CAS, ActionCache, Capabilities, ByteStream) over gRPC, with a content-addressed disk store and LRU eviction |
| `kiln-cc` | Compiler launcher that caches object files in any REAPI cache (kiln-cache, bazel-remote, buildbarn) and shares them across checkouts; cache hits skip the preprocessor |
| `kiln-dep` | Runs Ninja under a seccomp/ptrace tracer and reports files that build steps read without declaring them |
| `kiln-tia` | Test impact analysis: maps changed files to the tests they can affect, through the Ninja graph and recorded depfiles |
| `kiln-trace` | Critical path from `.ninja_log`, Clang `-ftime-trace` breakdowns, and a CI gate on build-time regressions |
| `kiln-repro` | Builds twice in different directories and time zones, diffs every output, and classifies each root cause of nondeterminism |
| `kiln-gen` | Generates the synthetic monorepo everything above is tested and benchmarked on |

How each one works: [docs/design.md](docs/design.md).

## Quick start

Requires CMake 3.20+, Ninja, and GCC or Clang on Linux; building from source
also needs Go 1.27+.

```bash
make build                                   # binaries in ./bin
./bin/kiln-cache -dir /var/cache/kiln &      # gRPC on 127.0.0.1:8980
```

Or run the cache server from the container image (amd64 and arm64):

```bash
docker run -d -p 8980:8980 -p 8981:8981 -v kiln-cache:/data ghcr.io/7shantanu7/kiln-cache:latest
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
| no cache | 13m 22s (801.5 s) | |
| empty cache (every compile a miss) | 15m 43s (943.2 s), **+18%** | 4,250 misses |
| second checkout in another directory, warm cache | **38.2 s, 21× faster** | **4,250 / 4,250 hits**, all direct |

All 250 tests pass on the cached build. The cache held 362 MB (objects plus
direct-mode manifests). Without a cache the build uses all 4 cores
(parallelism 3.99) and its critical path is 20.7 s.

Every hit was found in direct mode, without running the preprocessor, at a
median 15 ms per file. Before direct mode the same benchmark ran the
preprocessor for every hit and reached 5.5× (144 s); on the 200-file CI
fixture a hit took a median 119 ms that way and 28 ms in direct mode. The
cold build pays for a preprocessor run and an upload per file.

### What CI sees: pull request builds against a cache warmed from main

10 seeded pull requests, each editing 1 to 3 files (sources or headers), each
built clean in a fresh checkout in a new directory with the cache read-only
(`bench/pr-clean.sh`):

| | hit rate | clean build |
|---|---|---|
| median | **99.4%** | **44.4 s** (18× faster than no cache) |
| worst (3 widely included headers) | **98.0%** (86 of 4,250 missed) | 57.3 s |
| best (one `.cpp`) | 99.98% (1 missed) | 38.9 s |
| all 10 pooled | 99.3% | |

Every miss is a file the change really affects; every hit was direct. All
tests passed on every build.

### Test impact analysis (kiln-tia)

20 seeded one-file pull requests (half `.cpp`, half header edits) on the warm
checkout:

| median per PR | all tests | kiln-tia selected |
|---|---|---|
| tests run | 250 | 27.5 |
| test time | 4.03 s | **0.49 s** |
| kiln-tia analysis | | 1.59 s |
| incremental build + analysis + tests | 8.79 s | **6.54 s** |

Selections ranged from 1 test (a header in a leaf library) to 200 (a `.cpp`
in a library almost everything links). The fixture's whole suite takes 4 s,
so the absolute saving is small; it grows with test suite cost.

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

## Releases

Tagging `v*` runs [.github/workflows/release.yml](.github/workflows/release.yml):

- static binaries for linux/amd64 and linux/arm64, built reproducibly
  (`scripts/release/build.sh`); the workflow rebuilds them from a copy of the
  tree in another directory and fails unless the checksums match;
- the multi-arch `kiln-cache` image on ghcr.io;
- for both: SLSA v1.0 build provenance and an SPDX SBOM as signed GitHub
  attestations, and Sigstore keyless signatures (cosign) on `SHA256SUMS` and
  the image.

Every push and pull request runs the same build, rebuild check, SBOM and image
build without publishing. To verify a release:

```bash
gh attestation verify kiln_v0.1.1_linux_amd64.tar.gz --repo 7shantanu7/kiln
gh attestation verify oci://ghcr.io/7shantanu7/kiln-cache:v0.1.1 --repo 7shantanu7/kiln
cosign verify-blob SHA256SUMS --bundle SHA256SUMS.sigstore.json \
  --certificate-identity-regexp '^https://github.com/7shantanu7/kiln/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Testing

`make test` runs the Go unit tests, including the tracer against real
processes. CI ([.github/workflows/ci.yml](.github/workflows/ci.yml)) also runs,
on generated fixtures:

- a cache round trip for GCC and Clang × Debug, Release, RelWithDebInfo: the
  second checkout must be 100% cache hits, all in direct mode, with
  byte-identical objects and passing tests;
- the same round trip against bazel-remote and buildbarn (bb-storage), to
  check REAPI interoperability;
- the round trip cross-compiled for aarch64 on an x86-64 runner, with the
  tests run under QEMU user-mode emulation
  ([cmake/toolchains/aarch64-linux-gnu.cmake](cmake/toolchains/aarch64-linux-gnu.cmake));
- the build-time gate, both ways: an unchanged rebuild must pass, an injected
  compile-time regression must fail, and `kiln-trace headers` must name its
  cause from real Clang `-ftime-trace` output;
- `kiln-dep` must find exactly the injected undeclared dependencies, and none
  in a clean fixture;
- `kiln-repro` must report all six injected causes, leave only the two that
  need source changes once toolchain fixes are on, and pass a fixed fixture.

## Limitations

- Linux only. `kiln-dep` supports x86-64 and arm64.
- On a miss, `kiln-cc` runs the preprocessor to compute the key, which costs
  time on a cold cache (see the numbers). Direct mode removes it from hits.
  It does not cache links.
- The cache speaks plaintext gRPC with no authentication; put it behind a
  TLS-terminating proxy for anything beyond a trusted network.
- `kiln-tia` is only as sound as the graph. Run `kiln-dep` in CI so undeclared
  inputs do not hide affected tests.
- Every number above comes from a synthetic fixture
  ([docs/fixture.md](docs/fixture.md)), not a production codebase.

## License

Apache-2.0
