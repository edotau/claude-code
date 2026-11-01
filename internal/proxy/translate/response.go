package translate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   json.RawMessage `json:"content"`
			Refusal   string          `json:"refusal"`
			ToolCalls []inToolCall    `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage      `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// FromChatResponse converts a non-streaming chat-completions response into an Anthropic Messages response.
func FromChatResponse(body []byte, clientModel string) ([]byte, error) {
	var in chatResponse
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("translate: response: %w", err)
	}
	if len(in.Error) > 0 && string(in.Error) != "null" {
		return nil, fmt.Errorf("translate: upstream error: %s", in.Error)
	}
	if len(in.Choices) == 0 {
		return nil, errors.New("translate: response has no choices")
	}
	c := in.Choices[0]
	content := []map[string]any{}
	text := contentText(c.Message.Content)
	if text == "" {
		text = c.Message.Refusal
	}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range c.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = newID("toolu_")
		}
		content = append(content, map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": toolInput(argsText(tc.Function.Arguments))})
	}
	return json.Marshal(map[string]any{
		"id":            newID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         clientModel,
		"content":       content,
		"stop_reason":   stopReason(c.FinishReason, len(c.Message.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsage(in.Usage),
	})
}

// toolInput parses arguments into an object; anything else is kept verbatim under _raw_arguments.
func toolInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) == nil && obj != nil {
		return json.RawMessage(args)
	}
	raw, _ := json.Marshal(map[string]string{"_raw_arguments": args})
	return raw
}

// contentText reads message content as a string or an array of text parts.
func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
