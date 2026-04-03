set shell := ["bash", "-cu"]

test:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test ./...

bench:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test ./... -run=^$ -bench=. -benchmem -count=2

bench-baseline:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test -run='^$' -bench=. -benchmem -count=5 ./internal/... ./... 2>&1 | tee misc/bench_baseline.txt

bench-check:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p tmp/go-tmp tmp/go-build misc
    if [ ! -f misc/bench_baseline.txt ]; then
        echo "No baseline at misc/bench_baseline.txt — run 'just bench-baseline' first"
        exit 1
    fi
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test -run='^$' -bench=. -benchmem -count=5 ./internal/... ./... 2>&1 | tee bench-results.txt
    command -v benchstat >/dev/null 2>&1 || go install golang.org/x/perf/cmd/benchstat@latest
    benchstat misc/bench_baseline.txt bench-results.txt

compat:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test -tags compat -v -run TestCompat ./...

compat-update:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test -tags compat -v -timeout 30m -run TestCompatUpdateLockfile ./...

compat-all: compat compat-update
