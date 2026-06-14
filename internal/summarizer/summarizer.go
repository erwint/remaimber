// Package summarizer maintains short rolling summaries of conversation sessions
// by folding successive message windows through an LLM. The backend is pluggable
// via environment variables: the local `claude` CLI (default, uses existing
// auth) or any OpenAI-compatible chat-completions endpoint (e.g. Ollama).
package summarizer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/erwin/remaimber/internal/types"
)

// defaultTimeout bounds a single summarization call. It's generous because a
// large local model on a full window can be slow (tens of seconds per call).
const defaultTimeout = 300 * time.Second

// DefaultWindow is the number of user/assistant messages folded per LLM call.
// Larger windows mean far fewer calls (cheaper backfill) and more context per
// summary; modern models have ample context to absorb it.
const DefaultWindow = 40

// Config selects and parameterizes the summarization backend.
type Config struct {
	// Backend is "claude" (shell out to the claude CLI) or an OpenAI-compatible
	// base URL beginning with http:// or https://.
	Backend string
	Model   string
	APIKey  string        // optional bearer token for the HTTP backend
	Timeout time.Duration // per-call timeout
	Window  int           // user/assistant messages folded per call
}

// LoadConfig reads configuration from the environment:
//
//	REMAIMBER_LLM           "claude" (default) or an OpenAI-compatible base URL
//	REMAIMBER_LLM_MODEL     model name (default "haiku" for the claude backend)
//	REMAIMBER_LLM_KEY       optional bearer token for the HTTP backend
//	REMAIMBER_LLM_TIMEOUT   per-call timeout in seconds (default 300)
//	REMAIMBER_LLM_WINDOW    messages folded per call (default 40)
func LoadConfig() Config {
	c := Config{
		Backend: os.Getenv("REMAIMBER_LLM"),
		Model:   os.Getenv("REMAIMBER_LLM_MODEL"),
		APIKey:  os.Getenv("REMAIMBER_LLM_KEY"),
		Timeout: defaultTimeout,
		Window:  DefaultWindow,
	}
	if c.Backend == "" {
		c.Backend = "claude"
	}
	if c.Model == "" && c.Backend == "claude" {
		c.Model = "haiku"
	}
	if s := os.Getenv("REMAIMBER_LLM_TIMEOUT"); s != "" {
		if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
			c.Timeout = time.Duration(secs) * time.Second
		}
	}
	if s := os.Getenv("REMAIMBER_LLM_WINDOW"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			c.Window = n
		}
	}
	return c
}

// timeout returns the configured per-call timeout, or the default if unset.
func (c Config) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// WindowSize returns the configured fold window, or the default if unset.
func (c Config) WindowSize() int {
	if c.Window > 0 {
		return c.Window
	}
	return DefaultWindow
}

// IsHTTP reports whether the backend is an OpenAI-compatible HTTP endpoint.
func (c Config) IsHTTP() bool {
	return strings.HasPrefix(c.Backend, "http://") || strings.HasPrefix(c.Backend, "https://")
}

const systemPrompt = `You maintain a running, recall-optimized summary of a Claude Code coding session. ` +
	`The summary exists so that someone can later FIND and resume this exact session by skimming or searching it, ` +
	`so it must be specific and keyword-rich.

You receive the summary so far and the next batch of messages (oldest to newest). ` +
	`Produce ONE updated summary that integrates them: keep still-relevant facts from the prior summary, ` +
	`fold in what is new, and refresh the "current state". Do not describe only the latest messages, ` +
	`and do not drop earlier work just because newer messages arrived.

Capture concretely:
- the main goal or topic of the session
- specific things built, changed, decided, or debugged — name real files, functions, commands, flags, libraries, and error messages
- the current state and anything left unfinished or planned next

Write 2-4 plain sentences (at most ~80 words). Be concrete and searchable. ` +
	`Use neutral third person (no "the user"), no preamble, no markdown, no bullet points. ` +
	`Output ONLY the summary text.`

// Summarize folds a window of new messages into the previous summary, returning
// the updated summary. prev may be empty for the first window.
func (c Config) Summarize(ctx context.Context, prev string, window []types.Message) (string, error) {
	user := renderPrompt(prev, window)
	if c.IsHTTP() {
		return c.summarizeHTTP(ctx, user)
	}
	return c.summarizeClaude(ctx, user)
}

func renderPrompt(prev string, window []types.Message) string {
	var b strings.Builder
	b.WriteString("Summary so far:\n")
	if prev == "" {
		b.WriteString("(none yet — this is the start of the session)\n")
	} else {
		b.WriteString(prev + "\n")
	}
	b.WriteString("\nNew messages (oldest to newest):\n")
	for _, m := range window {
		text := strings.TrimSpace(m.ContentText)
		if text == "" {
			continue
		}
		if len(text) > 1200 {
			text = text[:1200] + "…"
		}
		role := m.Role
		if role == "" {
			role = m.Type
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, text)
	}
	return b.String()
}

// summarizeClaude shells out to `claude -p`, passing the prompt on stdin so it
// is not subject to argv length limits.
func (c Config) summarizeClaude(ctx context.Context, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	args := []string{"-p", "--append-system-prompt", systemPrompt}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(user)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude summarize: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// summarizeHTTP calls an OpenAI-compatible /chat/completions endpoint.
func (c Config) summarizeHTTP(ctx context.Context, user string) (string, error) {
	if c.Model == "" {
		return "", fmt.Errorf("REMAIMBER_LLM_MODEL is required for the HTTP backend")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	reqBody, _ := json.Marshal(map[string]any{
		"model":  c.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": user},
		},
	})

	url := strings.TrimRight(c.Backend, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM endpoint %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode LLM response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
