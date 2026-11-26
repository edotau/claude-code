package jev

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/proc"
)

const (
	qwenURL   = "http://127.0.0.1:10100/inference/v1/chat/completions"
	qwenEnv   = "HARNESS_JEV_QWEN_URL"
	qwenModel = "qwen3.8-27b:gguf:q6"
	Confident = 0.85 // Open-Jev's own pick stands at or above this; below it the helper is asked
)

// Helper asks the local Qwen (thinking off) for a tier on a brief Open-Jev was unsure of; only "fast" is acted on.
func Helper(ctx context.Context, brief string) (string, error) {
	body, err := post(ctx, strings.TrimSpace(proc.Value(ctx, qwenEnv)), qwenURL, "", map[string]any{
		"model": qwenModel, "reasoning_effort": "none", "max_tokens": 20,
		"response_format": map[string]string{"type": "json_object"},
		"messages":        []map[string]string{{"role": "system", "content": helperPrompt()}, {"role": "user", "content": clip(brief)}},
	})
	if err != nil {
		return "", err
	}
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
	b.WriteString("You route coding-agent tasks to a model tier. " + q.Instructions + "\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "- %s: %s\n", k, q.Criteria[k])
	}
	b.WriteString(`Answer only JSON: {"tier":"fast|standard|deep"}`)
	return b.String()
}
