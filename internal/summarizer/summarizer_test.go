package summarizer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erwin/remaimber/internal/types"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("REMAIMBER_LLM", "")
	t.Setenv("REMAIMBER_LLM_MODEL", "")
	c := LoadConfig()
	if c.Backend != "claude" {
		t.Errorf("default backend = %q, want claude", c.Backend)
	}
	if c.Model != "haiku" {
		t.Errorf("default model = %q, want haiku", c.Model)
	}
	if c.IsHTTP() {
		t.Error("claude backend should not be HTTP")
	}
}

func TestWindowAndTimeoutConfig(t *testing.T) {
	t.Setenv("REMAIMBER_LLM", "")
	t.Setenv("REMAIMBER_LLM_MODEL", "")
	t.Setenv("REMAIMBER_LLM_WINDOW", "")
	t.Setenv("REMAIMBER_LLM_TIMEOUT", "")
	if got := LoadConfig().WindowSize(); got != DefaultWindow {
		t.Errorf("default window = %d, want %d", got, DefaultWindow)
	}

	t.Setenv("REMAIMBER_LLM_WINDOW", "75")
	t.Setenv("REMAIMBER_LLM_TIMEOUT", "120")
	c := LoadConfig()
	if c.WindowSize() != 75 {
		t.Errorf("window override = %d, want 75", c.WindowSize())
	}
	if c.timeout() != 120*time.Second {
		t.Errorf("timeout override = %v, want 120s", c.timeout())
	}

	// Invalid values fall back to defaults.
	t.Setenv("REMAIMBER_LLM_WINDOW", "-5")
	if LoadConfig().WindowSize() != DefaultWindow {
		t.Error("invalid window should fall back to default")
	}
}

func TestIsHTTP(t *testing.T) {
	if !(Config{Backend: "http://localhost:11434/v1"}).IsHTTP() {
		t.Error("http url should be HTTP backend")
	}
	if (Config{Backend: "claude"}).IsHTTP() {
		t.Error("claude should not be HTTP backend")
	}
}

func TestRenderWindow(t *testing.T) {
	out := renderWindow([]types.Message{
		{Role: "user", ContentText: "add a flag"},
		{Role: "assistant", ContentText: "done"},
		{Role: "assistant", ContentText: ""}, // skipped
	})
	if !strings.Contains(out, "[user] add a flag") || !strings.Contains(out, "[assistant] done") {
		t.Errorf("window missing rendered messages:\n%s", out)
	}
	if strings.Contains(out, "[assistant] \n") {
		t.Error("empty message should be skipped")
	}
}

func TestRenderReducePinsGoal(t *testing.T) {
	out := renderReduce("add cross-worktree resume", []string{"first part", "second part"})
	if !strings.Contains(out, "Opening goal:\nadd cross-worktree resume") {
		t.Errorf("goal not pinned:\n%s", out)
	}
	if !strings.Contains(out, "1. first part") || !strings.Contains(out, "2. second part") {
		t.Errorf("partials not enumerated:\n%s", out)
	}

	out = renderReduce("", []string{"only part"})
	if !strings.Contains(out, "(unknown)") {
		t.Error("missing goal should render as (unknown)")
	}
}

func TestStripEphemeral(t *testing.T) {
	cases := map[string]string{
		"Implemented two filters (Commit 113148a) to cut folds.": "Implemented two filters to cut folds.",
		"A batch run (b9f4s5zmx) processed the records.":         "A batch run processed the records.",
		"Reduced folds (run abc-123) for the giant session.":     "Reduced folds for the giant session.",
		"Plain prose with no identifiers stays intact.":          "Plain prose with no identifiers stays intact.",
	}
	for in, want := range cases {
		if got := StripEphemeral(in); got != want {
			t.Errorf("StripEphemeral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapWindowHTTP(t *testing.T) {
	var sawSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		json.Unmarshal(body, &req)
		if len(req.Messages) == 2 {
			sawSystem = req.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"  window summary  "}}]}`)
	}))
	defer srv.Close()

	c := Config{Backend: srv.URL, Model: "m"}
	got, err := c.MapWindow(context.Background(), []types.Message{{Role: "user", ContentText: "do x"}})
	if err != nil {
		t.Fatalf("MapWindow: %v", err)
	}
	if got != "window summary" {
		t.Errorf("map result = %q, want trimmed 'window summary'", got)
	}
	if !strings.Contains(sawSystem, "excerpt") {
		t.Errorf("map system prompt not sent: %q", sawSystem)
	}
}

func TestReduceSentenceRangeScales(t *testing.T) {
	// Longer sessions get a larger budget; bounded at the top.
	lo4, hi4 := reduceSentenceRange(4)
	lo16, hi16 := reduceSentenceRange(16)
	_, hi100 := reduceSentenceRange(100)
	if !(hi4 < hi16 && hi16 <= hi100) {
		t.Errorf("budget should grow with scope: hi4=%d hi16=%d hi100=%d", hi4, hi16, hi100)
	}
	if lo4 < 1 || hi100 > 10 {
		t.Errorf("range out of sane bounds: %d..%d", lo4, hi100)
	}
	if lo16 > hi16 {
		t.Errorf("lo must not exceed hi: %d..%d", lo16, hi16)
	}
}

func TestReducePromptIncludesRange(t *testing.T) {
	p := reducePrompt(5, 8)
	if !strings.Contains(p, "5-8 sentences") {
		t.Errorf("reduce prompt missing scaled range:\n%s", p)
	}
}

func TestReduceSummariesBatches(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"consolidated"}}]}`)
	}))
	defer srv.Close()
	c := Config{Backend: srv.URL, Model: "m"}

	// Few partials -> single reduce call.
	if _, err := c.ReduceSummaries(context.Background(), "goal", []string{"a", "b", "c"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("small reduce = %d calls, want 1", calls)
	}

	// More than maxReduceInputs -> hierarchical (batch reduces + final reduce).
	calls = 0
	many := make([]string, maxReduceInputs+5)
	for i := range many {
		many[i] = "p"
	}
	if _, err := c.ReduceSummaries(context.Background(), "goal", many); err != nil {
		t.Fatal(err)
	}
	if calls < 3 { // 2 batch reduces + 1 final
		t.Errorf("hierarchical reduce = %d calls, want >= 3", calls)
	}
}

func TestCompleteHTTPRequiresModel(t *testing.T) {
	c := Config{Backend: "http://localhost:9/v1"}
	if _, err := c.MapWindow(context.Background(), nil); err == nil {
		t.Error("expected error when model is missing for HTTP backend")
	}
}

func TestCompleteHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := Config{Backend: srv.URL, Model: "m"}
	if _, err := c.MapWindow(context.Background(), nil); err == nil {
		t.Error("expected error on non-200 status")
	}
}
