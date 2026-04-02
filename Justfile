set shell := ["bash", "-cu"]

test:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test ./...

bench:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test ./... -run=^$ -bench=. -benchmem -count=2

compat:
    mkdir -p tmp/go-tmp tmp/go-build
    GOTMPDIR="$PWD/tmp/go-tmp" GOCACHE="$PWD/tmp/go-build" go test -tags compat -v -run TestCompat ./...
