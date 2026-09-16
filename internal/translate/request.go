package translate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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

// OpenAI reasoning models reject max_tokens, temperature and top_p; a vendor prefix (openai/) is tolerated.
var reasoningModelRE = regexp.MustCompile(`^(?:[\w.-]+/)?(?:o\d|gpt-5)`)

// ToChatRequest converts an Anthropic Messages request body into a chat-completions body targeting wireModel.
func ToChatRequest(body []byte, wireModel string) (out []byte, stream bool, err error) {
	var in messagesRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, false, fmt.Errorf("translate: request: %w", err)
	}
	req := chatRequest{Model: wireModel, Stop: in.StopSequences, Temperature: in.Temperature, TopP: in.TopP}
	if reasoningModelRE.MatchString(wireModel) {
		req.MaxCompletionTokens, req.Temperature, req.TopP = in.MaxTokens, nil, nil
	} else {
		req.MaxTokens = in.MaxTokens
	}
	if in.Metadata != nil {
		req.User = in.Metadata.UserID
	}
	if in.Stream {
		req.Stream, req.StreamOptions = true, &streamOptions{IncludeUsage: true}
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
			req.Messages = appendAssistant(req.Messages, blocks)
		} else {
			req.Messages = appendUser(req.Messages, blocks)
		}
	}

	for _, t := range in.Tools {
		if len(t.InputSchema) == 0 && t.Type != "" && t.Type != "custom" {
			continue // Anthropic server tools (web_search, …) have no function equivalent
		}
		params := t.InputSchema
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, chatTool{Type: "function", Function: toolDef{Name: t.Name, Description: t.Description, Parameters: params}})
	}
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

	out, err = json.Marshal(req)
	return out, in.Stream, err
}

// appendUser emits tool results first (they must directly follow the assistant tool_calls), then the user turn.
func appendUser(msgs []chatMessage, blocks []block) []chatMessage {
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
			inner, _ := parseBlocks(b.Content)
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
		return msgs
	}
	if !hasImage {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p.Text
		}
		return append(msgs, chatMessage{Role: "user", Content: strings.Join(texts, "\n\n")})
	}
	return append(msgs, chatMessage{Role: "user", Content: parts})
}

func appendAssistant(msgs []chatMessage, blocks []block) []chatMessage {
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
		calls = append(calls, chatToolCall{ID: b.ID, Type: "function", Function: chatFunction{Name: b.Name, Arguments: args}})
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
