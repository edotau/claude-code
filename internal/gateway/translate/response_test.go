package translate

import (
	"strings"
	"testing"
)

var benchChatResponse = []byte(`{"id":"c1","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant",
	"content":"` + strings.Repeat("hello world ", 100) + `","tool_calls":[{"id":"call_1","type":"function",
	"function":{"name":"read","arguments":"{\"path\":\"main.go\"}"}}]}}],
	"usage":{"prompt_tokens":1200,"completion_tokens":80,"prompt_tokens_details":{"cached_tokens":1000}}}`)

func BenchmarkFromChatResponse(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := FromChatResponse(benchChatResponse, "claude-x"); err != nil {
			b.Fatal(err)
		}
	}
}
