// Package remote syncs transcripts between machines, over ssh or through an S3
// bucket.
//
// What moves is the agents' own transcript files, not database rows: they are
// what every importer already reads, so a pulled conversation is parsed,
// segmented and searched by exactly the rules a local one is, and an archive
// never has to trust another machine's schema version.
//
// The two transports differ in who has to act. Over ssh the other machine is
// read directly — its agents' session directories are already the source, so
// there is nothing to publish first. An S3 bucket is passive: a machine pushes
// its transcripts into it under its own name, and others pull them from there.
package remote

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// Location is where a sync reads from or writes to.
type Location struct {
	Scheme string // "ssh" or "s3"
	// Host is the ssh destination ("user@host") or the S3 bucket.
	Host string
	Port string // ssh only
	// Prefix narrows the location. Over ssh it is a directory on the remote
	// (default: the remote home); in S3 it is a key prefix (default: the
	// bucket root). Either way, may be empty.
	Prefix string
}

func (l *Location) String() string {
	host := l.Host
	if l.Port != "" {
		host += ":" + l.Port
	}
	if l.Prefix == "" {
		return l.Scheme + "://" + host
	}
	if strings.HasPrefix(l.Prefix, "/") {
		return l.Scheme + "://" + host + l.Prefix
	}
	return l.Scheme + "://" + host + "/" + l.Prefix
}

// Parse reads a location:
//
//	ssh://[user@]host[:port][/path]   path is absolute, or ~/… for the remote home
//	s3://bucket[/prefix]
func Parse(s string) (*Location, error) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%q is not a sync location: want ssh://[user@]host[/path] or s3://bucket[/prefix]", s)
	}
	loc := &Location{Scheme: u.Scheme}
	switch u.Scheme {
	case "ssh":
		loc.Host = u.Hostname()
		if u.User != nil {
			loc.Host = u.User.Username() + "@" + loc.Host
		}
		loc.Port = u.Port()
		// As in git's ssh:// URLs: the path is absolute, and "/~/x" names x
		// under the remote home.
		p := u.Path
		switch {
		case p == "" || p == "/":
			p = ""
		case p == "/~" || strings.HasPrefix(p, "/~/"):
			p = strings.TrimPrefix(p, "/")
		}
		loc.Prefix = strings.TrimSuffix(p, "/")
	case "s3":
		loc.Host = u.Host
		loc.Prefix = strings.Trim(u.Path, "/")
	default:
		return nil, fmt.Errorf("unsupported sync scheme %q: want ssh:// or s3://", u.Scheme)
	}
	return loc, nil
}

// Object is one transcript as a source lists it.
type Object struct {
	// Key is "<agent>/<path under that agent's session root>", the same for a
	// file whichever transport carried it.
	Key  string
	ETag string
	Size int64
}

// Source lists and reads another machine's transcripts.
type Source interface {
	List(ctx context.Context) ([]Object, error)
	Fetch(ctx context.Context, key string, w io.Writer) error
}

// Sink receives this machine's transcripts.
type Sink interface {
	Put(ctx context.Context, key, localPath string) error
}

// Options configure a transport.
type Options struct {
	// Origin names the machine whose transcripts are read or written. An S3
	// store keeps each machine under its own name; over ssh it only labels.
	Origin string
	// Agent says which agent's sessions an ssh path holds. Without it the path
	// is treated as a home directory holding the usual agent directories.
	Agent string
	// AWSProfile selects an AWS CLI profile; empty leaves AWS_PROFILE and the
	// default credential chain to decide.
	AWSProfile string
}

// OpenSource returns the reader for a location.
func OpenSource(loc *Location, opts Options) (Source, error) {
	switch loc.Scheme {
	case "ssh":
		return newSSH(loc, opts)
	case "s3":
		return newS3(loc, opts)
	}
	return nil, fmt.Errorf("unsupported sync scheme %q", loc.Scheme)
}

// OpenSink returns the writer for a location. Only a store can be pushed to:
// over ssh the other machine reads this one directly, and writing transcripts
// into its agent directories would make them look like its own.
func OpenSink(loc *Location, opts Options) (Sink, error) {
	if loc.Scheme != "s3" {
		return nil, fmt.Errorf("push needs an s3:// store; over ssh, pull from this machine on the other one instead " +
			"(remaimber sync pull ssh://<this-host> --origin <name>)")
	}
	return newS3(loc, opts)
}

var originName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidOrigin checks a machine name. It becomes a path segment in a store and a
// value people filter on, so it is kept to characters that are safe as both.
func ValidOrigin(name string) error {
	if name == "" {
		return fmt.Errorf("an origin is required: name the machine these conversations come from with --origin")
	}
	if strings.EqualFold(name, "local") {
		return fmt.Errorf(`origin "local" is reserved for this machine's own sessions`)
	}
	if !originName.MatchString(name) {
		return fmt.Errorf("origin %q: use letters, digits, '.', '_' or '-' (at most 63, starting with a letter or digit)", name)
	}
	return nil
}

// shquote quotes s as one word for a POSIX shell.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
