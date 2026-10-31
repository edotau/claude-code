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
	"fmt"
	"net/http"
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

// imageChars approximates an image's token cost (~1500 tokens) in characters.
const imageChars = 6000

// CountTokens answers a Messages count_tokens request with a local estimate (~4 characters per token).
func CountTokens(body []byte) ([]byte, error) {
	var in struct {
		System   json.RawMessage `json:"system"`
		Messages []inMessage     `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translate: count_tokens: %w", err)
	}
	chars := contentChars(in.System)
	for _, m := range in.Messages {
		chars += contentChars(m.Content)
	}
	if string(in.Tools) != "null" {
		chars += len(in.Tools)
	}
	return json.Marshal(map[string]int{"input_tokens": (chars + 3) / 4})
}

func contentChars(raw json.RawMessage) int {
	blocks, _ := parseBlocks(raw)
	n := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			n += len(b.Text)
		case "thinking":
			n += len(b.Thinking)
		case "image", "document":
			n += imageChars
		case "tool_use":
			n += len(b.Name) + len(b.Input)
		case "tool_result":
			n += contentChars(b.Content)
		}
	}
	return n
}

const maxErrorText = 4096

// FromChatError maps an upstream error status/body into an Anthropic error body.
func FromChatError(status int, body []byte) []byte {
	msg := upstreamMessage(body)
	if msg == "" {
		msg = http.StatusText(status)
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream error (status %d)", status)
	}
	out, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType(status), "message": msg},
	})
	return out
}

func errorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 413:
		return "request_too_large"
	case status == 429:
		return "rate_limit_error"
	case status == 503 || status == 529:
		return "overloaded_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// upstreamMessage extracts a message from OpenAI ({error:{message}}), Gemini ([{error:…}]),
// Ollama ({error:"…"}) and FastAPI ({detail}) shapes, falling back to the trimmed body text.
func upstreamMessage(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ""
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			v = arr[0]
		}
		if obj, ok := v.(map[string]any); ok {
			if e, ok := obj["error"].(map[string]any); ok {
				obj = e
			} else if s, ok := obj["error"].(string); ok && s != "" {
				return s
			}
			for _, k := range []string{"message", "detail"} {
				if s, ok := obj[k].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	if len(text) > maxErrorText {
		text = text[:maxErrorText]
	}
	return text
}

// streamErrorStatus reads a numeric error.code from an in-band stream error, defaulting to 500.
func streamErrorStatus(raw json.RawMessage) int {
	var e struct {
		Code any `json:"code"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if f, ok := e.Code.(float64); ok && f >= 400 && f < 600 {
			return int(f)
		}
	}
	return http.StatusInternalServerError
}
