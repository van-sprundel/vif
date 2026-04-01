#!/usr/bin/env bash
# fetch.sh — Download real-world composer.json + composer.lock pairs for compat testing.
#
# Usage:
#   bash testdata/fixtures/compat/fetch.sh
#
# Each project lands in its own subdirectory under this script's directory.
# Existing files are not overwritten unless you pass --force.
# Only application repos that commit their lockfiles are used.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FORCE="${1:-}"

fetch() {
    local name="$1"
    local json_url="$2"
    local lock_url="$3"

    local dir="$SCRIPT_DIR/$name"
    mkdir -p "$dir"

    if [[ -f "$dir/composer.json" && -f "$dir/composer.lock" && "$FORCE" != "--force" ]]; then
        echo "[$name] already present, skipping (pass --force to re-download)"
        return
    fi

    echo "[$name] fetching composer.json ..."
    curl --silent --show-error --location --fail \
        -o "$dir/composer.json" \
        "$json_url"

    echo "[$name] fetching composer.lock ..."
    curl --silent --show-error --location --fail \
        -o "$dir/composer.lock" \
        "$lock_url"

    echo "[$name] done"
}

# ---------------------------------------------------------------------------
# Matomo — matomo-org/matomo (~209K lockfile, analytics platform)
# ---------------------------------------------------------------------------
fetch "matomo" \
    "https://raw.githubusercontent.com/matomo-org/matomo/5.x-dev/composer.json" \
    "https://raw.githubusercontent.com/matomo-org/matomo/5.x-dev/composer.lock"

# ---------------------------------------------------------------------------
# Bagisto — bagisto/bagisto (~566K lockfile, e-commerce)
# ---------------------------------------------------------------------------
fetch "bagisto" \
    "https://raw.githubusercontent.com/bagisto/bagisto/master/composer.json" \
    "https://raw.githubusercontent.com/bagisto/bagisto/master/composer.lock"

# ---------------------------------------------------------------------------
# Monica — monicahq/monica (~698K lockfile, personal CRM, large dep tree)
# ---------------------------------------------------------------------------
fetch "monica" \
    "https://raw.githubusercontent.com/monicahq/monica/main/composer.json" \
    "https://raw.githubusercontent.com/monicahq/monica/main/composer.lock"

echo ""
echo "All fixtures ready in $SCRIPT_DIR"
echo "Run compat tests with:"
echo "  go test -tags compat -v -run TestCompat"
