# vif — Project Conventions

## What this is

Fast PHP package manager written in Go. Phase 1: lockfile-based `vif install` only.

Architecture: `/ARCHITECTURE.md`

## Go Module

- Module path: `github.com/van-sprundel/vif`
- Go version: 1.23+ (use latest stable)
- All internal packages under `internal/`

## Commands

```bash
# Build
go build -o vif .

# Test (all)
go test ./...

# Test (verbose, single package)
go test -v ./internal/cache/

# Benchmarks
go test -bench=. -benchmem -count=5 ./internal/...

# Lint
go vet ./...
```

## Code Style

- Standard `gofmt` formatting (enforced)
- Table-driven tests
- Errors: wrap with `fmt.Errorf("context: %w", err)` — always add context
- No naked returns
- Interfaces at the consumer, not the producer
- No `init()` functions

## Testing Rules

- TDD: tests first, then implementation
- Every exported function has at least one test
- Use `t.TempDir()` for filesystem tests — never write to the working directory
- Use `testdata/` for fixtures
- Use `httptest.Server` for HTTP tests — never hit real networks in unit tests
- Benchmarks in every performance-sensitive package
- Build tag `//go:build integration` for slow integration tests

## Dependencies

External (keep minimal):
- `modernc.org/sqlite` — pure Go SQLite
- `github.com/spf13/cobra` — CLI

Everything else: stdlib only. Do not add dependencies without explicit approval.

## File Organization

- One type per concept, not one type per file
- `types.go` in `pkg/` for shared types
- `schema.go` in `cache/` for SQL DDL
- `templates.go` in `autoload/` for PHP template strings
- Test files next to source: `foo_test.go`

## Error Handling

- Return errors, don't panic
- Wrap with context at every boundary
- CLI layer (`cmd/`) prints errors to stderr and exits 1
- Internal packages never call `os.Exit` or `log.Fatal`

## Source

You're free to check github.com/composer/composer repository if you're unsure about a technical decision. Composer is old, but some stuff like resolving might be interesting to check.

## Phase 1 Scope

Only `vif install` reading `composer.lock`. No resolver, no `composer.json` parsing, no scripts, no auth, no Windows. See ARCHITECTURE.md for full scope.
