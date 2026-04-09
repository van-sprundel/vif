#!/bin/sh
set -eu

REPO="van-sprundel/vif"
INSTALL_DIR="${VIF_INSTALL_DIR:-/usr/local/bin}"

main() {
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    arch="$(uname -m)"

    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) echo "Unsupported architecture: $arch" >&2; exit 1 ;;
    esac

    case "$os" in
        linux|darwin) ;;
        *) echo "Unsupported OS: $os" >&2; exit 1 ;;
    esac

    if [ "${1:-}" = "" ] || [ "${1:-}" = "latest" ]; then
        tag="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)"
    else
        tag="$1"
    fi

    if [ -z "$tag" ]; then
        echo "Failed to determine release version" >&2
        exit 1
    fi

    binary="vif-${os}-${arch}"
    url="https://github.com/${REPO}/releases/download/${tag}/${binary}"

    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    echo "Downloading vif ${tag} for ${os}/${arch}..."
    curl -fsSL -o "${tmpdir}/vif" "$url"
    chmod +x "${tmpdir}/vif"

    if [ -w "$INSTALL_DIR" ]; then
        mv "${tmpdir}/vif" "${INSTALL_DIR}/vif"
    else
        echo "Installing to ${INSTALL_DIR} (requires sudo)..."
        sudo mv "${tmpdir}/vif" "${INSTALL_DIR}/vif"
    fi

    echo "vif ${tag} installed to ${INSTALL_DIR}/vif"
}

main "$@"
