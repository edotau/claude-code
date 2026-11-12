package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/models"
)

type messagesRequest struct {
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

// ChatToolChoice rewrites a tool_choice Gemini rejects (an Anthropic object, Copilot's "validated") into chat form.
func ChatToolChoice(raw json.RawMessage) (json.RawMessage, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw, false
	}
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
	out, ok := chatToolChoice(tc.Type, tc.Name)
	if !ok {
		return raw, false
	}
	enc, err := json.Marshal(out)
	return enc, err == nil
}

// chatToolChoice maps an Anthropic tool_choice type (and tool name) to its chat-completions value.
func chatToolChoice(typ, name string) (any, bool) {
	switch typ {
	case "auto", "none":
		return typ, true
	case "any":
		return "required", true
	case "tool":
		return map[string]any{"type": "function", "function": map[string]string{"name": name}}, true
	}
	return nil, false
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
		if flattenChoice(c) {
			changed = true
		}
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

// flattenChoice joins one choice's content-part array into a string in place, reporting whether it did.
func flattenChoice(c map[string]json.RawMessage) bool {
	var msg map[string]json.RawMessage
	if json.Unmarshal(c["message"], &msg) != nil || len(msg["content"]) == 0 || msg["content"][0] != '[' {
		return false // a null (tool-call-only) or string content stays as sent
	}
	text, err := contentText(msg["content"])
	if err != nil {
		return false
	}
	msg["content"], _ = json.Marshal(text)
	c["message"], _ = json.Marshal(msg)
	return true
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
	req := chatRequestParams(in, wireModel)
	if req.Messages, err = chatMessages(in, wireModel); err != nil {
		return nil, false, err
	}
	req.Tools = chatTools(in.Tools, wireModel)
	if err := applyToolChoice(&req, in); err != nil {
		return nil, false, err
	}

	out, err = json.Marshal(req)
	return out, in.Stream, err
}

// chatRequestParams builds the chat request's model, sampling, token-cap and stream fields from in.
func chatRequestParams(in messagesRequest, wireModel string) chatRequest {
	req := chatRequest{Model: wireModel, Stop: in.StopSequences, Temperature: in.Temperature, TopP: in.TopP}
	maxTokens := in.MaxTokens
	if l, ok := models.Limits(wireModel); ok && l.Output > 0 { // an output cap above the model's limit is a 400
		maxTokens = min(maxTokens, l.Output)
	}
	if models.IsOpenAIReasoning(wireModel) { // reasoning models reject max_tokens, temperature, top_p and stop
		req.MaxCompletionTokens, req.Temperature, req.TopP, req.Stop = maxTokens, nil, nil, nil
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
	return req
}

// chatMessages converts the system prompt and conversation turns of in into chat messages.
func chatMessages(in messagesRequest, wireModel string) ([]chatMessage, error) {
	var msgs []chatMessage
	sys, err := parseBlocks(in.System)
	if err != nil {
		return nil, fmt.Errorf("translate: system: %w", err)
	}
	if text := joinText(sys, "\n\n"); text != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: text})
	}
	for i, m := range in.Messages {
		blocks, err := parseBlocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("translate: messages[%d]: %w", i, err)
		}
		if m.Role == "assistant" {
			msgs = appendAssistant(msgs, blocks, wireModel)
			continue
		}
		if msgs, err = appendUser(msgs, blocks); err != nil {
			return nil, fmt.Errorf("translate: messages[%d]: %w", i, err)
		}
	}
	return msgs, nil
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
		out = append(out,
			chatTool{Type: "function", Function: toolDef{Name: t.Name, Description: t.Description, Parameters: params}})
	}
	return out
}

// applyToolChoice maps in's tool_choice onto req when req has tools; naming a tool req lacks is an error.
func applyToolChoice(req *chatRequest, in messagesRequest) error {
	tc := in.ToolChoice
	if tc == nil {
		return nil
	}
	if tc.Type == "tool" && !slices.ContainsFunc(req.Tools, func(t chatTool) bool { return t.Function.Name == tc.Name }) {
		return fmt.Errorf("translate: tool_choice names %q, which has no chat-completions function (a server tool?)",
			tc.Name)
	}
	if len(req.Tools) == 0 {
		return nil
	}
	if v, ok := chatToolChoice(tc.Type, tc.Name); ok {
		req.ToolChoice = v
	}
	if tc.DisableParallelToolUse {
		f := false
		req.ParallelToolCalls = &f
	}
	return nil
}

// userTurn accumulates the content parts of one user turn.
type userTurn struct {
	parts    []chatPart
	hasImage bool
}

func (u *userTurn) addImage(s *source) {
	if url := imageDataURL(s); url != "" {
		u.parts = append(u.parts, chatPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
		u.hasImage = true
	}
}

func (u *userTurn) addText(text string) {
	if text != "" {
		u.parts = append(u.parts, chatPart{Type: "text", Text: text})
	}
}

// add folds one non-tool-result block into the turn; a document chat completions cannot carry is an error.
func (u *userTurn) add(b block) error {
	switch b.Type {
	case "text":
		u.addText(b.Text)
	case "image":
		u.addImage(b.Source)
	case "document":
		text, err := documentText(b)
		if err != nil {
			return err
		}
		u.addText(text)
	}
	return nil
}

// documentText is a text document's data; a PDF or URL source has no chat-completions form, so it is an error.
func documentText(b block) (string, error) {
	if b.Source != nil && b.Source.Type == "text" {
		return b.Source.Data, nil
	}
	kind := "missing"
	if b.Source != nil {
		kind = b.Source.Type
	}
	return "", fmt.Errorf("document source %q has no chat-completions form (use a model with an anthropic route)", kind)
}

// message renders the turn: plain text unless an image is present; false when it has no parts.
func (u *userTurn) message() (chatMessage, bool) {
	if len(u.parts) == 0 {
		return chatMessage{}, false
	}
	if u.hasImage {
		return chatMessage{Role: "user", Content: u.parts}, true
	}
	texts := make([]string, len(u.parts))
	for i, p := range u.parts {
		texts[i] = p.Text
	}
	return chatMessage{Role: "user", Content: strings.Join(texts, "\n\n")}, true
}

// toolResult converts a tool_result block to a tool message; its images ride the following user turn.
func (u *userTurn) toolResult(b block) (chatMessage, error) {
	inner, err := parseBlocks(b.Content)
	if err != nil {
		return chatMessage{}, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
	}
	var texts []string
	for _, ib := range inner {
		switch ib.Type {
		case "image":
			u.addImage(ib.Source) // tool messages are text-only
		case "text":
			texts = append(texts, ib.Text)
		case "document":
			text, err := documentText(ib)
			if err != nil {
				return chatMessage{}, fmt.Errorf("tool_result %s: %w", b.ToolUseID, err)
			}
			texts = append(texts, text)
		}
	}
	content := strings.Join(slices.DeleteFunc(texts, func(t string) bool { return t == "" }), "\n")
	if b.IsError {
		content = "[tool error] " + content // chat tool messages have no error flag
	}
	return chatMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: content}, nil
}

// appendUser emits tool results first (they must directly follow the assistant tool_calls), then the user turn.
func appendUser(msgs []chatMessage, blocks []block) ([]chatMessage, error) {
	var turn userTurn
	for _, b := range blocks {
		if b.Type != "tool_result" {
			if err := turn.add(b); err != nil {
				return nil, err
			}
			continue
		}
		m, err := turn.toolResult(b)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if m, ok := turn.message(); ok {
		msgs = append(msgs, m)
	}
	return msgs, nil
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
var geminiSchemaKeys = map[string]bool{
	"type":                 true,
	"format":               true,
	"description":          true,
	"nullable":             true,
	"enum":                 true,
	"items":                true,
	"properties":           true,
	"required":             true,
	"minItems":             true,
	"maxItems":             true,
	"minimum":              true,
	"maximum":              true,
	"minLength":            true,
	"maxLength":            true,
	"pattern":              true,
	"title":                true,
	"default":              true,
	"anyOf":                true,
	"additionalProperties": true,
	"example":              true,
	"propertyOrdering":     true,
	"minProperties":        true,
	"maxProperties":        true,
}

// GeminiSchema strips keys Google rejects from a tool schema (const → enum, oneOf → anyOf); false when unchanged.
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
	changed := rewriteKeywords(m)
	for k, sub := range m {
		if !geminiSchemaKeys[k] {
			delete(m, k)
			changed = true
			continue
		}
		changed = cleanChildren(k, sub) || changed
	}
	return changed
}

// rewriteKeywords turns const into enum and oneOf into anyOf, reporting whether m had either.
func rewriteKeywords(m map[string]any) bool {
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
	return changed
}

// cleanChildren cleans the sub-schemas held under keyword k, reporting whether any changed.
func cleanChildren(k string, sub any) bool {
	changed := false
	switch k {
	case "properties":
		props, _ := sub.(map[string]any)
		for _, p := range props {
			changed = cleanSchema(p) || changed
		}
	case "items", "additionalProperties":
		changed = cleanSchema(sub)
	case "anyOf":
		subs, _ := sub.([]any)
		for _, s := range subs {
			changed = cleanSchema(s) || changed
		}
	}
	return changed
}
