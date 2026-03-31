# vif Architecture

Fast PHP package manager written in Go. Lockfile-based install that aims to be significantly faster than Composer.

## Philosophy

- Speed through parallelism, caching, and hardlinks — not through clever algorithms
- Composer compatibility: read `composer.lock`, produce a `vendor/` that works identically
- Phase 1 is lockfile-only (`vif install`). No resolver, no update, no require.

---

## Go Module Structure

Module path: `github.com/van-sprundel/vif`

```
go.mod
go.sum
main.go                              # Entrypoint: cobra root command
cmd/
  root.go                            # Root cobra command, global flags
  install.go                         # `vif install` command wiring
internal/
  lockfile/
    lockfile.go                      # Parse composer.lock -> []Package
    lockfile_test.go
  pkg/
    types.go                         # Package, Dist, Autoload — shared types
  cache/
    cache.go                         # SQLite-backed global cache: lookup, store, path helpers
    cache_test.go
    schema.go                        # DDL statements
  downloader/
    downloader.go                    # Parallel HTTP download + zip extraction into cache
    downloader_test.go
  installer/
    installer.go                     # Populate vendor/ from cache via hardlinks (fallback: copy)
    installer_test.go
  autoload/
    autoload.go                      # Generate vendor/autoload.php and supporting files
    autoload_test.go
    classmap.go                      # Scan PHP files for class/interface/trait/enum declarations
    classmap_test.go
    templates.go                     # Embedded PHP template strings
  ui/
    ui.go                            # Terminal progress bar, summary output
testdata/
  fixtures/                          # composer.lock samples, fake zip archives for tests
```

### Package responsibilities

| Package | Does | Does NOT |
|---|---|---|
| `lockfile` | Parse `composer.lock` into `[]pkg.Package` | Validate versions, resolve anything |
| `pkg` | Define shared types | Contain logic |
| `cache` | SQLite metadata ops, cache directory layout, existence checks | Download, extract |
| `downloader` | Parallel HTTP GET, zip extraction into cache dirs, SHA verification | Know about vendor/ |
| `installer` | Hardlink/copy from cache to `vendor/<name>/`, write `installed.json` | Download anything |
| `autoload` | Generate all autoload PHP files from package autoload metadata | Resolve dependencies |
| `ui` | Progress bars, timing, summary | Business logic |

---

## Key Types

### `internal/pkg/types.go`

```go
package pkg

type Package struct {
    Name    string   `json:"name"`
    Version string   `json:"version"`
    Type    string   `json:"type"`
    Dist    Dist     `json:"dist"`
    Autoload    Autoload `json:"autoload"`
    AutoloadDev Autoload `json:"autoload-dev"`
}

type Dist struct {
    Type      string `json:"type"`      // "zip"
    URL       string `json:"url"`
    Reference string `json:"reference"`
    Shasum    string `json:"shasum"`
}

type Autoload struct {
    PSR4     map[string]any `json:"psr-4"`     // namespace -> string | []string
    PSR0     map[string]any `json:"psr-0"`
    Classmap []string       `json:"classmap"`
    Files    []string       `json:"files"`
}
```

### `internal/lockfile/lockfile.go`

```go
package lockfile

type LockFile struct {
    ContentHash string        `json:"content-hash"`
    Packages    []pkg.Package `json:"packages"`
    PackagesDev []pkg.Package `json:"packages-dev"`
}
```

---

## Cache Design

Location: `$XDG_CACHE_HOME/vif/` (default: `~/.cache/vif/`).

```
~/.cache/vif/
  vif.db                             # SQLite database
  files/
    <cache_key>/                     # SHA-256 hex of dist URL
      archive.zip
      extracted/                     # Unzipped package contents
```

### SQLite schema

```sql
CREATE TABLE IF NOT EXISTS packages (
    name       TEXT NOT NULL,
    version    TEXT NOT NULL,
    dist_url   TEXT NOT NULL,
    dist_ref   TEXT NOT NULL,
    cache_key  TEXT NOT NULL,
    cached_at  INTEGER NOT NULL,
    PRIMARY KEY (name, version)
);
```

### Cache flow

1. Compute `cache_key = sha256hex(dist.url)` for each package
2. Query SQLite + check `files/<cache_key>/extracted/` exists on disk
3. If miss: download zip, extract, insert SQLite row
4. If hit: skip download entirely

---

## Phase 1 Command Surface

```
vif install [--verbose|-v]
```

Reads `composer.lock` from current directory. No `composer.json` required for Phase 1 (all needed data is in the lockfile).

**Behavior:**
1. Parse `composer.lock` -> list of packages (both `packages` and `packages-dev`)
2. For each package, check cache; download if missing (parallel)
3. Remove existing `vendor/` contents (packages only, preserve `vendor/autoload.php` flow)
4. Hardlink from cache to `vendor/<package-name>/` (fallback: copy)
5. Generate autoloader files
6. Write `vendor/composer/installed.json`
7. Print summary with timing

**Flags:**
- `--verbose` / `-v`: per-package output lines instead of progress bar

**Exit codes:**
- 0: success
- 1: error (missing lockfile, download failure, etc.)

**Not in Phase 1:** `--no-dev`, `--no-scripts`, `--no-autoloader` (these are trivial to add later; keeping the flag surface minimal for now).

---

## Autoloader Generation

Generate files that match Composer's output structure so existing PHP code works unchanged.

### Files generated

| File | Purpose |
|---|---|
| `vendor/autoload.php` | Entry point (~5 LOC): require ClassLoader, return instance |
| `vendor/composer/ClassLoader.php` | Embedded copy of Composer's ClassLoader (MIT licensed, ~500 LOC) |
| `vendor/composer/autoload_real.php` | Registers the loader, includes static/files |
| `vendor/composer/autoload_static.php` | Static arrays: `$prefixLengthsPsr4`, `$prefixDirsPsr4`, `$prefixesPsr0`, `$classMap`, `$files` |
| `vendor/composer/autoload_psr4.php` | Return PSR-4 map |
| `vendor/composer/autoload_namespaces.php` | Return PSR-0 map |
| `vendor/composer/autoload_classmap.php` | Return classmap (from scanned classmap dirs) |
| `vendor/composer/autoload_files.php` | Return files map with deterministic hash keys |
| `vendor/composer/installed.json` | Package metadata (Composer v2 format) |

### Classmap scanning

For packages declaring `classmap` autoload entries, scan the listed directories/files for PHP class declarations. Use regex: `(?:class|interface|trait|enum)\s+(\w+)` with `namespace\s+([\w\\]+)` detection. No full PHP parser needed.

### Autoload files hash

Hash key for each file entry: `md5(packageName + ":" + relativeFilePath)`. Deterministic and unique per file.

### Root package autoload

Phase 1 does NOT process root `composer.json` autoload (the project's own PSR-4 etc.). Only vendor package autoload entries are included. Root autoload can be added as a fast follow-up.

---

## Dependencies

```
modernc.org/sqlite              # Pure Go SQLite (no CGO)
github.com/spf13/cobra          # CLI framework
```

Everything else is stdlib:
- `net/http` — downloads
- `encoding/json` — lockfile parsing
- `archive/zip` — extraction
- `crypto/sha256` — cache keys
- `os` — hardlinks (`os.Link`)
- `sync` / `context` — concurrency
- `io/fs` — file walking

### Why these choices

- **modernc.org/sqlite over mattn/go-sqlite3**: No CGO. Cache ops are not on the hot path.
- **Cobra**: Gives subcommands, flags, help for free. We will need subcommands in Phase 2+.
- **No HTTP client library**: `net/http` is sufficient. We don't need retries or auth in Phase 1.

---

## Testing Strategy

### TDD approach

Every task starts with tests. The implementation PR must include tests that:
1. Were written (or outlined) before the implementation
2. Cover the happy path and at least one meaningful error case
3. Use table-driven tests where there are multiple input variations

### Test categories

| Category | Location | What |
|---|---|---|
| Unit tests | `*_test.go` next to source | Each package in isolation. Use interfaces for dependencies. |
| Integration tests | `internal/*/integration_test.go` | Real SQLite, real filesystem, real zip files. Build-tagged if slow. |
| End-to-end | `e2e_test.go` or `cmd/*_test.go` | Full `vif install` against fixture lockfile + HTTP test server. |

### Fixture strategy

- `testdata/fixtures/` contains real `composer.lock` files (small, ~5 packages)
- Test zip archives: create in-memory during tests using `archive/zip`, or commit tiny fixtures
- HTTP: use `httptest.Server` for download tests

### Benchmarks

Every performance-sensitive package includes benchmarks:

```
cache/       — BenchmarkLookup, BenchmarkInsert
downloader/  — BenchmarkDownloadParallel (with httptest, measures concurrency scaling)
installer/   — BenchmarkHardlink, BenchmarkCopy (measure per-file overhead)
autoload/    — BenchmarkGenerate (measure with 100+ packages)
lockfile/    — BenchmarkParse (with large lockfile fixture)
```

Benchmarks are run in CI with `go test -bench=. -benchmem -count=5`. Results stored for comparison across commits.

### Regression gates

- All tests must pass before merge
- Benchmark results compared against baseline; regressions >10% flagged (not blocking initially, but tracked)
- `go vet` and `staticcheck` in CI

---

## Performance Measurement

### What to measure

| Metric | How | Where |
|---|---|---|
| Cold install (empty cache) | Wall clock, `time vif install` | E2E benchmark |
| Warm install (full cache) | Wall clock | E2E benchmark |
| Download throughput | Packages/sec, bytes/sec | `downloader` benchmark |
| Hardlink throughput | Files/sec | `installer` benchmark |
| Cache lookup latency | ns/op | `cache` benchmark |
| Autoloader generation | ms for N packages | `autoload` benchmark |
| Memory allocation | `-benchmem` | All benchmarks |

### Comparison target

Compare against `composer install` on the same lockfile. The goal is measurable, significant improvement — not a specific multiplier.

---

## Phase 1 Task Breakdown

Ordered. Each task includes its own tests.

### Task 1: Project scaffold + types

Set up Go module, directory structure, `pkg/types.go`, cobra root + install command (stub). Verify `go build` and `go test ./...` pass.

**AC:** `go build` produces `vif` binary. `vif install` prints "not implemented". All directories exist.

### Task 2: Lockfile parser

Parse `composer.lock` into `LockFile` struct. Table-driven tests with real fixture lockfiles (small). Benchmark with a larger fixture (~100 packages).

**AC:** Parse a real `composer.lock`, return correct package count, names, dist URLs. Benchmark exists.

### Task 3: Cache layer (SQLite)

SQLite database creation, schema migration, insert, lookup, cache key computation, path helpers. Tests use temp directories.

**AC:** Insert a package, look it up, verify paths. Benchmark lookup/insert. Handles concurrent access (WAL mode).

### Task 4: Parallel downloader

Download zip files from URLs, extract into cache directory structure. Integrate with cache layer (skip cached packages). Worker pool with configurable concurrency. Tests use `httptest.Server`.

**AC:** Download N packages in parallel from test server. Verify extracted files on disk. Verify cache hit skips download. Benchmark parallel vs sequential.

### Task 5: Installer (vendor population)

Hardlink files from cache `extracted/` to `vendor/<name>/`. Fallback to copy on cross-device. Remove stale packages. Write `vendor/composer/installed.json`.

**AC:** Given cached packages, produce correct `vendor/` layout. Verify hardlinks (same inode). Verify copy fallback works. Benchmark.

### Task 6: Autoloader generation

Generate all autoload PHP files from package metadata. Include classmap scanner. Embed `ClassLoader.php`.

**AC:** Generated autoloader files match Composer's output for a test fixture (diff should be minimal/explainable). PHP `require 'vendor/autoload.php'` works and classes resolve. Benchmark with 100+ packages.

### Task 7: CLI wiring + progress UI

Wire all layers together in `cmd/install.go`. Progress bar for downloads and installs. Summary with timing. Error handling with context.

**AC:** `vif install` in a directory with a `composer.lock` produces a working `vendor/`. Output shows progress and timing. Missing lockfile prints error and exits 1.

### Task 8: End-to-end test + comparison benchmark

E2E test that runs full `vif install` against a real (small) project. Benchmark comparing cold and warm install times. Document results.

**AC:** E2E test passes. Benchmark numbers recorded. README or benchmark file shows comparison methodology.

---

## Decisions Log

1. **No `composer.json` parsing in Phase 1.** The lockfile contains all data needed: package names, versions, dist URLs, autoload config. We skip root-package autoload, scripts, installer-paths, and config. This dramatically simplifies Phase 1.

2. **Hardlinks over symlinks.** Symlinks break when cache moves. Hardlinks are transparent to PHP. Cross-device fallback to copy.

3. **Pure Go SQLite.** No CGO build complexity. Cache operations are not performance-critical.

4. **Embedded ClassLoader.php.** Avoids downloading it or depending on a Composer installation. It's MIT licensed and stable.

5. **No root autoload in Phase 1.** Only vendor package autoload entries. Root `composer.json` autoload is a fast follow-up but adds a hard dependency on parsing `composer.json`.

6. **Flat vendor/ only.** All packages go to `vendor/<name>/`. No installer-paths, no custom install locations.

7. **Download parallelism.** Worker pool, default `min(numCPU, 16)`. Simple and effective.
