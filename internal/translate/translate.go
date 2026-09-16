// Package translate converts between the Anthropic Messages wire and OpenAI chat-completions.
//
// Choices: the output cap is sent as max_tokens except for OpenAI reasoning models (o-series, gpt-5),
// which only accept max_completion_tokens; upstream reasoning text (reasoning_content / reasoning) is
// dropped because Claude Code requires a signature on thinking blocks that a foreign model cannot mint.
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// newID returns prefix plus a random suffix; tests replace it for stable goldens.
var newID = func(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// block is an Anthropic content block; only the fields the translation reads are decoded.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Source    *source         `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type source struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// parseBlocks accepts content as a plain string or a block array.
func parseBlocks(raw json.RawMessage) ([]block, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []block{{Type: "text", Text: s}}, nil
	}
	var bs []block
	err := json.Unmarshal(raw, &bs)
	return bs, err
}

// chatUsage is the chat-completions usage object.
type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// anthropicUsage maps chat usage; Anthropic's input_tokens excludes cache reads.
func anthropicUsage(u *chatUsage) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u == nil {
		return out
	}
	in := u.PromptTokens
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens > 0 && d.CachedTokens <= in {
		in -= d.CachedTokens
		out["cache_read_input_tokens"] = d.CachedTokens
	}
	out["input_tokens"] = in
	out["output_tokens"] = u.CompletionTokens
	return out
}

// stopReason maps a finish_reason; providers such as Gemini report "stop" even when tools were called.
func stopReason(finish string, usedTools bool) string {
	switch {
	case finish == "length":
		return "max_tokens"
	case usedTools:
		return "tool_use"
	default:
		return "end_turn"
	}
}

// inToolCall is a response/stream tool call; arguments may arrive as a JSON string or a bare object.
type inToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

func argsText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	return s
}
