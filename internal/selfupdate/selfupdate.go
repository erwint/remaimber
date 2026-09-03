// Package selfupdate replaces the running binary with the newest release.
//
// remaimber is installed two ways that never bring it up to date on their own:
// a plugin's install hook, which only runs when the binary is missing, and a
// release tarball someone downloaded once. A plugin can update itself through
// its marketplace while the CLI it depends on stays on whatever version first
// landed — a machine here sat three releases behind, with the fixes for its own
// symptoms already published.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Repo is the source of releases.
const Repo = "erwint/remaimber"

// apiBase and dlBase are variables so tests can serve their own release.
var (
	apiBase = "https://api.github.com"
	dlBase  = "https://github.com"
)

// httpClient bounds every call: this runs from a hook, where a hang is worse
// than a missed update. Transport is the default one, which reads HTTP_PROXY,
// HTTPS_PROXY and NO_PROXY — a machine that reaches GitHub only through a proxy
// is the normal case in a corporate network, and needs no configuration here.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// noRedirect resolves the release redirect without following it.
var noRedirect = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// userAgent identifies the client. GitHub rejects requests without one, and a
// filtering proxy is likelier to allow a request that says what it is.
const userAgent = "remaimber (+https://github.com/" + Repo + ")"

// Latest returns the newest published tag, e.g. "v0.9.1".
//
// The tag comes from the redirect on the releases page first, and only then
// from the API. github.com/…/releases/latest redirects to the tag, which costs
// no API quota and needs no second host allowed through a proxy — an
// unauthenticated API call is limited to 60 an hour per address, which several
// machines behind one NAT can exhaust between them, and the failure looks like
// a 403 with nothing to explain it.
func Latest(ctx context.Context) (string, error) {
	tag, redirectErr := latestFromRedirect(ctx)
	if redirectErr == nil {
		return tag, nil
	}
	tag, apiErr := latestFromAPI(ctx)
	if apiErr == nil {
		return tag, nil
	}
	return "", fmt.Errorf("release lookup failed: %v; and via the API: %v", redirectErr, apiErr)
}

func latestFromRedirect(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/%s/releases/latest", dlBase, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("%s: %s%s", url, resp.Status, bodyHint(resp))
	}
	tag := loc[strings.LastIndex(loc, "/")+1:]
	if !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("%s redirected to %q, which is not a tag", url, loc)
	}
	return tag, nil
}

func latestFromAPI(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", apiBase, Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	// A token raises the rate limit from 60 an hour to 5000, and is the usual
	// way out of a shared-address 403.
	for _, env := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if tok := os.Getenv(env); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
			break
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s%s", url, resp.Status, bodyHint(resp))
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	if release.TagName == "" {
		return "", fmt.Errorf("%s returned no tag", url)
	}
	return release.TagName, nil
}

// bodyHint quotes the start of a failed response. A bare status code hides the
// difference between a rate limit, a proxy denial and a moved repository, all
// of which arrive as 403 or 404 and want different fixes.
func bodyHint(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 300))
	if err != nil || len(b) == 0 {
		return ""
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	return " — " + text
}

// Newer reports whether latest is a higher version than current. A build with
// no version — `go install` leaves "dev" — is never newer than anything, so an
// automatic update leaves it alone rather than replacing a binary somebody
// built on purpose.
func Newer(current, latest string) bool {
	c, ok := parseVersion(current)
	if !ok {
		return false
	}
	l, ok := parseVersion(latest)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parseVersion reads the major.minor.patch of a version string, ignoring a
// leading "v" and anything after the patch — a local build reports something
// like "0.8.5-2-gabc1234-dirty", which is the 0.8.5 line.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" || v == "dev" {
		return out, false
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		if i > 2 {
			break
		}
		digits := part
		for j, r := range part {
			if r < '0' || r > '9' {
				digits = part[:j]
				break
			}
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Apply downloads the given tag and replaces the running binary, returning the
// path it wrote. The replacement is a rename onto the target, so a concurrent
// remaimber keeps running against the file it already opened rather than
// reading a half-written one.
func Apply(ctx context.Context, tag string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return ApplyTo(ctx, tag, exe)
}

// ApplyTo replaces one specific path, so the replacement itself is testable
// without a test replacing its own test binary.
func ApplyTo(ctx context.Context, tag, exe string) (string, error) {
	url := fmt.Sprintf("%s/%s/releases/download/%s/remaimber_%s_%s.tar.gz",
		dlBase, Repo, tag, runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("no remaimber binary in %s", url)
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(h.Name) != "remaimber" || h.Typeflag != tar.TypeReg {
			continue
		}
		// Written beside the target so the rename stays on one filesystem.
		tmp, err := os.CreateTemp(filepath.Dir(exe), ".remaimber-update-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(tmp.Name())
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			return "", err
		}
		if err := tmp.Close(); err != nil {
			return "", err
		}
		if err := os.Chmod(tmp.Name(), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmp.Name(), exe); err != nil {
			return "", fmt.Errorf("replace %s: %w", exe, err)
		}
		return exe, nil
	}
}
