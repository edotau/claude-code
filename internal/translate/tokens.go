package translate

import (
	"encoding/json"
	"fmt"
)

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
