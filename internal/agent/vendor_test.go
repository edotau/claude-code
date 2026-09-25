package agent

import (
	"strings"
	"testing"
)

// Sampled from `opencode run --format json` 1.18.31 (ids shortened).
const openCodeOK = `{"type":"step_start","timestamp":1,"sessionID":"ses_A","part":{"id":"p1","sessionID":"ses_A","type":"step-start"}}
{"type":"text","timestamp":2,"sessionID":"ses_A","part":{"id":"p2","sessionID":"ses_A","type":"text","text":"Hi! How can I help you today?"}}
{"type":"step_finish","timestamp":3,"sessionID":"ses_A","part":{"id":"p3","reason":"stop","sessionID":"ses_A","type":"step-finish","tokens":{"total":20009,"input":19880,"output":9,"reasoning":120,"cache":{"write":0,"read":0}},"cost":0}}
`

func TestParseOpenCodeStream(t *testing.T) {
	var out strings.Builder
	res, err := parseOpenCodeStream(strings.NewReader(openCodeOK), &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "ses_A" || res.Answer != "Hi! How can I help you today?" || out.String() != res.Answer {
		t.Errorf("res = %+v, streamed %q", res, out.String())
	}
	if res.Usage != (Usage{InputTokens: 19880, OutputTokens: 129}) {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestParseOpenCodeStreamError(t *testing.T) {
	stream := `{"type":"error","timestamp":1,"sessionID":"ses_B","error":{"name":"APIError","data":{"message":"models/x is not found"}}}` + "\n"
	res, err := parseOpenCodeStream(strings.NewReader(stream), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "models/x is not found") || res.SessionID != "ses_B" {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
}

// Sampled from `gemini --output-format stream-json` 0.61.0.
const geminiOK = `{"type":"init","timestamp":"2026-09-25T12:25:15.373Z","session_id":"8da8c0c1","model":"gemini-3.5-flash-lite"}
{"type":"message","timestamp":"2026-09-25T12:25:15.373Z","role":"user","content":"Count from 1 to 5, one per line."}
{"type":"message","timestamp":"2026-09-25T12:25:17.141Z","role":"assistant","content":"1\n2\n3","delta":true}
{"type":"message","timestamp":"2026-09-25T12:25:17.142Z","role":"assistant","content":"\n4\n5","delta":true}
{"type":"result","timestamp":"2026-09-25T12:25:17.173Z","status":"success","stats":{"total_tokens":16612,"input_tokens":16384,"output_tokens":9,"cached":0,"input":16384,"duration_ms":1800,"tool_calls":0}}
`

func TestParseGeminiStream(t *testing.T) {
	var out strings.Builder
	res, err := parseGeminiStream(strings.NewReader(geminiOK), &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "8da8c0c1" || res.Answer != "1\n2\n3\n4\n5" || out.String() != res.Answer {
		t.Errorf("res = %+v, streamed %q", res, out.String())
	}
	if res.Usage != (Usage{InputTokens: 16384, OutputTokens: 228}) {
		t.Errorf("usage = %+v (thinking must count as output)", res.Usage)
	}
}

func TestParseGeminiStreamError(t *testing.T) {
	stream := `{"type":"init","session_id":"s2","model":"gemini-nope-9"}
{"type":"result","status":"error","error":{"type":"unknown","message":"[API Error: models/gemini-nope-9 is not found]"},"stats":{"total_tokens":0,"input_tokens":0}}
`
	res, err := parseGeminiStream(strings.NewReader(stream), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "gemini-nope-9 is not found") || res.SessionID != "s2" {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
}
