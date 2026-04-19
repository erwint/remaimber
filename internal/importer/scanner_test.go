package importer

import "testing"

func TestProjectPathFromKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"-Volumes-Data-src-foo", "/Volumes/Data/src/foo"},
		{"-Users-erwin-projects-bar", "/Users/erwin/projects/bar"},
		{"", ""},
	}
	for _, tt := range tests {
		got := ProjectPathFromKey(tt.key)
		if got != tt.want {
			t.Errorf("ProjectPathFromKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

func TestPrettyProjectName(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"-Volumes-Data-src-owner-repo", "owner/repo"},
		{"-Users-erwin-projects-myapp", "projects/myapp"},
		{"-a-b", "a/b"},
		{"", ""},
	}
	for _, tt := range tests {
		got := PrettyProjectName(tt.key)
		if got != tt.want {
			t.Errorf("PrettyProjectName(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
