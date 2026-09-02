#!/usr/bin/env bash
# Ensures remaimber is installed. Downloads from GitHub Releases if missing.
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

# The binary is no use to the person typing commands if the shell cannot find
# it. Hooks and the MCP server reach it by absolute path, so this is invisible
# until someone tries to run it — which is why it is fixed rather than reported.
# One marked line, appended once, declinable with REMAIMBER_NO_PATH_EDIT=1.
ensure_on_path() {
  case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) return 0 ;;
  esac

  if [ -n "${REMAIMBER_NO_PATH_EDIT:-}" ]; then
    echo "remaimber: ${INSTALL_DIR} is not on your PATH; add it yourself:" >&2
    echo "           export PATH=\"${INSTALL_DIR}:\$PATH\"" >&2
    return 0
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
    return 0
  fi

  if [ -f "$profile" ] && grep -q "added by remaimber" "$profile"; then
    return 0 # already there; a second line would just accumulate
  fi

  printf '\n# added by remaimber\nexport PATH="%s:$PATH"\n' "$INSTALL_DIR" >> "$profile"
  echo "remaimber: added ${INSTALL_DIR} to your PATH in ${profile} — open a new shell to use it"
}

# Already reachable: nothing to do.
if command -v remaimber &>/dev/null; then
  exit 0
fi

# Installed, but where the shell will not look — the state a machine lands in
# when a previous install picked a directory that is not on PATH. Fix the PATH
# rather than downloading a copy that would be just as unreachable.
for dir in "${HOME}/.local/bin" "${HOME}/bin" "/usr/local/bin"; do
  if [ -x "${dir}/remaimber" ]; then
    INSTALL_DIR="$dir"
    ensure_on_path
    exit 0
  fi
done

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
ensure_on_path
