package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

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
