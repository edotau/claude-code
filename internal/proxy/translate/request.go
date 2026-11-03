package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

type messagesRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`
	Messages      []inMessage     `json:"messages"`
	MaxTokens     int             `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Tools         []inTool        `json:"tools"`
	ToolChoice    *struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type inMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type inTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	TopP                *float64       `json:"top_p,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Stream              bool           `json:"stream,omitempty"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	Tools               []chatTool     `json:"tools,omitempty"`
	ToolChoice          any            `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool          `json:"parallel_tool_calls,omitempty"`
	User                string         `json:"user,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
	// ThoughtSignature satisfies Gemini's per-call validation on replayed tool calls.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// SkipThoughtSignature is Gemini's documented validator bypass for tool calls replayed without their signature.
const SkipThoughtSignature = "skip_thought_signature_validator"

// ChatToolChoice rewrites a tool_choice Databricks Gemini rejects (Anthropic object, Copilot's "validated") into the chat form.
func ChatToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		switch str {
		case "auto", "required", "none":
			return raw, false
		}
		return json.RawMessage(`"auto"`), true
	}
	var tc struct{ Type, Name string }
	if json.Unmarshal(raw, &tc) != nil {
		return raw, false
	}
	var out any
	switch tc.Type {
	case "auto", "none":
		out = tc.Type
	case "any":
		out = "required"
	case "tool":
		out = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
	default:
		return raw, false
	}
	enc, err := json.Marshal(out)
	return enc, err == nil
}

// FlattenChatContent joins a non-stream chat response's content-part arrays (Databricks Gemini) into strings.
func FlattenChatContent(body []byte) []byte {
	var top map[string]json.RawMessage
	var choices []map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || json.Unmarshal(top["choices"], &choices) != nil {
		return body
	}
	changed := false
	for _, c := range choices {
		var msg map[string]json.RawMessage
		var parts []struct{ Type, Text string }
		if json.Unmarshal(c["message"], &msg) != nil || json.Unmarshal(msg["content"], &parts) != nil || parts == nil {
			continue // a null (tool-call-only) or string content stays as sent
		}
		var sb strings.Builder
		for _, pt := range parts {
			if pt.Type == "text" {
				sb.WriteString(pt.Text)
			}
		}
		msg["content"], _ = json.Marshal(sb.String())
		c["message"], _ = json.Marshal(msg)
		changed = true
	}
	if !changed {
		return body
	}
	top["choices"], _ = json.Marshal(choices)
	if out, err := json.Marshal(top); err == nil {
		return out
	}
	return body
}

// IsGeminiModel reports whether a wire model is a Gemini model (case-insensitive).
func IsGeminiModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "gemini")
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string  `json:"type"`
	Function toolDef `json:"function"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToChatRequest converts an Anthropic Messages request body into a chat-completions body targeting wireModel.
func ToChatRequest(body []byte, wireModel string) (out []byte, stream bool, err error) {
	var in messagesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, false, fmt.Errorf("translate: request: %w", err)
	}
	req := chatRequest{Model: wireModel, Stop: in.StopSequences, Temperature: in.Temperature, TopP: in.TopP}
	maxTokens := in.MaxTokens
	if l, ok := models.Limits(wireModel); ok && l.Output > 0 { // an output cap above the model's limit is a 400
		maxTokens = min(maxTokens, l.Output)
	}
	if models.IsOpenAIReasoning(wireModel) { // reasoning models reject max_tokens, temperature and top_p
		req.MaxCompletionTokens, req.Temperature, req.TopP = maxTokens, nil, nil
	} else {
		req.MaxTokens = maxTokens
	}
	if in.Metadata != nil {
		req.User = in.Metadata.UserID
	}
	if in.Stream {
		req.Stream = true
		if !IsGeminiModel(wireModel) { // Gemini rejects stream_options but still reports usage
			req.StreamOptions = &streamOptions{IncludeUsage: true}
		}
	}

	sys, err := parseBlocks(in.System)
	if err != nil {
		return nil, false, fmt.Errorf("translate: system: %w", err)
	}
	if text := joinText(sys, "\n\n"); text != "" {
		req.Messages = append(req.Messages, chatMessage{Role: "system", Content: text})
	}
	for i, m := range in.Messages {
		blocks, err := parseBlocks(m.Content)
		if err != nil {
			return nil, false, fmt.Errorf("translate: messages[%d]: %w", i, err)
		}
		if m.Role == "assistant" {
			req.Messages = appendAssistant(req.Messages, blocks, wireModel)
		} else {
			if req.Messages, err = appendUser(req.Messages, blocks); err != nil {
				return nil, false, fmt.Errorf("translate: messages[%d]: %w", i, err)
			}
		}
	}

	req.Tools = chatTools(in.Tools, wireModel)
	applyToolChoice(&req, in)

	out, err = json.Marshal(req)
	return out, in.Stream, err
}

// chatTools converts tools to function tools for wireModel.
func chatTools(tools []inTool, wireModel string) []chatTool {
	var out []chatTool
	for _, t := range tools {
		if len(t.InputSchema) == 0 && t.Type != "" && t.Type != "custom" {
			continue // Anthropic server tools (web_search, …) have no function equivalent
		}
		params := t.InputSchema
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if IsGeminiModel(wireModel) {
			params, _ = GeminiSchema(params)
		}
		out = append(out, chatTool{Type: "function", Function: toolDef{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	return out
}

// applyToolChoice maps in's tool_choice onto req; it applies only when req has tools.
func applyToolChoice(req *chatRequest, in messagesRequest) {
	if tc := in.ToolChoice; tc != nil && len(req.Tools) > 0 {
		switch tc.Type {
		case "auto":
			req.ToolChoice = "auto"
		case "any":
			req.ToolChoice = "required"
		case "none":
			req.ToolChoice = "none"
		case "tool":
			req.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
		}
		if tc.DisableParallelToolUse {
			f := false
			req.ParallelToolCalls = &f
		}
	}
}

// appendUser emits tool results first (they must directly follow the assistant tool_calls), then the user turn.
func appendUser(msgs []chatMessage, blocks []block) ([]chatMessage, error) {
	var parts []chatPart
	hasImage := false
	addImage := func(s *source) {
		if u := imageDataURL(s); u != "" {
			parts = append(parts, chatPart{Type: "image_url", ImageURL: &imageURL{URL: u}})
			hasImage = true
		}
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, chatPart{Type: "text", Text: b.Text})
			}
		case "image":
			addImage(b.Source)
		case "document":
			if b.Source != nil && b.Source.Type == "text" && b.Source.Data != "" {
				parts = append(parts, chatPart{Type: "text", Text: b.Source.Data})
			}
		case "tool_result":
			inner, err := parseBlocks(b.Content)
			if err != nil {
				return nil, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			text := joinText(inner, "\n")
			for _, ib := range inner {
				if ib.Type == "image" {
					addImage(ib.Source) // tool messages are text-only, so images ride the following user turn
				}
			}
			msgs = append(msgs, chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: text})
		}
	}
	if len(parts) == 0 {
		return msgs, nil
	}
	if !hasImage {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p.Text
		}
		return append(msgs, chatMessage{Role: "user", Content: strings.Join(texts, "\n\n")}), nil
	}
	return append(msgs, chatMessage{Role: "user", Content: parts}), nil
}

func appendAssistant(msgs []chatMessage, blocks []block, wireModel string) []chatMessage {
	var calls []chatToolCall
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		args := "{}"
		var buf bytes.Buffer
		if len(b.Input) > 0 && string(b.Input) != "null" && json.Compact(&buf, b.Input) == nil {
			args = buf.String()
		}
		call := chatToolCall{ID: b.ID, Type: "function", Function: chatFunction{Name: b.Name, Arguments: args}}
		if IsGeminiModel(wireModel) {
			call.ThoughtSignature = SkipThoughtSignature
		}
		calls = append(calls, call)
	}
	text := joinText(blocks, "\n\n")
	if text == "" && len(calls) == 0 {
		return msgs
	}
	m := chatMessage{Role: "assistant", ToolCalls: calls}
	if text != "" {
		m.Content = text
	}
	return append(msgs, m)
}

// joinText concatenates the text blocks, skipping thinking and every non-text block.
func joinText(blocks []block, sep string) string {
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, sep)
}

func imageDataURL(s *source) string {
	if s == nil {
		return ""
	}
	switch s.Type {
	case "base64":
		return "data:" + s.MediaType + ";base64," + s.Data
	case "url":
		return s.URL
	}
	return ""
}

// geminiSchemaKeys are the JSON-schema keys Google's function declarations accept; others 400 with "Unknown name".
var geminiSchemaKeys = map[string]bool{"type": true, "format": true, "description": true, "nullable": true, "enum": true,
	"items": true, "properties": true, "required": true, "minItems": true, "maxItems": true, "minimum": true, "maximum": true,
	"minLength": true, "maxLength": true, "pattern": true, "title": true, "default": true, "anyOf": true,
	"additionalProperties": true, "example": true, "propertyOrdering": true, "minProperties": true, "maxProperties": true}

// GeminiSchema strips keys Google rejects from a tool parameter schema (const becomes enum, oneOf anyOf).
// It returns the input untouched, with false, when nothing needed to change.
func GeminiSchema(raw json.RawMessage) (json.RawMessage, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw, false
	}
	changed := cleanSchema(v)
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(v)
	return out, err == nil
}

// cleanSchema edits one schema node and its sub-schemas in place, reporting whether anything changed.
func cleanSchema(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if c, ok := m["const"]; ok {
		m["enum"] = []any{c}
		changed = true
	}
	if c, ok := m["oneOf"]; ok {
		if _, has := m["anyOf"]; !has {
			m["anyOf"] = c
		}
		changed = true
	}
	for k, sub := range m {
		if !geminiSchemaKeys[k] {
			delete(m, k)
			changed = true
			continue
		}
		switch k {
		case "properties":
			props, _ := sub.(map[string]any)
			for _, p := range props {
				changed = cleanSchema(p) || changed
			}
		case "items", "additionalProperties":
			changed = cleanSchema(sub) || changed
		case "anyOf":
			subs, _ := sub.([]any)
			for _, s := range subs {
				changed = cleanSchema(s) || changed
			}
		}
	}
	return changed
}
