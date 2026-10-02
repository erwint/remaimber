package remote

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erwint/remaimber/internal/importer"
)

// sshSource reads transcripts straight out of another machine's agent
// directories. Every call is one ssh invocation running a POSIX sh script, so
// the remote needs nothing installed — not remaimber, not a particular shell.
type sshSource struct {
	dest  string
	port  string
	roots []sshRoot
	// run executes a script on the remote and streams its stdout; a field so
	// tests can run the same script locally.
	run func(ctx context.Context, script string, stdout io.Writer) error
}

// sshRoot is one agent's session directory, as a shell word that expands on
// the remote (so "~" is the remote home, not this one's).
type sshRoot struct {
	agent string
	dir   string
	// maxDepth bounds the walk: Claude Code and pi keep sessions one directory
	// down, and deeper files (subagent transcripts) are not sessions.
	maxDepth int
}

func newSSH(loc *Location, opts Options) (*sshSource, error) {
	s := &sshSource{dest: loc.Host, port: loc.Port}
	s.run = s.runSSH

	base := loc.Prefix
	if base == "" {
		base = "~"
	}
	if opts.Agent != "" {
		// The path is that agent's session directory itself.
		if _, ok := importer.AgentRoots[opts.Agent]; !ok {
			return nil, fmt.Errorf("unknown agent %q: want claude, codex or pi", opts.Agent)
		}
		s.roots = []sshRoot{{agent: opts.Agent, dir: shellPath(base), maxDepth: depthFor(opts.Agent)}}
		return s, nil
	}
	// Otherwise it is a home directory, holding each agent where it would.
	for _, agent := range []string{importer.AgentClaude, importer.AgentCodex, importer.AgentPi} {
		s.roots = append(s.roots, sshRoot{
			agent:    agent,
			dir:      shellPath(strings.TrimSuffix(base, "/") + "/" + importer.AgentRoots[agent]),
			maxDepth: depthFor(agent),
		})
	}
	return s, nil
}

func depthFor(agent string) int {
	if agent == importer.AgentCodex {
		return 0 // dated directories, any depth
	}
	return 2
}

// shellPath renders a remote path as a shell word, letting a leading ~ expand
// to the remote home and quoting the rest.
func shellPath(p string) string {
	switch {
	case p == "~":
		return `"$HOME"`
	case strings.HasPrefix(p, "~/"):
		return `"$HOME"/` + shquote(strings.TrimPrefix(p, "~/"))
	}
	return shquote(p)
}

// listScript prints "<agent>\t<size> <mtime> ./<relative path>" per transcript.
// stat differs between GNU and BSD, so its format flag is chosen once, by
// trying the GNU one; a file that vanishes between find and stat is skipped
// rather than leaving half a line.
func (s *sshSource) listScript() string {
	var b strings.Builder
	b.WriteString(`if stat -c %s . >/dev/null 2>&1; then F='-c%s %Y %n'; else F='-f%z %m %N'; fi
list() {
	[ -d "$2" ] || return 0
	( cd "$2" && find . $3 -type f -name '*.jsonl' 2>/dev/null | while IFS= read -r f; do
		s=$(stat "$F" "$f" 2>/dev/null) && printf '%s\t%s\n' "$1" "$s"
	done )
}
`)
	for _, r := range s.roots {
		depth := `''`
		if r.maxDepth > 0 {
			depth = "'-maxdepth " + strconv.Itoa(r.maxDepth) + "'"
		}
		// $3 is deliberately unquoted in the function so it splits into
		// "-maxdepth" and the number, or vanishes when empty.
		fmt.Fprintf(&b, "list %s %s %s\n", r.agent, r.dir, depth)
	}
	return b.String()
}

func (s *sshSource) List(ctx context.Context) ([]Object, error) {
	var out bytes.Buffer
	if err := s.run(ctx, s.listScript(), &out); err != nil {
		return nil, fmt.Errorf("list %s: %w", s.dest, err)
	}
	return parseSSHListing(&out)
}

// parseSSHListing reads listScript's output. The etag is size and mtime: what
// the local importer already trusts to mean "unchanged", and all a plain
// directory can offer without hashing every file on every sync.
func parseSSHListing(r io.Reader) ([]Object, error) {
	var objs []Object
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		agent, rest, ok := strings.Cut(sc.Text(), "\t")
		if !ok {
			continue
		}
		fields := strings.SplitN(rest, " ", 3)
		if len(fields) != 3 {
			continue
		}
		size, err1 := strconv.ParseInt(fields[0], 10, 64)
		mtime, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rel := strings.TrimPrefix(fields[2], "./")
		objs = append(objs, Object{
			Key:  agent + "/" + rel,
			ETag: fmt.Sprintf("%d-%d", size, mtime),
			Size: size,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

func (s *sshSource) Fetch(ctx context.Context, key string, w io.Writer) error {
	agent, rel, ok := strings.Cut(key, "/")
	if !ok {
		return fmt.Errorf("sync key %q has no agent prefix", key)
	}
	for _, r := range s.roots {
		if r.agent == agent {
			return s.run(ctx, "cat -- "+r.dir+"/"+shquote(rel), w)
		}
	}
	return fmt.Errorf("sync key %q names an agent this location does not hold", key)
}

// runSSH runs a script on the remote. BatchMode makes a host that would ask
// for a password fail at once instead of hanging a sync on a prompt nobody
// sees; the script goes through sh explicitly, since the login shell may not
// be a POSIX one.
func (s *sshSource) runSSH(ctx context.Context, script string, stdout io.Writer) error {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
	if s.port != "" {
		args = append(args, "-p", s.port)
	}
	args = append(args, s.dest, "sh -c "+shquote(script))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}
