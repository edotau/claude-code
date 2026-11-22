package server

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/proxy/translate"
)

func TestAnthropicSearchWire(t *testing.T) {
	body := `{"model":"x","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":2}],` +
		`"messages":[{"role":"assistant","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{}},` +
		`{"type":"web_search_tool_result","tool_use_id":"s1","content":[{"title":"T","url":"https://e.com","encrypted_content":"S"}]}]}]}`
	out, stream, err := anthropicSearchBody([]byte(body), "databricks-claude-sonnet-5-5")
	if err != nil || !stream {
		t.Fatalf("body: stream=%v err=%v", stream, err)
	}
	s := string(out)
	for _, bad := range []string{`"stream"`, "web_search_20250305", "server_tool_use", "web_search_tool_result"} {
		if strings.Contains(s, bad) {
			t.Errorf("body still has %s: %s", bad, s)
		}
	}
	if !strings.Contains(s, "input_schema") || !strings.Contains(s, "https://e.com") {
		t.Errorf("tool or flattened results missing: %s", s)
	}
}

func TestAnthropicAsChat(t *testing.T) {
	chat, content, err := anthropicAsChat([]byte(`{"content":[{"type":"tool_use","id":"tu1","name":"web_search","input":{"query":"q"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":4}}`))
	if err != nil || !strings.Contains(string(content), "tu1") {
		t.Fatalf("chat: %v %s", err, content)
	}
	reply, err := translate.ParseReply(chat)
	if err != nil || len(reply.Calls) != 1 || reply.Calls[0].SearchQuery() != "q" {
		t.Fatalf("reply: %+v %v", reply, err)
	}
}
