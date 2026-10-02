package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// s3Store reads and writes agent session directories kept in a bucket, laid
// out as they are in a home directory (see the package comment).
//
// It drives the AWS CLI rather than linking an SDK: the CLI already resolves
// profiles, SSO sessions, instance roles and AWS_PROFILE exactly as the rest
// of a machine's tooling does, so a bucket that `aws s3 ls` can see needs no
// configuration here.
type s3Store struct {
	bucket  string
	roots   []s3Root
	profile string
	// run executes an aws command and streams its stdout; a field so tests can
	// stand in for the CLI.
	run func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error
}

// s3Root is one agent's session directory as a key prefix: "" or ending in "/".
type s3Root struct {
	agent  string
	prefix string
}

func newS3(loc *Location, opts Options) (*s3Store, error) {
	roots, err := agentRoots(loc.Prefix, opts.Agent)
	if err != nil {
		return nil, err
	}
	s := &s3Store{bucket: loc.Host, profile: opts.AWSProfile}
	for _, r := range roots {
		p := r.path
		if p != "" {
			p += "/"
		}
		s.roots = append(s.roots, s3Root{agent: r.agent, prefix: p})
	}
	s.run = s.runAWS
	return s, nil
}

// List lists each agent's prefix separately: a bucket holding a whole home
// backup has far more under it than transcripts, and listing only the session
// directories keeps that out of the listing entirely.
func (s *s3Store) List(ctx context.Context) ([]Object, error) {
	var objs []Object
	for _, r := range s.roots {
		var out bytes.Buffer
		// The CLI pages through list-objects-v2 itself and returns one document.
		err := s.run(ctx, []string{"s3api", "list-objects-v2",
			"--bucket", s.bucket, "--prefix", r.prefix,
			"--query", "Contents[].{Key: Key, Size: Size, ETag: ETag}",
			"--output", "json"}, nil, &out)
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, r.prefix, err)
		}
		found, err := parseS3Listing(out.Bytes(), r.agent, r.prefix)
		if err != nil {
			return nil, err
		}
		objs = append(objs, found...)
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

// parseS3Listing reads the CLI's listing of one agent's prefix. A prefix with
// nothing under it comes back as "null", which is an empty directory rather
// than an error.
func parseS3Listing(data []byte, agent, prefix string) ([]Object, error) {
	var items []struct {
		Key  string
		Size int64
		ETag string
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && string(trimmed) != "null" {
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("read listing: %w", err)
		}
	}
	var objs []Object
	for _, it := range items {
		rel := strings.TrimPrefix(it.Key, prefix)
		if rel == "" || !strings.HasSuffix(rel, ".jsonl") {
			continue
		}
		// Multipart uploads give an ETag that is not an MD5, but it still
		// changes whenever the object does, which is all a sync needs.
		objs = append(objs, Object{Key: agent + "/" + rel, Size: it.Size, ETag: strings.Trim(it.ETag, `"`)})
	}
	return objs, nil
}

// url maps a sync key to its object.
func (s *s3Store) url(key string) (string, error) {
	agent, rel, ok := strings.Cut(key, "/")
	if !ok {
		return "", fmt.Errorf("sync key %q has no agent prefix", key)
	}
	for _, r := range s.roots {
		if r.agent == agent {
			return "s3://" + s.bucket + "/" + r.prefix + rel, nil
		}
	}
	return "", fmt.Errorf("sync key %q names an agent this location does not hold", key)
}

func (s *s3Store) Fetch(ctx context.Context, key string, w io.Writer) error {
	u, err := s.url(key)
	if err != nil {
		return err
	}
	// "-" streams the object to stdout, so nothing is staged twice.
	return s.run(ctx, []string{"s3", "cp", "--only-show-errors", u, "-"}, nil, w)
}

// Holds reports whether a push here would have somewhere to put an agent's
// sessions; with --agent, only that one.
func (s *s3Store) Holds(agent string) bool {
	for _, r := range s.roots {
		if r.agent == agent {
			return true
		}
	}
	return false
}

func (s *s3Store) Put(ctx context.Context, key, localPath string) error {
	u, err := s.url(key)
	if err != nil {
		return err
	}
	return s.run(ctx, []string{"s3", "cp", "--only-show-errors", localPath, u}, nil, io.Discard)
}

func (s *s3Store) runAWS(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if _, err := exec.LookPath("aws"); err != nil {
		return errors.New("the AWS CLI (aws) is not on PATH; S3 sync uses it for credentials and transfer")
	}
	if s.profile != "" {
		args = append([]string{"--profile", s.profile}, args...)
	}
	cmd := exec.CommandContext(ctx, "aws", args...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = stdin
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
