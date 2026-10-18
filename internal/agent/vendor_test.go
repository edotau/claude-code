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

// Captured live from `agy --output-format stream-json` 1.4.2 on this box.
const agyOK = `{"event":"init","conversation_id":"415ed744","init":{"model":"gemini-3.8-flash-low","cwd":"/tmp","tools":["ask_permission"]}}
{"event":"step_update","step_update":{"conversation_id":"415ed744","step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"conversation_id":"415ed744","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"pong"}}
{"event":"result","result":{"conversation_id":"415ed744","status":"SUCCESS","response":"pong\n","duration_seconds":1.57,"num_turns":1,"usage":{"input_tokens":18621,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":18622}}}
`

func TestParseAgyStream(t *testing.T) {
	var out strings.Builder
	res, err := parseAgyStream(strings.NewReader(agyOK), &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "415ed744" || res.Answer != "pong" || out.String() != "pong" {
		t.Errorf("res = %+v, streamed %q", res, out.String())
	}
	if res.Usage != (Usage{InputTokens: 18621, OutputTokens: 1, CacheReadTokens: 0}) {
		t.Errorf("usage = %+v", res.Usage)
	}
}

// Captured live: a tool call is soft-denied headless (exit 0, no step_update text, status still SUCCESS).
const agySoftDeny = `{"event":"result","result":{"conversation_id":"d1","status":"SUCCESS","response":"","num_turns":1,"usage":{"input_tokens":71644,"output_tokens":346},"denied_actions":[{"action":"command","display_name":"RunCommand"}]}}
`

func TestParseAgyStream_SoftDeny(t *testing.T) {
	res, err := parseAgyStream(strings.NewReader(agySoftDeny), &strings.Builder{})
	if err != nil {
		t.Fatalf("a soft deny is still status SUCCESS: %v", err)
	}
	if res.SessionID != "d1" || res.Answer != "" {
		t.Errorf("res = %+v, want empty answer (no agent_response deltas, tool call denied)", res)
	}
	if res.Usage != (Usage{InputTokens: 71644, OutputTokens: 346}) {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestParseAgyStream_Error(t *testing.T) {
	stream := `{"event":"init","conversation_id":"e1","init":{"model":"gemini-nope-9","cwd":"/tmp","tools":[]}}
{"event":"result","result":{"conversation_id":"e1","status":"ERROR","error":"model gemini-nope-9 is not found","usage":{"input_tokens":100,"output_tokens":0}}}
`
	res, err := parseAgyStream(strings.NewReader(stream), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "gemini-nope-9 is not found") || res.SessionID != "e1" {
		t.Fatalf("err = %v, res = %+v", err, res)
	}
}
