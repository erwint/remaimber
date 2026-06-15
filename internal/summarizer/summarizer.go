// Package summarizer builds short, recall-optimized summaries of conversation
// sessions via map-reduce: each window of messages is summarized independently
// (map), then the window summaries are consolidated into one, anchored on the
// session's opening goal (reduce). The backend is pluggable via environment
// variables: the local `claude` CLI (default, uses existing auth) or any
// OpenAI-compatible chat-completions endpoint (e.g. Ollama).
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
	"regexp"
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

// Summaries are built map-reduce rather than by a left fold: each window is
// summarized independently (map), then all window summaries are consolidated
// into one (reduce), anchored on the session's opening goal. This avoids the
// recency bias of folding, where late windows dominate and early work is lost.

const mapSystemPrompt = `Summarize this excerpt of a coding session in 1-2 plain sentences: ` +
	`what was being worked on, the concrete actions and decisions, and especially any user-facing commands, ` +
	`features, or workflows introduced. Name specific commands, flags, files, functions, libraries, or errors. ` +
	`Describe the work directly — never name an actor (no "the user", "the developer", "the assistant"). ` +
	`Omit incidental identifiers (commit hashes, internal task or run IDs, temp paths) and transient status. ` +
	`No preamble, no markdown. Output only the summary.`

// reduceSentenceRange scales the final summary length to the session's scope:
// a long, multi-feature session must not be crushed into the same 2-3 sentences
// as a short one, or headline features get dropped no matter how they're ranked.
// Bounded at the top so the summary stays skimmable rather than approaching the
// original transcript.
func reduceSentenceRange(numPartials int) (lo, hi int) {
	switch {
	case numPartials <= 4:
		return 2, 3
	case numPartials <= 10:
		return 3, 5
	case numPartials <= 25:
		return 4, 7
	default:
		return 6, 8
	}
}

// reducePrompt is the final-consolidation prompt with a scope-scaled length.
func reducePrompt(loSentences, hiSentences int) string {
	return fmt.Sprintf(`You are consolidating partial summaries of a single Claude Code coding session `+
		`into ONE recall-optimized summary that lets someone later find and resume this exact session.

You are given the session's opening goal and its partial summaries in chronological order. `+
		`Produce one cohesive summary of the WHOLE session, giving early and late phases EQUAL weight — do not `+
		`over-emphasize the end, and do not drop earlier phases. Cover every distinct feature or workflow; `+
		`a longer session warrants a longer summary.

Prioritize, in this order:
1. User-facing outcomes — the commands, features, and workflows delivered, and what someone can now DO. `+
		`Name them concretely (slash commands, CLI subcommands, the actual user workflow).
2. Key decisions and the core concepts or identifiers involved.
3. Notable implementation details — mention briefly; do NOT lead with or dwell on bare file names.
Then give the final state and anything left to do.

Describe the work directly — never name an actor (no "the user", "the developer", "the assistant", "the AI"). `+
		`Omit incidental artifacts: commit hashes, internal task/run/batch IDs, temp paths, and transient status `+
		`like "currently processing". Write %d-%d sentences as flowing prose, no preamble, no markdown, `+
		`no bullet points. Output only the summary text.`, loSentences, hiSentences)
}

// mergeSystemPrompt is used for intermediate batches when a session has too many
// partials for one reduce call. It preserves detail (no tight length cap) so the
// final reduce still has every distinct point to work from.
const mergeSystemPrompt = `Merge these partial summaries of a coding session into one thorough intermediate ` +
	`summary that preserves every distinct feature, command, decision, file, and technology mentioned. ` +
	`This will be consolidated again later, so favor completeness over brevity; do not compress aggressively. ` +
	`Describe the work directly — never name an actor. Omit incidental identifiers (commit hashes, internal ` +
	`task/run IDs, temp paths). No preamble, no markdown. Output only the summary.`

// MapWindow summarizes a single window of messages independently (the map step).
func (c Config) MapWindow(ctx context.Context, window []types.Message) (string, error) {
	return c.complete(ctx, mapSystemPrompt, renderWindow(window))
}

// maxReduceInputs bounds how many partial summaries go into one reduce call, so
// a very long session stays within model context. Excess is reduced
// hierarchically (reduce batches, then reduce the batch results).
const maxReduceInputs = 40

// ReduceSummaries consolidates chronological partial summaries into one final
// summary anchored on goal, with length scaled to the session's scope. Empty
// input yields an empty summary.
func (c Config) ReduceSummaries(ctx context.Context, goal string, partials []string) (string, error) {
	lo, hi := reduceSentenceRange(len(partials))
	return c.reduceWithTarget(ctx, goal, partials, lo, hi)
}

// reduceWithTarget keeps the original (scope-based) length target across the
// hierarchical merge, so a huge session's final summary is sized to the whole
// session — not to the small set of intermediate merges feeding the last pass.
func (c Config) reduceWithTarget(ctx context.Context, goal string, partials []string, lo, hi int) (string, error) {
	switch {
	case len(partials) == 0:
		return "", nil
	case len(partials) <= maxReduceInputs:
		return c.complete(ctx, reducePrompt(lo, hi), renderReduce(goal, partials))
	}
	var mids []string
	for i := 0; i < len(partials); i += maxReduceInputs {
		end := i + maxReduceInputs
		if end > len(partials) {
			end = len(partials)
		}
		m, err := c.complete(ctx, mergeSystemPrompt, renderReduce(goal, partials[i:end]))
		if err != nil {
			return "", err
		}
		mids = append(mids, m)
	}
	return c.reduceWithTarget(ctx, goal, mids, lo, hi)
}

func renderWindow(window []types.Message) string {
	var b strings.Builder
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

func renderReduce(goal string, partials []string) string {
	var b strings.Builder
	b.WriteString("Opening goal:\n")
	if strings.TrimSpace(goal) == "" {
		b.WriteString("(unknown)\n")
	} else {
		b.WriteString(strings.TrimSpace(goal) + "\n")
	}
	b.WriteString("\nPartial summaries (chronological):\n")
	for i, p := range partials {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(p))
	}
	return b.String()
}

var (
	reCommit     = regexp.MustCompile(`(?i)\(?\bcommits?\b[:\s]+[0-9a-f]{7,40}\)?`)
	reParenLabel = regexp.MustCompile(`(?i)\s*\((?:run|batch|task|id|session)\b[^)]*\)`)
	reParenID    = regexp.MustCompile(`\s*\([a-z]{1,3}[0-9][a-z0-9]{6,}\)`)
	reMultiSpace = regexp.MustCompile(`\s{2,}`)
)

// StripEphemeral removes incidental identifiers a model may carry over from tool
// output — commit hashes, internal task/run IDs — that are noise for recall and
// often stale. The reduce prompt asks the model to omit these; this is a safety
// net for the obvious patterns. Conservative by design to avoid eating prose.
func StripEphemeral(s string) string {
	s = reCommit.ReplaceAllString(s, "")
	s = reParenLabel.ReplaceAllString(s, "")
	s = reParenID.ReplaceAllString(s, "")
	s = reMultiSpace.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, " .", ".")
	s = strings.ReplaceAll(s, " ,", ",")
	return strings.TrimSpace(s)
}

// complete dispatches one (system, user) prompt to the configured backend.
func (c Config) complete(ctx context.Context, system, user string) (string, error) {
	if c.IsHTTP() {
		return c.completeHTTP(ctx, system, user)
	}
	return c.completeClaude(ctx, system, user)
}

// completeClaude shells out to `claude -p`, passing the prompt on stdin so it
// is not subject to argv length limits.
func (c Config) completeClaude(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	args := []string{"-p", "--append-system-prompt", system}
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

// completeHTTP calls an OpenAI-compatible /chat/completions endpoint.
func (c Config) completeHTTP(ctx context.Context, system, user string) (string, error) {
	if c.Model == "" {
		return "", fmt.Errorf("REMAIMBER_LLM_MODEL is required for the HTTP backend")
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	reqBody, _ := json.Marshal(map[string]any{
		"model":  c.Model,
		"stream": false,
		"messages": []map[string]string{
			{"role": "system", "content": system},
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
