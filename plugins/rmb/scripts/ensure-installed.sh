#!/usr/bin/env bash
# Ensures remaimber is installed. Downloads from GitHub Releases if missing.
#
# A copy of the repo's scripts/ensure-installed.sh: a Codex plugin is installed
# as a self-contained directory, so it cannot reach a script outside its root.
set -euo pipefail

REPO="erwint/remaimber"

# Install into a directory the user's PATH already has, so `remaimber` works in
# their shell with no further setup. Only conventional per-user bin directories
# are considered — scattering a binary into whatever happens to be writable would
# be worse than the fallback. That fallback is ~/.local/bin, which the hooks and
# the MCP server look in regardless of PATH.
INSTALL_DIR=""
for candidate in "${HOME}/.local/bin" "${HOME}/bin"; do
  case ":${PATH}:" in
    *":${candidate}:"*) INSTALL_DIR="$candidate"; break ;;
  esac
done
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

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

# The plugin registers the MCP server and the hooks itself, so nothing to
# configure here — `remaimber setup` would write Claude Code's settings.json.

echo "remaimber ${LATEST} installed to ${INSTALL_DIR}/remaimber"
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *)
    echo "remaimber: installed to ${INSTALL_DIR}, which is not on your PATH." >&2
    echo "remaimber: archiving works anyway — the hooks and the MCP server look there directly." >&2
    echo "remaimber: to run it yourself, add to ~/.zshrc (or ~/.bashrc):" >&2
    echo "           export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
    ;;
esac

