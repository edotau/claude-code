package translate

import (
	"cmp"
	"encoding/json"
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
	r, err := ParseReply(body)
	if err != nil {
		return nil, err
	}
	content := []map[string]any{}
	if text := cmp.Or(r.Text, r.Refusal); text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range r.Calls {
		content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": toolInput(c.Args)})
	}
	return json.Marshal(map[string]any{
		"id":            newID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         clientModel,
		"content":       content,
		"stop_reason":   stopReason(r.Finish, len(r.Calls) > 0),
		"stop_sequence": nil,
		"usage":         anthropicUsage(r.Usage),
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

// contentText reads message content as a string or an array of text parts; an absent field is "".
func contentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("translate: message content: %w", err)
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	return b.String(), nil
}
