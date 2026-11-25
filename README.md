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

Pending: the full-size benchmark ([docs/benchmarks.md](docs/benchmarks.md)) is
being run; numbers will be added here only once measured.

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
