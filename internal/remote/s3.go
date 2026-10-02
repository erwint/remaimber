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

// s3Store keeps each machine's transcripts under <prefix>/<origin>/<key>.
//
// It drives the AWS CLI rather than linking an SDK: the CLI already resolves
// profiles, SSO sessions, instance roles and AWS_PROFILE exactly as the rest
// of a machine's tooling does, so a bucket that `aws s3 ls` can see needs no
// configuration here.
type s3Store struct {
	bucket  string
	base    string // "<prefix>/<origin>/", or "<origin>/" at the bucket root
	profile string
	// run executes an aws command and streams its stdout; a field so tests can
	// stand in for the CLI.
	run func(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error
}

func newS3(loc *Location, opts Options) (*s3Store, error) {
	if err := ValidOrigin(opts.Origin); err != nil {
		return nil, err
	}
	base := opts.Origin + "/"
	if loc.Prefix != "" {
		base = loc.Prefix + "/" + base
	}
	s := &s3Store{bucket: loc.Host, base: base, profile: opts.AWSProfile}
	s.run = s.runAWS
	return s, nil
}

func (s *s3Store) List(ctx context.Context) ([]Object, error) {
	var out bytes.Buffer
	// The CLI pages through list-objects-v2 itself and returns one document.
	err := s.run(ctx, []string{"s3api", "list-objects-v2",
		"--bucket", s.bucket, "--prefix", s.base,
		"--query", "Contents[].{Key: Key, Size: Size, ETag: ETag}",
		"--output", "json"}, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, s.base, err)
	}
	return parseS3Listing(out.Bytes(), s.base)
}

// parseS3Listing reads the CLI's listing. An empty prefix comes back as
// "null", which is an empty store rather than an error.
func parseS3Listing(data []byte, base string) ([]Object, error) {
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
		key := strings.TrimPrefix(it.Key, base)
		if key == it.Key || !strings.HasSuffix(key, ".jsonl") {
			continue
		}
		objs = append(objs, Object{Key: key, Size: it.Size, ETag: strings.Trim(it.ETag, `"`)})
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

func (s *s3Store) url(key string) string {
	return "s3://" + s.bucket + "/" + s.base + key
}

func (s *s3Store) Fetch(ctx context.Context, key string, w io.Writer) error {
	// "-" streams the object to stdout, so nothing is staged twice.
	return s.run(ctx, []string{"s3", "cp", "--only-show-errors", s.url(key), "-"}, nil, w)
}

func (s *s3Store) Put(ctx context.Context, key, localPath string) error {
	return s.run(ctx, []string{"s3", "cp", "--only-show-errors", localPath, s.url(key)}, nil, io.Discard)
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
