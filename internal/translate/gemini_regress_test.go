package translate

import (
	"strings"
	"testing"
)

const streamReq = `{"model":"x","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

// Gemini rejects stream_options; other models keep include_usage.
func TestToChatRequestStreamOptionsGemini(t *testing.T) {
	out, _, err := ToChatRequest([]byte(streamReq), "databricks-gemini-3-8-flash")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "stream_options") || !strings.Contains(string(out), `"stream":true`) {
		t.Errorf("gemini body = %s, want stream without stream_options", out)
	}
	out, _, err = ToChatRequest([]byte(streamReq), "databricks-claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"stream_options":{"include_usage":true}`) {
		t.Errorf("non-gemini body = %s, want include_usage", out)
	}
}

const toolReplay = `{"model":"x","max_tokens":8,"messages":[{"role":"user","content":"w?"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"f","input":{"c":1}},{"type":"tool_use","id":"b","name":"g","input":{}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"ok"},{"type":"tool_result","tool_use_id":"b","content":"ok"}]}]}`

// Replayed tool calls carry Gemini's validator-bypass thoughtSignature, and only for Gemini.
func TestToChatRequestToolCallThoughtSignature(t *testing.T) {
	out, _, err := ToChatRequest([]byte(toolReplay), "databricks-gemini-3-8-flash")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), `"thoughtSignature":"skip_thought_signature_validator"`); n != 2 {
		t.Errorf("gemini signatures = %d in %s, want 2", n, out)
	}
	out, _, err = ToChatRequest([]byte(toolReplay), "databricks-claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "thoughtSignature") {
		t.Errorf("non-gemini body carries thoughtSignature: %s", out)
	}
}

// delta.content as an array of parts (Databricks Gemini) streams its text; reasoning-only and null do not error.
func TestStreamChatArrayDeltaContent(t *testing.T) {
	run := func(in string) (string, error) {
		var buf strings.Builder
		err := StreamChat(&buf, nil, strings.NewReader(in), "m")
		return buf.String(), err
	}
	out, err := run(`data: {"choices":[{"delta":{"content":[{"type":"text","text":"hel"},{"type":"text","text":"lo"}],"role":"assistant"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n")
	if err != nil || !strings.Contains(out, `"text":"hello"`) {
		t.Fatalf("array content: err=%v out=%s", err, out)
	}
	for name, delta := range map[string]string{
		"reasoning-only": `{"content":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]}],"role":"assistant"}`,
		"null":           `{"content":null}`,
	} {
		in := `data: {"choices":[{"delta":` + delta + `}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
		if out, err := run(in); err != nil || !strings.Contains(out, `"text":"ok"`) {
			t.Errorf("%s: err=%v out=%s", name, err, out)
		}
	}
}
