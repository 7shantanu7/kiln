# The synthetic fixture (kiln-gen)

kiln is developed and benchmarked against a generated monorepo, not a real
codebase. Every number in this repository comes from that fixture, on the
machine named next to it.

## Shape

`kiln-gen` with defaults writes:

- 250 static libraries, each with 16 translation units, one header per
  module, an umbrella header and one test executable: **500 targets, 4,250
  translation units**.
- Each library depends on 1 to 3 of the 40 libraries before it, so the graph
  is a layered DAG with real link dependencies.
- Every 10th library runs a Python code generator (`tools/codegen.py`) on a
  `.def` file to produce a header.
- Each module computes a value with `std::array`, `std::sort`, `std::map` and
  `std::string`, so files take a realistic fraction of a second to compile.

Every value the C++ computes is also computed by the Go model in
`internal/fixture`, so the generated tests check real expected values. If a
cache served a wrong object file, tests would fail.

The fixture is deterministic for a given `-seed`.

## Injected bugs

`-missing-deps N` alternates between two kinds of undeclared dependency:

1. **Generated-header race.** A library includes another library's generated
   header and adds that library's generated include directory by hand,
   without depending on it. Whether a clean build fails depends on the order
   Ninja happens to pick, even with `-j1`, which is the point: it can pass
   locally and fail in CI.
2. **Undeclared codegen input.** A `.def` file includes a shared `.def` the
   custom command does not list in `DEPENDS`. Editing the shared file does not
   regenerate the header.

`-nondet` injects reproducibility bugs (`all`, or a comma list):

| kind | what the fixture does |
|---|---|
| `date-time` | every 20th library embeds `__DATE__ " " __TIME__` |
| `configure-timestamp` | `string(TIMESTAMP)` into a configured header |
| `file-macro` | a check macro prints `__FILE__` (an absolute path) |
| `codegen-order` | the code generator iterates a Python `set`, whose order follows per-process hash randomisation |
| `configured-abspath` | `configure_file` bakes `${CMAKE_BINARY_DIR}/data` into a header |

A sixth cause, absolute paths in DWARF debug info, comes from any `-g` build;
`kiln-repro` builds `RelWithDebInfo` by default.

`kiln-fixture.json` in the generated tree records the configuration and every
injection, so tests compare tool output against ground truth.
