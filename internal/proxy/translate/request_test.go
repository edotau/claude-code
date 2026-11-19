package translate

import (
	"strings"
	"testing"
)

// benchMessagesBody is an agentic turn: system blocks, two tools, a tool_use/tool_result pair and a long user text.
var benchMessagesBody = []byte(`{"model":"claude-x","max_tokens":4096,"stream":true,"temperature":0.2,
	"system":[{"type":"text","text":"You are a coding agent.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Be brief."}],
	"tools":[{"name":"read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
	{"name":"bash","description":"run a command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],
	"tool_choice":{"type":"auto"},
	"messages":[{"role":"user","content":"read main.go and run the tests"},
	{"role":"assistant","content":[{"type":"text","text":"reading"},{"type":"tool_use","id":"t1","name":"read","input":{"path":"main.go"}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("package main\\n", 200) + `"},
	{"type":"text","text":"now run them"}]}]}`)

func BenchmarkToChatRequest(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := ToChatRequest(benchMessagesBody, "gpt-5"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFlattenChatContent(b *testing.B) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
		{"role":"assistant","content":"plain"}]}`)
	b.ReportAllocs()
	for b.Loop() {
		FlattenChatContent(body)
	}
}
