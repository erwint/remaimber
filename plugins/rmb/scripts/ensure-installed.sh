#!/usr/bin/env bash
# Ensures remaimber is installed. Downloads from GitHub Releases if missing.
#
# A copy of the repo's scripts/ensure-installed.sh: a Codex plugin is installed
# as a self-contained directory, so it cannot reach a script outside its root.
set -euo pipefail

REPO="erwint/remaimber"

# Prefer a directory the PATH already has, so `remaimber` is runnable with no
# further setup. Only places a binary conventionally belongs are considered: a
# tool that scatters itself into whatever happens to be writable — another
# tool's ~/.cargo/bin, say — is worse than one that asks for a PATH entry.
INSTALL_DIR=""
for candidate in "${HOME}/.local/bin" "${HOME}/bin" "/usr/local/bin"; do
  case ":${PATH}:" in
    *":${candidate}:"*)
      # /usr/local/bin is on many PATHs but often root-owned; only take it when
      # it can be written without sudo.
      if [ -d "$candidate" ] && [ ! -w "$candidate" ]; then continue; fi
      INSTALL_DIR="$candidate"
      break
      ;;
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

# Deliberately no `remaimber setup` here. The plugin already supplies the hooks
# and the MCP server, so running setup would write a second copy of the hooks
# into settings.json and fire everything twice — and setup now wires up Codex and
# pi as well, which is not a Claude Code hook's business.

echo "remaimber ${LATEST} installed to ${INSTALL_DIR}/remaimber"

# Nothing on the PATH was usable, so the binary landed somewhere the shell will
# not find. Archiving still works — the hooks and the MCP server look here
# directly — but every documented command would fail for the person typing it,
# so add the directory to their shell profile. One marked line, appended once,
# and skippable with REMAIMBER_NO_PATH_EDIT=1.
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) exit 0 ;;
esac

if [ -n "${REMAIMBER_NO_PATH_EDIT:-}" ]; then
  echo "remaimber: ${INSTALL_DIR} is not on your PATH; add it yourself:" >&2
  echo "           export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
  exit 0
fi

shell="${SHELL:-}"
if [ -z "$shell" ] && [ "$(uname -s)" = "Darwin" ]; then
  shell="$(dscl . -read "/Users/$(id -un)" UserShell 2>/dev/null | awk '{print $2}')"
fi
case "$shell" in
  */zsh)  profile="${ZDOTDIR:-$HOME}/.zshrc" ;;
  */bash) if [ "$(uname -s)" = "Darwin" ]; then profile="${HOME}/.bash_profile"; else profile="${HOME}/.bashrc"; fi ;;
  *)      profile="" ;;
esac

if [ -z "$profile" ]; then
  echo "remaimber: ${INSTALL_DIR} is not on your PATH, and your shell is not one this" >&2
  echo "           script edits. Add the equivalent of:" >&2
  echo "           export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
  exit 0
fi

if [ -f "$profile" ] && grep -q "added by remaimber" "$profile"; then
  exit 0 # already there; a second line would just accumulate
fi

{
  printf '\n# added by remaimber\nexport PATH="%s:$PATH"\n' "$INSTALL_DIR"
} >> "$profile"
echo "remaimber: added ${INSTALL_DIR} to your PATH in ${profile} — open a new shell to use it"
