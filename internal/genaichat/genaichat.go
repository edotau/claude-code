// Package genaichat boxes the google.golang.org/genai SDK behind one narrow entry point.
package genaichat

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/genai"
)

// Usage reports token counts from a single Stream call.
type Usage struct {
	Input, Output, Thinking, CacheRead int
}

// thinkingLevel maps an effort name to the SDK's ThinkingLevel; xhigh/max top out at High.
func thinkingLevel(effort string) genai.ThinkingLevel {
	switch effort {
	case "low":
		return genai.ThinkingLevelLow
	case "medium":
		return genai.ThinkingLevelMedium
	case "high", "xhigh", "max":
		return genai.ThinkingLevelHigh
	default:
		return ""
	}
}

// Stream sends prompt to model, writes each response chunk to w, and returns the full text and usage.
func Stream(ctx context.Context, apiKey, model, prompt, effort string, w io.Writer) (Usage, string, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return Usage{}, "", fmt.Errorf("genai: new client: %w", err)
	}

	var cfg *genai.GenerateContentConfig
	if level := thinkingLevel(effort); level != "" {
		cfg = &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: level}}
	}

	var text string
	var usage Usage
	for resp, err := range client.Models.GenerateContentStream(ctx, model, genai.Text(prompt), cfg) {
		if err != nil {
			return usage, text, fmt.Errorf("genai: stream: %w", err)
		}
		if chunk := resp.Text(); chunk != "" {
			if _, werr := io.WriteString(w, chunk); werr != nil {
				return usage, text, fmt.Errorf("genai: write: %w", werr)
			}
			text += chunk
		}
		if u := resp.UsageMetadata; u != nil {
			usage = Usage{
				Input:     int(u.PromptTokenCount),
				Output:    int(u.CandidatesTokenCount),
				Thinking:  int(u.ThoughtsTokenCount),
				CacheRead: int(u.CachedContentTokenCount),
			}
		}
	}
	return usage, text, nil
}
