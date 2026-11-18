#!/usr/bin/env bash
# Ensures remaimber is installed. Downloads from GitHub Releases if missing.
set -euo pipefail

REPO="erwint/remaimber"

# Whether `remaimber` is "on PATH" is a question about the user's shell, not
# about this process. The agent that runs this script has a PATH of its own:
# the MCP launchers prepend ~/.local/bin before calling it, and agents are often
# started with ~/.local/bin already there (Claude Code installs itself into it).
# Judged by that PATH, a binary the user's terminal cannot find looks installed,
# and the profile is never fixed. So ask a fresh interactive login shell.
user_shell() {
  local shell="${SHELL:-}"
  if [ -z "$shell" ]; then
    if [ "$(uname -s)" = "Darwin" ]; then
      shell="$(dscl . -read "/Users/$(id -un)" UserShell 2>/dev/null | awk '{print $2}')"
    else
      shell="$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f7)"
    fi
  fi
  printf '%s' "$shell"
}

# The PATH a new terminal gets, or "" when it cannot be learned. The shell
# starts from a bare environment, as a terminal's does: given this process's
# PATH it would pass the agent's entries straight through and confirm them. It
# runs with no stdin and a deadline: a profile that prompts or hangs must not
# stall the session that triggered this. Output is fenced by markers because an
# interactive profile may print banners of its own.
USER_PATH=""
USER_PATH_KNOWN=""
# load_user_path sets USER_PATH, starting the shell only the first time. It is
# called directly, never in $(...), or the answer would die with the subshell.
load_user_path() {
  [ -z "$USER_PATH_KNOWN" ] || return 0
  USER_PATH_KNOWN=1
  local shell out tmp pid i
  shell="$(user_shell)"
  [ -x "$shell" ] || return 0
  # Into a file, not a pipe: a profile's own background children would hold a
  # pipe open long after the shell is gone. Killed with KILL, because an
  # interactive shell ignores TERM.
  tmp="$(mktemp "${TMPDIR:-/tmp}/remaimber-path.XXXXXX")" || return 0
  env -i HOME="$HOME" USER="${USER:-$(id -un)}" LOGNAME="${LOGNAME:-$(id -un)}" SHELL="$shell" \
    ${ZDOTDIR:+ZDOTDIR="$ZDOTDIR"} ${LANG:+LANG="$LANG"} TERM=dumb PATH=/usr/bin:/bin:/usr/sbin:/sbin \
    "$shell" -ilc 'printf "\n__remaimber_path__%s__end__\n" "$PATH"' </dev/null >"$tmp" 2>/dev/null &
  pid=$!
  for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.5
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  out="$(cat "$tmp" 2>/dev/null)"
  rm -f "$tmp"
  case "$out" in
    *__remaimber_path__*__end__*)
      out="${out#*__remaimber_path__}"
      USER_PATH="${out%%__end__*}"
      ;;
  esac
}

# on_user_path DIR: yes when a new terminal finds DIR on its PATH; no when it
# does not, or when the shell could not be asked.
on_user_path() {
  load_user_path
  case ":${USER_PATH}:" in
    *":$1:"*) return 0 ;;
  esac
  return 1
}

# profile_mentions FILE DIR: whether a profile already names DIR, spelled out
# or through $HOME or ~ — the fallback when the shell could not be asked.
profile_mentions() {
  [ -f "$1" ] || return 1
  local rel="${2#"$HOME"/}"
  grep -qF -e "$2" "$1" && return 0
  [ "$rel" != "$2" ] && grep -qF -e "\$HOME/$rel" -e "\${HOME}/$rel" -e "~/$rel" "$1"
}

# Prefer a directory the user's PATH already has, so `remaimber` is runnable
# with no further setup. Only places a binary conventionally belongs are
# considered: a tool that scatters itself into whatever happens to be writable
# — another tool's ~/.cargo/bin, say — is worse than one that asks for a PATH
# entry.
pick_install_dir() {
  local candidate path
  load_user_path
  path="${USER_PATH:-$PATH}" # could not ask: the inherited PATH is the best guess
  for candidate in "${HOME}/.local/bin" "${HOME}/bin" "/usr/local/bin"; do
    case ":${path}:" in
      *":${candidate}:"*) ;;
      *) continue ;;
    esac
    # /usr/local/bin is on many PATHs but often root-owned; only take it when
    # it can be written without sudo.
    if [ -d "$candidate" ] && [ ! -w "$candidate" ]; then continue; fi
    printf '%s' "$candidate"
    return
  done
  printf '%s' "${HOME}/.local/bin"
}

# A directory once confirmed reachable is remembered, so the shell is started
# once per install rather than at every session start.
STAMP="${HOME}/.remaimber/.on-path"

# The binary is no use to the person typing commands if their shell cannot find
# it. Hooks and the MCP server reach it by absolute path, so this is invisible
# until someone tries to run it — which is why it is fixed rather than reported.
# One marked line, appended once, declinable with REMAIMBER_NO_PATH_EDIT=1.
ensure_on_path() {
  local dir="$1" shell profile line
  if [ -f "$STAMP" ] && [ "$(cat "$STAMP" 2>/dev/null)" = "$dir" ]; then
    return 0
  fi
  if on_user_path "$dir"; then
    mark_on_path "$dir"
    return 0
  fi

  if [ -n "${REMAIMBER_NO_PATH_EDIT:-}" ]; then
    echo "remaimber: ${dir} is not on your PATH; add it yourself:" >&2
    echo "           export PATH=\"${dir}:\$PATH\"" >&2
    return 0
  fi

  shell="$(user_shell)"
  case "$shell" in
    */zsh)  profile="${ZDOTDIR:-$HOME}/.zshrc" ;;
    */bash) if [ "$(uname -s)" = "Darwin" ]; then profile="${HOME}/.bash_profile"; else profile="${HOME}/.bashrc"; fi ;;
    *)      profile="" ;;
  esac

  if [ -z "$profile" ]; then
    echo "remaimber: ${dir} is not on your PATH, and your shell is not one this" >&2
    echo "           script edits. Add the equivalent of:" >&2
    echo "           export PATH=\"${dir}:\$PATH\"" >&2
    return 0
  fi

  line="export PATH=\"${dir}:\$PATH\""
  if [ -f "$profile" ] && grep -qxF "$line" "$profile"; then
    # Ours is there, yet the shell did not pick it up (or could not be asked):
    # a second copy would not help either.
    mark_on_path "$dir"
    return 0
  fi
  if [ -z "$USER_PATH" ] && profile_mentions "$profile" "$dir"; then
    mark_on_path "$dir" # could not ask, but the profile already sees to it
    return 0
  fi

  printf '\n# added by remaimber\n%s\n' "$line" >> "$profile"
  mark_on_path "$dir"
  echo "remaimber: added ${dir} to your PATH in ${profile} — open a new shell to use it"
}

mark_on_path() {
  mkdir -p "$(dirname "$STAMP")" 2>/dev/null && printf '%s\n' "$1" > "$STAMP" 2>/dev/null || true
}

# Installed already — on this process's PATH, or where a previous install put
# it. Either way, make sure the user's shell finds it too, rather than
# downloading a copy that would be just as unreachable.
existing="$(command -v remaimber 2>/dev/null || true)"
case "$existing" in
  /*) ;;
  *) existing="" ;;
esac
if [ -z "$existing" ]; then
  for dir in "${HOME}/.local/bin" "${HOME}/bin" "/usr/local/bin"; do
    if [ -x "${dir}/remaimber" ]; then
      existing="${dir}/remaimber"
      break
    fi
  done
fi
if [ -n "$existing" ]; then
  ensure_on_path "$(dirname "$existing")"
  exit 0
fi

load_user_path # once, here: pick_install_dir runs in a subshell
INSTALL_DIR="$(pick_install_dir)"

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
ensure_on_path "$INSTALL_DIR"
