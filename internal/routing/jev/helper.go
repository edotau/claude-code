package jev

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/platform/proc"
)

const (
	qwenEnv   = "HARNESS_JEV_QWEN_URL"
	Confident = 0.85 // Open-Jev's own pick stands at or above this; below it the helper is asked
)

// Helper asks the local model (thinking off) at OpenAI base URL base for a tier on a brief Open-Jev was unsure of.
func Helper(ctx context.Context, base, model, brief string) (string, error) {
	url := strings.TrimRight(base, "/") + "/chat/completions"
	override := strings.TrimSpace(proc.Value(ctx, qwenEnv))
	if override == "off" || override == "" && testing.Testing() {
		return "", ErrOff
	}
	target := cmp.Or(override, url)
	if backedOff(target) {
		return "", ErrDown
	}
	start := time.Now()
	body, err := post(ctx, timeout, override, url, "", map[string]any{
		"model": model, "reasoning_effort": "none", "max_tokens": 64,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []map[string]string{{"role": "system", "content": helperPrompt()}, {"role": "user", "content": clip(brief)}},
	})
	if ctx.Err() == nil {
		noteOutcome(target, err, time.Since(start))
	}
	if err != nil {
		return "", err
	}
	return parseHelper(body)
}

func parseHelper(body []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	var got struct {
		Tier string `json:"tier"`
	}
	if json.Unmarshal(body, &out) != nil || len(out.Choices) == 0 || json.Unmarshal([]byte(out.Choices[0].Message.Content), &got) != nil {
		return "", ErrBadResponse
	}
	switch got.Tier {
	case "fast", "standard", "deep":
		return got.Tier, nil
	}
	return "", ErrBadResponse
}

func helperPrompt() string {
	var q struct {
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria"`
	}
	_ = json.Unmarshal(assets.JevTier(), &q)
	keys := make([]string, 0, len(q.Criteria))
	for k := range q.Criteria {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "You route coding-agent tasks to a model tier. %s\n", q.Instructions)
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, q.Criteria[k])
	}
	b.WriteString("Boundary rules and frontier-jury exemplars:\n" +
		"- fast: mechanical single-step work or predetermined edits with exact text given.\n" +
		"  Example: \"Edit only these two files with the exact text below; do not commit.\" -> " +
		`{"reason":"Predetermined edits in named files with exact text.","tier":"fast"}` + "\n" +
		"- standard: routine engineering in a known scope — implement/fix in 1-3 files, bounded worktree/lane diff review " +
		"(\"Open only the diff's files and direct callers\", \"HARD CAP 15 minutes\", or \"Review only, no edits\" on a named " +
		"branch/worktree, EVEN IF the diff touches redaction, HTTP, or auth), memory bank consolidation/update, or resuming an interrupted task.\n" +
		"  Example: \"HARD CAP 15 minutes. Open only the diff's files and direct callers. Review redact-model-label / http-hardening worktree diff.\" -> " +
		`{"reason":"Bounded worktree diff review of named files and direct callers.","tier":"standard"}` + "\n" +
		"  Example: \"Background update/consolidation of the memory bank files.\" -> " +
		`{"reason":"Routine memory bank update following structured rules.","tier":"standard"}` + "\n" +
		"- deep: ONLY when the scope is open-ended or the root cause is unknown (investigating a mystery flaky test/race/deadlock " +
		"from scratch, multi-package architecture design, or an unbounded security/concurrency audit).\n" +
		`Write reason BEFORE tier. Answer only JSON: {"reason":"<=12 words","tier":"fast|standard|deep"}`)
	return b.String()
}
