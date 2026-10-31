package agent

import (
	"bufio"
	"strings"
	"testing"
)

func newBR(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

type nonJSONCase struct {
	name   string
	events string
	big    func(text string) string                       // one event carrying text
	run    func(string, eventLog) (Result, string, error) // returns the result and the streamed text
}

func ndjsonCases() []nonJSONCase {
	return []nonJSONCase{
		{"claude", claudeFixture, func(s string) string {
			return `{"type":"assistant","session_id":"s","message":{"content":[{"type":"text","text":"` + s + `"}]}}` + "\n"
		}, func(in string, lg eventLog) (Result, string, error) {
			var out strings.Builder
			res, err := parseClaudeStream(newBR(in), NewTextSink(&out), lg)
			return res, out.String(), err
		}},
		{"codex", codexFixture, func(s string) string {
			return `{"type":"item.completed","item":{"id":"i9","type":"agent_message","text":"` + s + `"}}` + "\n"
		}, func(in string, lg eventLog) (Result, string, error) {
			var out strings.Builder
			res, err := parseCodexStream(newBR(in), NewTextSink(&out), nil, lg)
			return res, out.String(), err
		}},
		{"agy", agyOK, func(s string) string {
			return `{"event":"step_update","step_update":{"conversation_id":"415ed744","step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":"` + s + `"}}` + "\n"
		}, func(in string, lg eventLog) (Result, string, error) {
			var out strings.Builder
			res, err := parseAgyStream(newBR(in), NewTextSink(&out), nil, lg)
			return res, out.String(), err
		}},
		{"vendor", openCodeOK, func(s string) string {
			return `{"type":"text","timestamp":2,"sessionID":"ses_A","part":{"id":"p2","sessionID":"ses_A","type":"text","text":"` + s + `"}}` + "\n"
		}, func(in string, lg eventLog) (Result, string, error) {
			var out strings.Builder
			res, err := parseOpenCodeStream(newBR(in), NewTextSink(&out), nil, lg)
			return res, out.String(), err
		}},
	}
}

// TestNonJSONLineKeepsEvents: a stray non-JSON line is logged as "raw" and the real events after it still parse.
func TestNonJSONLineKeepsEvents(t *testing.T) {
	for _, tc := range ndjsonCases() {
		t.Run(tc.name, func(t *testing.T) {
			var raws []string
			lg := func(kind, text string) {
				if kind == "raw" {
					raws = append(raws, text)
				}
			}
			clean, _, err := tc.run(tc.events, nil)
			if err != nil {
				t.Fatalf("clean stream: %v", err)
			}
			res, _, err := tc.run("WARNING: x\n"+tc.events, lg)
			if err != nil {
				t.Fatalf("stream after WARNING line: %v", err)
			}
			if len(raws) != 1 || raws[0] != "WARNING: x" {
				t.Errorf("raw log = %q, want [WARNING: x]", raws)
			}
			if res.Answer != clean.Answer || res.SessionID != clean.SessionID || res.Usage != clean.Usage {
				t.Errorf("with warning %+v, clean %+v", res, clean)
			}
		})
	}
}

// TestNonJSONLineHugeEvent: one 6 MiB event after a non-JSON line is not truncated or dropped.
func TestNonJSONLineHugeEvent(t *testing.T) {
	const size = 6 << 20
	for _, tc := range ndjsonCases() {
		t.Run(tc.name, func(t *testing.T) {
			in := "WARNING: x\n" + tc.big(strings.Repeat("a", size)) + tc.events
			_, streamed, err := tc.run(in, nil)
			if err != nil {
				t.Fatalf("6 MiB event: %v", err)
			}
			if got := strings.Count(streamed, "a"); got < size {
				t.Errorf("streamed %d 'a' bytes of %d: the huge event was cut", got, size)
			}
		})
	}
}
