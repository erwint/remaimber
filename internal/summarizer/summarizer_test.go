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

func TestRenderPromptIncludesPrevAndMessages(t *testing.T) {
	out := renderPrompt("earlier work", []types.Message{
		{Role: "user", ContentText: "add a flag"},
		{Role: "assistant", ContentText: "done"},
		{Role: "assistant", ContentText: ""}, // skipped
	})
	if !strings.Contains(out, "earlier work") {
		t.Error("prompt should include previous summary")
	}
	if !strings.Contains(out, "[user] add a flag") || !strings.Contains(out, "[assistant] done") {
		t.Errorf("prompt missing rendered messages:\n%s", out)
	}
}

func TestRenderPromptNoPrev(t *testing.T) {
	out := renderPrompt("", []types.Message{{Role: "user", ContentText: "hi"}})
	if !strings.Contains(out, "none yet") {
		t.Error("empty previous summary should be marked as none yet / start of session")
	}
}

func TestSummarizeHTTP(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.Unmarshal(body, &req)
		gotModel = req.Model
		if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"  rolling summary  "}}]}`)
	}))
	defer srv.Close()

	c := Config{Backend: srv.URL, Model: "llama3.2"}
	got, err := c.Summarize(context.Background(), "prev", []types.Message{{Role: "user", ContentText: "do x"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if got != "rolling summary" {
		t.Errorf("summary = %q, want trimmed 'rolling summary'", got)
	}
	if gotModel != "llama3.2" {
		t.Errorf("model = %q, want llama3.2", gotModel)
	}
}

func TestSummarizeHTTPRequiresModel(t *testing.T) {
	c := Config{Backend: "http://localhost:9/v1"}
	if _, err := c.Summarize(context.Background(), "", nil); err == nil {
		t.Error("expected error when model is missing for HTTP backend")
	}
}

func TestSummarizeHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := Config{Backend: srv.URL, Model: "m"}
	if _, err := c.Summarize(context.Background(), "", nil); err == nil {
		t.Error("expected error on non-200 status")
	}
}
