package remote

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erwint/remaimber/internal/db"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in                         string
		scheme, host, port, prefix string
	}{
		{"ssh://build@mac-mini.local", "ssh", "build@mac-mini.local", "", ""},
		{"ssh://mac-mini/", "ssh", "mac-mini", "", ""},
		{"ssh://nas:2222/~/backups/laptop", "ssh", "nas", "2222", "~/backups/laptop"},
		{"ssh://box/srv/codex/sessions/", "ssh", "box", "", "/srv/codex/sessions"},
		{"s3://team-bucket", "s3", "team-bucket", "", ""},
		{"s3://team-bucket/remaimber/", "s3", "team-bucket", "", "remaimber"},
	}
	for _, c := range cases {
		loc, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if loc.Scheme != c.scheme || loc.Host != c.host || loc.Port != c.port || loc.Prefix != c.prefix {
			t.Errorf("Parse(%q) = %+v", c.in, *loc)
		}
	}
	for _, bad := range []string{"mac-mini:/x", "/local/path", "https://example.com/x", "s3://"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted a location it cannot sync with", bad)
		}
	}
}

func TestValidOrigin(t *testing.T) {
	for _, ok := range []string{"mac-mini", "laptop", "build.box_2"} {
		if err := ValidOrigin(ok); err != nil {
			t.Errorf("ValidOrigin(%q): %v", ok, err)
		}
	}
	// Required, never this machine's own label, and safe as a path segment.
	for _, bad := range []string{"", "local", "LOCAL", "a/b", "../x", "-x", "has space"} {
		if ValidOrigin(bad) == nil {
			t.Errorf("ValidOrigin(%q) accepted it", bad)
		}
	}
}

// fakeHome lays out the three agents' directories as a machine has them.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/projects/-Users-x-proj/aaaaaaaa-0000-0000-0000-000000000001.jsonl", claudeLines("aaaaaaaa-0000-0000-0000-000000000001", 2))
	// A subagent transcript sits deeper and is not a session of its own.
	write(".claude/projects/-Users-x-proj/aaaaaaaa-0000-0000-0000-000000000001/subagents/agent-1.jsonl", "{}\n")
	write(".codex/sessions/2026/09/03/rollout-2026-09-03T10-00-00-bbbbbbbb-0000-0000-0000-000000000002.jsonl", "{}\n")
	write(".pi/agent/sessions/--Users-x-proj--/2026-09-03T10-00-00-000Z_cccccccc-0000-0000-0000-000000000003.jsonl", "{}\n")
	write(".claude/projects/-Users-x-proj/notes.txt", "not a transcript")
	return home
}

func claudeLines(id string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"type":"user","uuid":"%s-%d","timestamp":"2026-09-03T10:00:%02dZ","message":{"role":"user","content":"line %d"},"sessionId":"%s","cwd":"/Users/x/proj"}`+"\n",
			id, i, i, i, id)
	}
	return b.String()
}

// localSSH runs an ssh source's scripts with sh on this machine, with HOME
// pointed at a fake home — the same script the remote would run.
func localSSH(t *testing.T, home string, loc string, agent string) *sshSource {
	t.Helper()
	l, err := Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newSSH(l, Options{Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	s.run = func(ctx context.Context, script string, w io.Writer) error {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdout = w
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%w: %s", err, stderr.String())
		}
		return nil
	}
	return s
}

func TestSSHListsEveryAgentFromTheHome(t *testing.T) {
	home := fakeHome(t)
	src := localSSH(t, home, "ssh://box", "")
	objs, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Object{}
	for _, o := range objs {
		got[o.Key] = o
	}
	want := []string{
		"claude/-Users-x-proj/aaaaaaaa-0000-0000-0000-000000000001.jsonl",
		"codex/2026/09/03/rollout-2026-09-03T10-00-00-bbbbbbbb-0000-0000-0000-000000000002.jsonl",
		"pi/--Users-x-proj--/2026-09-03T10-00-00-000Z_cccccccc-0000-0000-0000-000000000003.jsonl",
	}
	if len(objs) != len(want) {
		t.Errorf("listed %d objects, want %d: %v", len(objs), len(want), objs)
	}
	for _, k := range want {
		o, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
			continue
		}
		if o.Size == 0 || !strings.Contains(o.ETag, "-") {
			t.Errorf("%s: size %d etag %q, want size and a size-mtime etag", k, o.Size, o.ETag)
		}
	}

	// And fetching returns the file's bytes.
	var buf bytes.Buffer
	key := want[0]
	if err := src.Fetch(context.Background(), key, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"line 1"`) {
		t.Errorf("fetched %q", buf.String())
	}
}

// A path with --agent is that agent's directory, wherever it is kept.
func TestSSHPathWithAgent(t *testing.T) {
	home := fakeHome(t)
	src := localSSH(t, home, "ssh://box/"+filepath.Join(home, ".codex", "sessions"), "codex")
	objs, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || !strings.HasPrefix(objs[0].Key, "codex/2026/") {
		t.Errorf("objs = %v, want the one codex rollout keyed under codex/", objs)
	}
}

// A home without any agent directories is an empty machine, not an error.
func TestSSHEmptyHome(t *testing.T) {
	src := localSSH(t, t.TempDir(), "ssh://box", "")
	objs, err := src.List(context.Background())
	if err != nil || len(objs) != 0 {
		t.Errorf("List = %v, %v; want nothing and no error", objs, err)
	}
}

func TestParseS3Listing(t *testing.T) {
	data := []byte(`[
		{"Key": "homes/laptop/.claude/projects/-p/x.jsonl", "Size": 12, "ETag": "\"abc123\""},
		{"Key": "homes/laptop/.claude/projects/-p/readme.txt", "Size": 3, "ETag": "\"zzz\""}
	]`)
	objs, err := parseS3Listing(data, "claude", "homes/laptop/.claude/projects/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != "claude/-p/x.jsonl" || objs[0].ETag != "abc123" || objs[0].Size != 12 {
		t.Errorf("objs = %+v, want the transcript keyed under its agent, its etag unquoted", objs)
	}
	// The CLI prints null for a prefix with nothing under it.
	if objs, err := parseS3Listing([]byte("null\n"), "claude", "x/"); err != nil || len(objs) != 0 {
		t.Errorf("null listing = %v, %v", objs, err)
	}
}

// fakeBucket stands in for the AWS CLI over an in-memory bucket.
type fakeBucket struct {
	objects map[string]string // full key -> content
	lists   []string          // prefixes listed
	puts    []string          // destination URLs
}

func (b *fakeBucket) run(_ context.Context, args []string, _ io.Reader, w io.Writer) error {
	switch {
	case args[0] == "s3api":
		var prefix string
		for i, a := range args {
			if a == "--prefix" {
				prefix = args[i+1]
			}
		}
		b.lists = append(b.lists, prefix)
		var items []string
		for k, v := range b.objects {
			if strings.HasPrefix(k, prefix) {
				items = append(items, fmt.Sprintf(`{"Key":%q,"Size":%d,"ETag":"\"%x\""}`, k, len(v), len(v)))
			}
		}
		if len(items) == 0 {
			_, err := io.WriteString(w, "null")
			return err
		}
		_, err := io.WriteString(w, "["+strings.Join(items, ",")+"]")
		return err
	case args[0] == "s3" && args[len(args)-1] == "-":
		key := strings.TrimPrefix(args[len(args)-2], "s3://bucket/")
		_, err := io.WriteString(w, b.objects[key])
		return err
	default:
		b.puts = append(b.puts, args[len(args)-1])
		return nil
	}
}

func s3With(t *testing.T, b *fakeBucket, loc, agent string) *s3Store {
	t.Helper()
	l, err := Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	s, err := newS3(l, Options{Agent: agent})
	if err != nil {
		t.Fatal(err)
	}
	s.run = b.run
	return s
}

// A bucket some other system fills with a copy of each machine's home holds
// more than sessions. Only the agents' own directories are listed, and in
// their native layout.
func TestS3ReadsAHomeCopiedIntoTheBucket(t *testing.T) {
	b := &fakeBucket{objects: map[string]string{
		"homes/laptop/.claude/projects/-Users-x-proj/" + idA + ".jsonl":                                                  claudeLines(idA, 1),
		"homes/laptop/.codex/sessions/2026/10/01/rollout-2026-10-01T10-00-00-bbbbbbbb-0000-0000-0000-000000000002.jsonl": "{}\n",
		"homes/laptop/Documents/taxes.jsonl":                                                                             "not a session",
		"homes/laptop/.claude/settings.json":                                                                             "{}",
		"homes/desk/.claude/projects/-p/other.jsonl":                                                                     "another machine",
	}}
	src := s3With(t, b, "s3://bucket/homes/laptop", "")
	objs, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Key != "claude/-Users-x-proj/"+idA+".jsonl" ||
		!strings.HasPrefix(objs[1].Key, "codex/2026/10/01/") {
		t.Errorf("objs = %+v, want the one Claude and one Codex session, keyed by agent", objs)
	}
	for _, p := range b.lists {
		if !strings.HasPrefix(p, "homes/laptop/.") {
			t.Errorf("listed %q: only the agents' session directories should be listed", p)
		}
	}

	var buf bytes.Buffer
	if err := src.Fetch(context.Background(), objs[0].Key, &buf); err != nil || !strings.Contains(buf.String(), idA) {
		t.Errorf("fetch = %q, %v", buf.String(), err)
	}
}

// One agent's session directory, copied on its own under any name.
func TestS3AgentDirectoryAnywhere(t *testing.T) {
	b := &fakeBucket{objects: map[string]string{
		"laptop/claude-projects/-Users-x-proj/" + idA + ".jsonl": claudeLines(idA, 1),
	}}
	src := s3With(t, b, "s3://bucket/laptop/claude-projects", "claude")
	objs, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Key != "claude/-Users-x-proj/"+idA+".jsonl" {
		t.Errorf("objs = %+v", objs)
	}

	// And a push writes the native layout back, so a pull reads it the same way.
	if err := src.Put(context.Background(), "claude/-p/x.jsonl", "/tmp/x.jsonl"); err != nil {
		t.Fatal(err)
	}
	if len(b.puts) != 1 || b.puts[0] != "s3://bucket/laptop/claude-projects/-p/x.jsonl" {
		t.Errorf("put to %v", b.puts)
	}
	if src.Holds("codex") {
		t.Error("an agent-scoped location claims to hold another agent")
	}
}

// fakeSource serves transcripts from memory and counts fetches.
type fakeSource struct {
	files   map[string]string
	etags   map[string]string
	fetched []string
	failOn  string
}

func (f *fakeSource) List(context.Context) ([]Object, error) {
	var out []Object
	for k, v := range f.files {
		out = append(out, Object{Key: k, ETag: f.etags[k], Size: int64(len(v))})
	}
	return out, nil
}

func (f *fakeSource) Fetch(_ context.Context, key string, w io.Writer) error {
	if key == f.failOn {
		return errors.New("connection reset")
	}
	f.fetched = append(f.fetched, key)
	_, err := io.WriteString(w, f.files[key])
	return err
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.OpenAt(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func lockDir(t *testing.T) {
	t.Helper()
	// The import lock lives in the state directory, under HOME.
	t.Setenv("HOME", t.TempDir())
}

const idA = "aaaaaaaa-0000-0000-0000-00000000000a"

func TestPullFetchesOnlyWhatChanged(t *testing.T) {
	lockDir(t)
	database := testDB(t)
	key := "claude/-Users-x-proj/" + idA + ".jsonl"
	src := &fakeSource{
		files: map[string]string{key: claudeLines(idA, 2)},
		etags: map[string]string{key: "v1"},
	}
	opts := PullOptions{Origin: "mac-mini", Source: "ssh://mac-mini"}

	st, err := Pull(context.Background(), database, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fetched != 1 || st.Imported != 1 {
		t.Fatalf("first pull = %+v, want one fetched and imported", st)
	}
	if origin, ok := db.SessionOrigin(database, idA); !ok || origin != "mac-mini" {
		t.Errorf("session origin = %q, %v; want mac-mini", origin, ok)
	}

	// Same etag: nothing is fetched.
	st, err = Pull(context.Background(), database, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Unchanged != 1 || st.Fetched != 0 || len(src.fetched) != 1 {
		t.Errorf("second pull = %+v after %d fetch(es), want it unchanged and not fetched", st, len(src.fetched))
	}

	// The transcript grew: fetched again, and only the new line is new.
	src.files[key] = claudeLines(idA, 3)
	src.etags[key] = "v2"
	if _, err := Pull(context.Background(), database, src, opts); err != nil {
		t.Fatal(err)
	}
	var n int
	database.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, idA).Scan(&n)
	if n != 3 || len(src.fetched) != 2 {
		t.Errorf("%d messages after %d fetches, want 3 after 2", n, len(src.fetched))
	}
}

// A session this machine recorded must never be relabelled as another's, and
// one pulled under one origin is not claimed again under another.
func TestPullLeavesSessionsItDoesNotOwn(t *testing.T) {
	lockDir(t)
	database := testDB(t)
	key := "claude/-Users-x-proj/" + idA + ".jsonl"
	if _, err := database.Exec(`INSERT INTO sessions (session_id, project_key) VALUES (?, '-p')`, idA); err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{files: map[string]string{key: claudeLines(idA, 2)}, etags: map[string]string{key: "v1"}}

	st, err := Pull(context.Background(), database, src, PullOptions{Origin: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Claimed["local"] != 1 || st.Imported != 0 {
		t.Errorf("pull = %+v, want the local session left alone", st)
	}
	// Whose it is shows in the key, so it is not even downloaded.
	if len(src.fetched) != 0 {
		t.Errorf("fetched %v, a session it was going to leave alone", src.fetched)
	}
	if origin, _ := db.SessionOrigin(database, idA); origin != "" {
		t.Errorf("a local session was relabelled %q", origin)
	}
}

// A transfer that failed is not recorded, so the next pull tries it again.
func TestPullRetriesWhatFailed(t *testing.T) {
	lockDir(t)
	database := testDB(t)
	key := "claude/-Users-x-proj/" + idA + ".jsonl"
	src := &fakeSource{
		files: map[string]string{key: claudeLines(idA, 1)}, etags: map[string]string{key: "v1"}, failOn: key,
	}
	st, _ := Pull(context.Background(), database, src, PullOptions{Origin: "laptop"})
	if st.Failed != 1 {
		t.Fatalf("pull = %+v, want one failure", st)
	}
	src.failOn = ""
	st, err := Pull(context.Background(), database, src, PullOptions{Origin: "laptop"})
	if err != nil || st.Imported != 1 {
		t.Errorf("retry = %+v, %v; want the failed transcript imported", st, err)
	}
}

func TestPullRequiresAnOrigin(t *testing.T) {
	if _, err := Pull(context.Background(), testDB(t), &fakeSource{}, PullOptions{}); err == nil {
		t.Error("a pull without an origin went ahead")
	}
}

// fakeSink records uploads.
type fakeSink struct{ put []string }

func (f *fakeSink) Put(_ context.Context, key, _ string) error {
	f.put = append(f.put, key)
	return nil
}

func (f *fakeSink) Holds(string) bool { return true }

func TestPushSendsOnlyWhatChangedAndNothingForgotten(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	database := testDB(t)
	// Tombstone the pi session, as `remaimber forget` would.
	if err := db.MarkPruned(database, []string{"cccccccc-0000-0000-0000-000000000003"}, "forget"); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{}
	opts := PushOptions{Dest: "s3://b/homes/laptop"}
	st, err := Push(context.Background(), database, sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Uploaded != 2 || st.Forgotten != 1 {
		t.Errorf("push = %+v (uploaded %v), want claude and codex sent, the forgotten pi session held back", st, sink.put)
	}
	for _, k := range sink.put {
		if strings.Contains(k, "subagents") {
			t.Errorf("pushed %s, which is not a session", k)
		}
	}

	st, err = Push(context.Background(), database, sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	if st.Uploaded != 0 || st.Unchanged != 2 {
		t.Errorf("second push = %+v, want nothing re-sent", st)
	}
}

// A dry run says what would move, and moves nothing: no fetch, no import, and
// no etag recorded, or the real run after it would skip everything it listed.
func TestPullDryRunListsWithoutTouching(t *testing.T) {
	lockDir(t)
	database := testDB(t)
	const idB = "bbbbbbbb-0000-0000-0000-00000000000b"
	const idL = "cccccccc-0000-0000-0000-00000000000c"
	kA := "claude/-Users-x-proj/" + idA + ".jsonl"
	kB := "claude/-Users-x-proj/" + idB + ".jsonl"
	kL := "claude/-Users-x-proj/" + idL + ".jsonl"
	kN := "claude/-Users-x-proj/" + idA + "/subagents/agent-1.jsonl"
	src := &fakeSource{
		files: map[string]string{kA: claudeLines(idA, 1), kB: claudeLines(idB, 1), kL: claudeLines(idL, 1), kN: "{}\n"},
		etags: map[string]string{kA: "v1", kB: "v1", kL: "v1", kN: "v1"},
	}
	// B was pulled before and has since changed; L is this machine's own.
	if err := db.RecordRemoteObject(database, "laptop", db.SyncPull, kB, "v0", 1, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO sessions (session_id, project_key) VALUES (?, '-p')`, idL); err != nil {
		t.Fatal(err)
	}

	st, err := Pull(context.Background(), database, src, PullOptions{Origin: "laptop", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, c := range st.Changes {
		states[c.Key] = c.Action + " " + c.Reason
	}
	if states[kA] != "fetch new" || states[kB] != "fetch changed" || states[kL] != "skip already local" ||
		states[kN] != "skip not a session" {
		t.Errorf("states = %v, want fetch new / fetch changed / skip already local / skip not a session", states)
	}
	// Every listed file is accounted for, skipped ones included.
	if len(st.Changes) != st.Listed {
		t.Errorf("dry run accounted for %d of %d listed files", len(st.Changes), st.Listed)
	}
	if len(src.fetched) != 0 {
		t.Errorf("a dry run fetched %v", src.fetched)
	}
	if _, exists := db.SessionOrigin(database, idA); exists {
		t.Error("a dry run imported a session")
	}
	if etags, _ := db.RemoteETags(database, "laptop", db.SyncPull); etags[kA] != "" || etags[kB] != "v0" {
		t.Errorf("a dry run recorded etags: %v", etags)
	}

	// So the real run still does the work.
	st, err = Pull(context.Background(), database, src, PullOptions{Origin: "laptop"})
	if err != nil || st.Imported != 2 || st.Claimed["local"] != 1 {
		t.Errorf("real run after the dry run = %+v, %v", st, err)
	}
}

func TestPushDryRunListsWithoutUploading(t *testing.T) {
	home := fakeHome(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	database := testDB(t)
	sink := &fakeSink{}
	opts := PushOptions{Dest: "s3://b/homes/laptop", DryRun: true}

	st, err := Push(context.Background(), database, sink, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.put) != 0 {
		t.Errorf("a dry run uploaded %v", sink.put)
	}
	if len(st.Changes) != 3 {
		t.Errorf("changes = %+v, want the three local sessions", st.Changes)
	}
	for _, c := range st.Changes {
		if c.Action != "upload" || c.Reason != "new" {
			t.Errorf("%s: %s %s, want upload new", c.Key, c.Action, c.Reason)
		}
	}
	opts.DryRun = false
	if st, _ := Push(context.Background(), database, sink, opts); st.Uploaded != 3 {
		t.Errorf("real push after the dry run uploaded %d, want 3", st.Uploaded)
	}
	// A second dry run lists the same files, now as skipped.
	opts.DryRun = true
	st, _ = Push(context.Background(), database, sink, opts)
	for _, c := range st.Changes {
		if c.Action != "skip" || c.Reason != "unchanged" {
			t.Errorf("%s: %s %s, want skip unchanged", c.Key, c.Action, c.Reason)
		}
	}
}
