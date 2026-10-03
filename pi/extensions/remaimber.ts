import { spawn } from "node:child_process";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";

// remaimber archives pi's conversations alongside Claude Code's and Codex's, so
// any of them can search the others. It reads pi's session files directly; this
// extension supplies the lifecycle pi has and a file on disk does not: when a
// session starts (capture its durable repo identity while the worktree still
// exists), and when it is worth importing what has been written so far. On pi
// 1.0 and later it also registers the MCP server, for the search tools.
//
// Everything here is fire-and-forget. An archiver that can stall or fail a
// coding session is worse than one that misses an update — the next session's
// sweep catches whatever this one dropped.

// The CLI lives outside the package, so the package has to be able to fetch it —
// the Claude Code and Codex plugins run this same script from their SessionStart
// hook. Without it, installing the pi package alone leaves an extension that
// spawns a binary nobody has, and says nothing.
const installer = join(dirname(fileURLToPath(import.meta.url)), "..", "..", "scripts", "ensure-installed.sh");

/**
 * Installs remaimber if it is missing, then runs `then`. The callback matters on
 * a machine that has the package but not yet the binary: anything fired before
 * the download lands spawns a command that does not exist, so the first session
 * would archive nothing and say nothing. Exits immediately — and calls back
 * immediately — when remaimber is already there.
 */
function ensureInstalled(ctx: ExtensionContext, then: () => void): void {
  let ran = false;
  const once = () => {
    if (!ran) {
      ran = true;
      then();
    }
  };
  try {
    const child = spawn("bash", [installer], { stdio: ["ignore", "pipe", "pipe"], detached: true });
    let said = "";
    child.stdout?.on("data", (d) => (said += d));
    child.stderr?.on("data", (d) => (said += d));
    child.on("error", () => {
      ctx.ui?.notify?.("remaimber: could not run the installer; conversations are not being archived", "warning");
      once();
    });
    child.on("exit", (code) => {
      const message = said.trim();
      if (code !== 0) {
        ctx.ui?.notify?.(message || "remaimber: install failed; conversations are not being archived", "warning");
      } else if (message) {
        ctx.ui?.notify?.(message, "info"); // only speaks up when it actually installed something
      }
      once();
    });
    child.unref();
  } catch {
    once(); // the archive is best-effort; a failed spawn must not stall the session
  }
}

/** Runs remaimber detached, ignoring failure — including remaimber not being installed. */
function fire(args: string[]): void {
  try {
    const child = spawn("remaimber", args, {
      stdio: ["ignore", "ignore", "ignore"],
      detached: true,
    });
    child.on("error", () => {});
    child.unref();
  } catch {
    // A spawn that cannot even start is the same non-event as one that fails.
  }
}

/**
 * pi names a session file "<timestamp>_<uuid>.jsonl" and the uuid is the id
 * remaimber keys on, so the path is the only place the id has to come from.
 */
function sessionID(ctx: ExtensionContext): string | undefined {
  const file = ctx.sessionManager?.getSessionFile?.();
  if (!file) return undefined; // ephemeral session (--no-session): nothing to archive
  const base = basename(file).replace(/\.jsonl$/, "");
  const sep = base.indexOf("_");
  return sep >= 0 ? base.slice(sep + 1) : base;
}

/**
 * The MCP server gives pi the search tools Claude Code and Codex have
 * (mcp__remaimber__find_context and the rest). It is registered from here
 * rather than written into ~/.pi/agent/mcp.json, so it arrives and leaves with
 * the package; an mcp.json entry named "remaimber" still takes precedence, which
 * is how to change its settings for good.
 *
 * pi before 1.0 has no MCP support and no registerMcpServer. There the package
 * keeps archiving and offers no tools, rather than failing to load.
 */
function registerMcp(pi: ExtensionAPI): void {
  const api = pi as unknown as {
    registerMcpServer?: (name: string, config: Record<string, unknown>) => void;
  };
  if (typeof api.registerMcpServer !== "function") return;
  try {
    api.registerMcpServer("remaimber", {
      // A stdio command is one executable, so the PATH fix the hooks get from
      // their shell line comes from sh here. The installer runs only when the
      // binary is missing, so a first session cannot race its download.
      command: "sh",
      args: [
        "-c",
        'PATH="$HOME/.local/bin:$HOME/bin:$PATH"; export PATH; ' +
          'command -v remaimber >/dev/null 2>&1 || bash "$1" >&2; exec remaimber mcp',
        "sh",
        installer,
      ],
      description:
        "Search and recall archived Claude Code, Codex and pi conversations: find where something was discussed, list past sessions, load part of one as context",
      // Loaded through tool_search when wanted, rather than declared on every
      // request: the server is listed in the system prompt with the description
      // above, which is what the model needs to know it is there.
      exposure: "deferred",
    });
  } catch {
    // Another extension holding the name, or a pi that rejects the config: the
    // archive still works, only the tools are missing.
  }
}

/** Import anything new, then summarize what has gone stale. Both self-throttle. */
function maintain(): void {
  fire(["import-if-stale"]);
  fire(["summarize-if-stale"]);
}

export default function (pi: ExtensionAPI) {
  // Registered while loading, so it connects with the session like any other
  // configured server.
  registerMcp(pi);

  pi.on("session_start", async (_event, ctx) => {
    const id = sessionID(ctx);
    const cwd = process.cwd();
    ensureInstalled(ctx, () => {
      if (id) {
        // Captured while the worktree still exists: it is what makes a session
        // started in a temporary worktree findable after that worktree is gone.
        fire(["record-identity", "--session", id, "--cwd", cwd]);
      }
      // Also catches up whatever a previous session left unfinished when it
      // ended uncleanly.
      maintain();
    });
  });

  // A settled agent is the recurring, cheap moment to archive: the turn is over,
  // the session file has just been written to, and nothing is waiting on us.
  pi.on("agent_settled", async () => maintain());

  // Compaction rewrites history in the live session; the file keeps the full
  // record, so import before the summary lands rather than after.
  pi.on("session_compact", async () => fire(["import"]));

  pi.on("session_shutdown", async (_event, ctx) => {
    const id = sessionID(ctx);
    if (id) {
      // Clears the liveness marker, so a later resume knows this session is not
      // still running somewhere.
      fire(["mark-ended", "--session", id]);
    }
    fire(["import"]);
    fire(["summarize-if-stale"]);
  });
}
