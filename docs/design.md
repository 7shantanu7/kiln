# kiln design

kiln adds Bazel-style guarantees (shared caching, a complete dependency graph,
reproducible outputs, test selection) to an existing CMake + Ninja C++ build,
without editing its `CMakeLists.txt` files. Everything hooks in from outside:
a compiler launcher, a CMake project include, Ninja's own query tools, and a
tracer around the build.

kiln is not a build system and does not do remote execution.

```
               CMAKE_PROJECT_INCLUDE=kiln.cmake
                 |  -ffile-prefix-map, launcher
                 v
 cmake ──> build.ninja ──> ninja ──> kiln-cc ──(REAPI gRPC)──> kiln-cache
                 |           |                                 (or bazel-remote,
                 |           |                                  buildbarn, ...)
     ninja -t query/deps     └── traced by kiln-dep (seccomp + ptrace)
                 |
   kiln-trace (time)   kiln-tia (tests)   kiln-repro (two builds, diff)
```

## kiln-cache: the cache server

The cache half of the Bazel Remote Execution API v2:
`ContentAddressableStorage`, `ActionCache`, `Capabilities`, plus `ByteStream`
for blobs too large for a batch call. Any REAPI client can use it.

- **Storage.** Blobs live on disk under `cas/<2 hex>/<sha256>`, action results
  under `ac/`. Writes stream into `tmp/` and are renamed into place only after
  the size and SHA-256 are verified, so a crash never leaves a corrupt blob.
- **Eviction.** An in-memory index of size and last access is rebuilt at start
  up. Past the size limit, least recently used entries go until the store is at
  90% of the limit.
- **Integrity.** `UpdateActionResult` refuses results whose outputs are not in
  the CAS. `GetActionResult` reports a miss if an output was evicted since, so a
  client never receives a result it cannot download.
- **Metrics.** Prometheus counters on `/metrics` (hits, misses, bytes, evictions).

## kiln-cc: the compiler launcher

Set as `CMAKE_<LANG>_COMPILER_LAUNCHER` (kiln.cmake does this). For each
`-c` compile it:

1. Runs the preprocessor (`-E`) and hashes the output. Paths under the
   configured base directories are rewritten to placeholders, but only inside
   line markers (`# 12 "/path/file.h"`). A path that reaches the object file,
   such as an expanded `__FILE__`, stays in the key.
2. Builds an REAPI `Command` from the compiler's identity (resolved path plus
   a hash of `--version`) and the normalised arguments, and an input root
   holding the preprocessed source's digest. The `Action` digest is the key.
3. On a hit, downloads the object and depfile, rewrites placeholders in the
   depfile back to this checkout's paths, and writes both atomically.
4. On a miss, compiles, then uploads the outputs and the action result, unless
   `--read-only` is set (use that for untrusted pull request builds).

### Direct mode

Running the preprocessor is most of the cost of a cache hit, so kiln-cc first
tries to find the key without it, the way ccache's direct mode does, but with
the lookup table in the shared cache so every checkout benefits:

1. A **manifest key** hashes the compiler identity, the normalised arguments,
   the include-path environment (`CPATH` and friends) and the source file's
   content.
2. The manifest, stored in the action cache under that key, holds up to 8
   entries. Each lists every file an earlier compile of that source read, taken
   from the preprocessor's line markers (so system headers are included), with
   its SHA-256 and size, and the preprocessed key that compile produced.
3. kiln-cc hashes the listed files in this checkout (paths mapped through the
   base directories). If one entry matches completely, the preprocessed key
   must be the same, so it fetches that result directly: two lookups and some
   hashing instead of a preprocessor run.
4. Otherwise it falls back to the preprocessed key, and after a hit or a
   successful compile adds an entry for the files this compile read. Read-only
   builds look manifests up but never write them.

A compile is never recorded if its output can differ while its inputs do not:
it uses `__DATE__`, `__TIME__` or `__TIMESTAMP__`; it uses `__FILE__` while
base directories are rewritten but `__FILE__` is not prefix-mapped; or one of
the files it read was modified while it ran. Like ccache, direct mode does not
notice a new header that shadows an old one earlier on the include path;
`--no-direct` (or `KILN_CC_DIRECT=0`) turns it off.

### Debug info

Debug info records absolute paths. If the compile has `-g` and the base
directories are not all covered by `-ffile-prefix-map`/`-fdebug-prefix-map`,
the working directory joins the key, so such objects are only reused in the
same location. With kiln.cmake's prefix maps, debug builds share across checkouts.

Invocations it cannot cache safely run the compiler directly: links,
`-E`/`-S`, response files, stdin, and options with side outputs
(`-ftime-trace`, `--coverage`, `-save-temps`, `-gsplit-dwarf`). Cache errors
never fail a build; kiln-cc falls back to compiling locally.

## kiln-dep: undeclared dependency detection

Ninja trusts the graph. A step that reads a file it does not declare builds
fine until the day it doesn't.

kiln-dep runs Ninja under a tracer. It starts itself as a shim that sets
`PR_SET_NO_NEW_PRIVS`, installs a seccomp BPF filter, and execs Ninja. The
filter returns `SECCOMP_RET_TRACE` only for `openat`, `openat2`, `renameat`
and `renameat2` (plus `open`, `creat`, `rename` on x86-64); every other
system call runs untouched. The filter is inherited by every compiler and
script Ninja starts. At each trap the tracer reads the path from the tracee's
memory (`process_vm_readv`), resolves it against the process's working
directory or `dirfd` via `/proc`, and resumes to the syscall exit to record
only successful calls.

Ninja runs with `-k 0`, so a step that fails (often because of the very
race being looked for) does not stop the rest of the build from being traced.
Read-only opens that fail with `ENOENT` are recorded as well: a compiler
probing for a generated header that does not exist yet is direct evidence of a
race.

Each child of Ninja is one build step. Ninja starts every step as
`/bin/sh -c "<command>"`, so the step is matched to its edge by that command
line, using `ninja -t compdb`; a step that failed before writing anything
is still identified. Then, for every file a step opened under the source or
build directory:

- **Generated file, producer not upstream.** If the producing edge is not
  reachable through the step's explicit, implicit or order-only inputs, a
  clean parallel build can run the step first. Depfile dependencies do not
  count: they are only known after the step has run once. This applies both
  to files the step read and to generated files it looked for and did not find.
- **Source file, no depfile.** Steps without a depfile (custom commands) must
  declare every input. A file they read but do not list will not trigger a
  rebuild when edited.
- **Writes outside the build directory** are reported too.

The graph comes from `ninja -t targets all`, `ninja -t query` and
`ninja -t deps`, so kiln never parses `build.ninja` itself.

## kiln-tia: test impact analysis

`ctest --show-only=json-v1` maps each test to its executable. From each
changed file, kiln-tia walks forward through edges that consume it
(explicit, implicit and depfile inputs; not order-only ones, which cannot
change an output) and selects the tests whose executables it reaches. A change
to a CMake file selects every test, because it can change the graph itself.
Files no step reads select nothing and are listed separately.

Selection is only sound if the graph is complete, which is what kiln-dep
checks. An undeclared input (like the shared `.def` files in the fixture) is
invisible to kiln-tia.

## kiln-trace: build time

- `path`: joins `.ninja_log` durations with the graph and finds the critical
  path (longest chain through static inputs). With unlimited cores the build
  cannot be faster than this.
- `headers`: aggregates Clang `-ftime-trace` files: frontend/backend split,
  header parse time both inclusive and exclusive (Clang nests `Source`
  events, so a header's own time is its duration minus its children's),
  template instantiation time and the slowest translation units.
- `snapshot` / `compare`: saves CPU time, critical path and per-rule-kind CPU
  time, and fails when any grows past a threshold, as a CI gate. Metrics with
  small baselines are ignored as noise.

kiln's CI checks the gate both ways (`scripts/ci/time-gate.sh`): a second
clean build of an unchanged tree must pass, and adding `<regex>` with one
regex instantiation to the most-included header must fail it (+59% compile
CPU on the fixture) and show up in `headers` as the top template cost.

## kiln-repro: reproducibility

Copies the sources into two directories with equal-length paths, builds both
with different time zones and a clock that has moved, and compares every edge
output plus every configure-generated file the build reads.

A differing file whose inputs (explicit, implicit, depfile) are all identical
is a root cause. Other differences are inherited and not reported separately.
Root causes are classified:

| cause | evidence | fixed by toolchain settings |
|---|---|---|
| absolute paths in debug info | build root found in `.debug_*` sections | yes: `-ffile-prefix-map` |
| source path in program data (`__FILE__`) | source path ending in a source extension, in a data section | yes: `-ffile-prefix-map` |
| build path in program data | build root elsewhere in data | no: source change |
| `__DATE__` / `__TIME__` | date or time pattern in the differing bytes | yes: `SOURCE_DATE_EPOCH` |
| embedded timestamp | ISO 8601 timestamp in the differing bytes | yes: `SOURCE_DATE_EPOCH` |
| unstable generator order | same lines, different order | no: source change |
| archive metadata | identical members, different headers | yes: deterministic `ar` |

Because both roots have the same length, substituting one for the other makes
path-only differences vanish byte for byte, which is how path causes are told
apart from the rest.
