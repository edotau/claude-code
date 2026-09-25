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
