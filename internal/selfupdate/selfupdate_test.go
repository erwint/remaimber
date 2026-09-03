package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"0.8.5", "v0.8.6", true},
		{"v0.8.6", "v0.8.6", false},
		{"0.9.0", "v0.8.6", false},
		{"0.8.6", "v0.9.0", true},
		{"0.8.6", "v1.0.0", true},
		// A local build reports its distance from the tag; it is still that line.
		{"0.8.5-2-gabc1234-dirty", "v0.8.6", true},
		{"0.8.6-1-gdef5678-dirty", "v0.8.6", false},
		// A build with no version was made deliberately (go install, a local
		// `go build`), and replacing it with a release would be a surprise.
		{"dev", "v0.8.6", false},
		{"", "v0.8.6", false},
		{"0.8.5", "not-a-version", false},
	}
	for _, c := range cases {
		if got := Newer(c.current, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// The update replaces the binary in place, which is the part worth proving: a
// half-written file at that path would break every hook on the machine.
func TestApplyReplacesTheBinary(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "remaimber")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := []byte("new binary")
	if err := tw.WriteHeader(&tar.Header{
		Name: "remaimber", Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	tarball := buf.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/"+Repo+"/releases/latest":
			// How GitHub answers: a redirect to the tag, costing no API quota.
			w.Header().Set("Location", "/"+Repo+"/releases/tag/v9.9.9")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/repos/"+Repo+"/releases/latest":
			w.Write([]byte(`{"tag_name":"v9.9.9"}`))
		default:
			w.Write(tarball)
		}
	}))
	defer srv.Close()
	apiBase, dlBase = srv.URL, srv.URL
	defer func() { apiBase, dlBase = "https://api.github.com", "https://github.com" }()

	tag, err := Latest(context.Background())
	if err != nil || tag != "v9.9.9" {
		t.Fatalf("Latest() = %q, %v", tag, err)
	}

	if !Newer("0.8.6", tag) {
		t.Fatal("a newer release must compare as newer")
	}

	written, err := ApplyTo(context.Background(), tag, exe)
	if err != nil {
		t.Fatalf("ApplyTo: %v", err)
	}
	if written != exe {
		t.Errorf("wrote %q, want %q", written, exe)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new binary" {
		t.Errorf("binary content = %q, want the downloaded one", got)
	}
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Error("the replacement is not executable")
	}
	// Nothing may be left beside it: a stray temp file in ~/.local/bin is litter
	// on every user's PATH.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the binary", names)
	}
}

// The redirect is tried first, so a machine whose API quota is spent — or whose
// proxy allows github.com but not api.github.com — still finds the release.
func TestLatestPrefersTheRedirect(t *testing.T) {
	apiCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+Repo+"/releases/latest" {
			w.Header().Set("Location", "https://example.test/"+Repo+"/releases/tag/v1.2.3")
			w.WriteHeader(http.StatusFound)
			return
		}
		apiCalls++
		http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	apiBase, dlBase = srv.URL, srv.URL
	defer func() { apiBase, dlBase = "https://api.github.com", "https://github.com" }()

	tag, err := Latest(context.Background())
	if err != nil || tag != "v1.2.3" {
		t.Fatalf("Latest() = %q, %v; want v1.2.3", tag, err)
	}
	if apiCalls != 0 {
		t.Errorf("the API was called %d time(s) when the redirect answered", apiCalls)
	}
}

// When both routes fail, the error has to carry what the server said: a bare
// 403 hides whether it is a rate limit, a proxy denial or a moved repository,
// and each wants a different fix.
func TestLatestExplainsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"API rate limit exceeded for 203.0.113.1"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	apiBase, dlBase = srv.URL, srv.URL
	defer func() { apiBase, dlBase = "https://api.github.com", "https://github.com" }()

	_, err := Latest(context.Background())
	if err == nil {
		t.Fatal("want an error when both routes fail")
	}
	if !strings.Contains(err.Error(), "rate limit exceeded") {
		t.Errorf("error = %q, want it to quote what the server said", err)
	}
}
