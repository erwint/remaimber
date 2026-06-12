#!/usr/bin/env bash
# Ensures remaimber is installed. Downloads from GitHub Releases if missing.
set -euo pipefail

REPO="erwint/remaimber"
INSTALL_DIR="${HOME}/.local/bin"

if command -v remaimber &>/dev/null; then
  exit 0
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "remaimber: unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

LATEST="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | cut -d'"' -f4)"
if [ -z "$LATEST" ]; then
  echo "remaimber: could not determine latest release" >&2
  exit 1
fi

URL="https://github.com/${REPO}/releases/download/${LATEST}/remaimber_${OS}_${ARCH}.tar.gz"

mkdir -p "$INSTALL_DIR"
curl -fsSL "$URL" | tar -xz -C "$INSTALL_DIR" remaimber
chmod +x "${INSTALL_DIR}/remaimber"

# Configure MCP server if not already present
remaimber setup >/dev/null 2>&1 || true

echo "remaimber ${LATEST} installed to ${INSTALL_DIR}/remaimber"
