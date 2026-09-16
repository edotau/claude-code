package translate

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func init() { newID = func(prefix string) string { return prefix + "test" } }

// jsonEqual compares two JSON documents semantically.
func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want invalid JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestToChatRequest(t *testing.T) {
	tests := []struct {
		name, model, in, want string
		stream                bool
	}{
		{
			name:  "string system and content with sampling knobs",
			model: "deepseek-chat",
			in: `{"model":"claude-x","system":"be brief","max_tokens":100,"temperature":0.5,"top_p":0.9,
				"stop_sequences":["END"],"metadata":{"user_id":"u1"},"unknown":true,
				"messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"deepseek-chat","max_tokens":100,"temperature":0.5,"top_p":0.9,"stop":["END"],"user":"u1",
				"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		},
		{
			name:  "system blocks, stream, reasoning model uses max_completion_tokens",
			model: "openai/gpt-5",
			in: `{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}],
				"max_tokens":8,"temperature":1,"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"y"}]}]}`,
			want: `{"model":"openai/gpt-5","max_completion_tokens":8,"stream":true,"stream_options":{"include_usage":true},
				"messages":[{"role":"system","content":"a\n\nb"},{"role":"user","content":"x\n\ny"}]}`,
			stream: true,
		},
		{
			name:  "tool_use and tool_result ordering, thinking dropped",
			model: "m",
			in: `{"max_tokens":1,"messages":[
				{"role":"user","content":"list files"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"ok"},
					{"type":"tool_use","id":"t1","name":"ls","input":{"path": "/"}},{"type":"tool_use","id":"t2","name":"pwd","input":{}}]},
				{"role":"user","content":[{"type":"text","text":"also this"},
					{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
					{"type":"tool_result","tool_use_id":"t2","content":"/home","is_error":false}]},
				{"role":"assistant","content":[{"type":"redacted_thinking","data":"x"}]}]}`,
			want: `{"model":"m","max_tokens":1,"messages":[
				{"role":"user","content":"list files"},
				{"role":"assistant","content":"ok","tool_calls":[
					{"id":"t1","type":"function","function":{"name":"ls","arguments":"{\"path\":\"/\"}"}},
					{"id":"t2","type":"function","function":{"name":"pwd","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t1","content":"a\nb"},
				{"role":"tool","tool_call_id":"t2","content":"/home"},
				{"role":"user","content":"also this"}]}`,
		},
		{
			name:  "assistant tool call without text has null content",
			model: "m",
			in:    `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":null}]}]}`,
			want:  `{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		},
		{
			name:  "images base64 and url, tool_result image rides the user turn",
			model: "m",
			in: `{"messages":[{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},
				{"type":"text","text":"look"},
				{"type":"image","source":{"type":"url","url":"https://x/y.jpg"}}]}]}`,
			want: `{"model":"m","messages":[
				{"role":"tool","tool_call_id":"t1","content":""},
				{"role":"user","content":[
					{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
					{"type":"text","text":"look"},
					{"type":"image_url","image_url":{"url":"https://x/y.jpg"}}]}]}`,
		},
		{
			name:  "tools, server tools skipped, tool_choice any",
			model: "m",
			in: `{"messages":[],"tools":[{"name":"f","description":"d","input_schema":{"type":"object","properties":{"a":{"type":"string"}}}},
				{"type":"web_search_20250305","name":"web_search"},{"name":"g"}],"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`,
			want: `{"model":"m","messages":null,"tool_choice":"required","parallel_tool_calls":false,"tools":[
				{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}},
				{"type":"function","function":{"name":"g","parameters":{"type":"object","properties":{}}}}]}`,
		},
		{
			name:  "tool_choice tool",
			model: "m",
			in:    `{"messages":[],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f"}}`,
			want: `{"model":"m","messages":null,"tool_choice":{"type":"function","function":{"name":"f"}},
				"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
		},
		{
			name:  "tool_choice none",
			model: "m",
			in:    `{"messages":[],"tools":[{"name":"f","input_schema":{}}],"tool_choice":{"type":"none"}}`,
			want:  `{"model":"m","messages":null,"tool_choice":"none","tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`,
		},
		{
			name:  "tool_choice without tools",
			model: "m",
			in:    `{"messages":[],"tool_choice":{"type":"auto"}}`,
			want:  `{"model":"m","messages":null}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, stream, err := ToChatRequest([]byte(tt.in), tt.model)
			if err != nil {
				t.Fatal(err)
			}
			if stream != tt.stream {
				t.Errorf("stream = %v, want %v", stream, tt.stream)
			}
			jsonEqual(t, out, tt.want)
		})
	}
	if _, _, err := ToChatRequest([]byte(`{`), "m"); err == nil {
		t.Error("invalid JSON: want error")
	}
}

func TestFromChatResponse(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			name: "text with cached usage",
			in: `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello","reasoning_content":"secret"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`,
			want: `{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","stop_reason":"end_turn","stop_sequence":null,
				"content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":6,"output_tokens":3,"cache_read_input_tokens":4}}`,
		},
		{
			name: "tool calls with invalid and object arguments",
			in: `{"choices":[{"message":{"content":null,"tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"a","arguments":"{\"x\":1}"}},
				{"id":"","type":"function","function":{"name":"b","arguments":"{oops"}},
				{"id":"call_3","type":"function","function":{"name":"c","arguments":{"y":2}}},
				{"id":"call_4","type":"function","function":{"name":"d","arguments":""}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
			want: `{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","stop_reason":"tool_use","stop_sequence":null,
				"content":[
					{"type":"tool_use","id":"call_1","name":"a","input":{"x":1}},
					{"type":"tool_use","id":"toolu_test","name":"b","input":{"_raw_arguments":"{oops"}},
					{"type":"tool_use","id":"call_3","name":"c","input":{"y":2}},
					{"type":"tool_use","id":"call_4","name":"d","input":{}}],
				"usage":{"input_tokens":1,"output_tokens":2}}`,
		},
		{
			name: "gemini stop with tool calls maps to tool_use",
			in:   `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"stop"}]}`,
			want: `{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","stop_reason":"tool_use","stop_sequence":null,
				"content":[{"type":"tool_use","id":"c","name":"f","input":{}}],"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "length and content parts",
			in:   `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},"finish_reason":"length"}]}`,
			want: `{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","stop_reason":"max_tokens","stop_sequence":null,
				"content":[{"type":"text","text":"ab"}],"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
		{
			name: "content_filter with refusal",
			in:   `{"choices":[{"message":{"content":null,"refusal":"no"},"finish_reason":"content_filter"}]}`,
			want: `{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","stop_reason":"end_turn","stop_sequence":null,
				"content":[{"type":"text","text":"no"}],"usage":{"input_tokens":0,"output_tokens":0}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := FromChatResponse([]byte(tt.in), "claude-client")
			if err != nil {
				t.Fatal(err)
			}
			jsonEqual(t, out, tt.want)
		})
	}
	for _, bad := range []string{`{`, `{"choices":[]}`, `{"error":{"message":"x"}}`} {
		if _, err := FromChatResponse([]byte(bad), "m"); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

// parseSSE splits Anthropic SSE output into "event <json>" strings.
func parseSSE(t *testing.T, out string) []string {
	t.Helper()
	if out == "" {
		return nil
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("output does not end with a blank line: %q", out)
	}
	var events []string
	for _, frame := range strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n") {
		lines := strings.Split(frame, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("bad frame %q", frame)
		}
		typ := strings.TrimPrefix(lines[0], "event: ")
		data := strings.TrimPrefix(lines[1], "data: ")
		var v map[string]any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			t.Fatalf("bad data %q: %v", data, err)
		}
		if v["type"] != typ {
			t.Errorf("event %q carries type %v", typ, v["type"])
		}
		events = append(events, typ+" "+data)
	}
	return events
}

func eventsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d\n got: %s\nwant: %s", len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range got {
		gt, gd, _ := strings.Cut(got[i], " ")
		wt, wd, _ := strings.Cut(want[i], " ")
		if gt != wt {
			t.Errorf("event %d: type %q, want %q", i, gt, wt)
			continue
		}
		jsonEqual(t, []byte(gd), wd)
	}
}

const msgStart = `message_start {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-client","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`

const textStream = ": keepalive\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking...\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\r\n\r\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

var textEvents = []string{
	msgStart,
	`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
	`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
	`content_block_stop {"type":"content_block_stop","index":0}`,
	`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"output_tokens":2}}`,
	`message_stop {"type":"message_stop"}`,
}

func TestStreamChat(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		reader func(io.Reader) io.Reader
		want   []string
	}{
		{name: "text only with usage chunk", in: textStream, want: textEvents},
		{name: "one byte per read splits lines", in: textStream, reader: iotest.OneByteReader, want: textEvents},
		{name: "half reads", in: textStream, reader: iotest.HalfReader, want: textEvents},
		{
			name: "text then two parallel tool calls with fragmented args, no DONE",
			in: `data: {"choices":[{"delta":{"content":"Checking"}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"ls","arguments":""}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"pa"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/\"}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"pwd","arguments":"{"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}` + "\n",
			want: []string{
				msgStart,
				`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking"}}`,
				`content_block_stop {"type":"content_block_stop","index":0}`,
				`content_block_start {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_a","name":"ls","input":{}}}`,
				`content_block_delta {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pa"}}`,
				`content_block_delta {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"th\":\"/\"}"}}`,
				`content_block_stop {"type":"content_block_stop","index":1}`,
				`content_block_start {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_b","name":"pwd","input":{}}}`,
				`content_block_delta {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
				`content_block_delta {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"}"}}`,
				`content_block_stop {"type":"content_block_stop","index":2}`,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":0}}`,
				`message_stop {"type":"message_stop"}`,
			},
		},
		{
			name: "gemini style: no index, whole args, finish stop, name after args buffered",
			in: `data: {"choices":[{"delta":{"tool_calls":[{"id":"g1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"id":"g2","function":{"arguments":"{}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"id":"g2","function":{"name":"g"}}]},"finish_reason":"stop"}]}` + "\n\n",
			want: []string{
				msgStart,
				`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"g1","name":"f","input":{}}}`,
				`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
				`content_block_stop {"type":"content_block_stop","index":0}`,
				`content_block_start {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"g2","name":"g","input":{}}}`,
				`content_block_delta {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
				`content_block_stop {"type":"content_block_stop","index":1}`,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":0}}`,
				`message_stop {"type":"message_stop"}`,
			},
		},
		{
			name: "empty stream still closes",
			in:   ": ping\n\n",
			want: []string{
				msgStart,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":0}}`,
				`message_stop {"type":"message_stop"}`,
			},
		},
		{
			name: "length",
			in:   `data: {"choices":[{"delta":{"content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\ndata: {\"ignored\":true}\n\n",
			want: []string{
				msgStart,
				`content_block_start {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`content_block_delta {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
				`content_block_stop {"type":"content_block_stop","index":0}`,
				`message_delta {"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":1}}`,
				`message_stop {"type":"message_stop"}`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r io.Reader = strings.NewReader(tt.in)
			if tt.reader != nil {
				r = tt.reader(r)
			}
			var buf strings.Builder
			flushes := 0
			if err := StreamChat(&buf, func() { flushes++ }, r, "claude-client"); err != nil {
				t.Fatal(err)
			}
			got := parseSSE(t, buf.String())
			eventsEqual(t, got, tt.want)
			if flushes != len(got) {
				t.Errorf("flushes = %d, want one per event (%d)", flushes, len(got))
			}
		})
	}
}

func TestStreamChatErrors(t *testing.T) {
	t.Run("in-band upstream error", func(t *testing.T) {
		in := `data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n" +
			`data: {"error":{"message":"quota exceeded","code":429}}` + "\n\n"
		var buf strings.Builder
		err := StreamChat(&buf, nil, strings.NewReader(in), "claude-client")
		if !errors.Is(err, errUpstream) {
			t.Fatalf("err = %v, want errUpstream", err)
		}
		got := parseSSE(t, buf.String())
		last := got[len(got)-1]
		if !strings.HasPrefix(last, "error ") {
			t.Fatalf("last event %q, want error", last)
		}
		jsonEqual(t, []byte(strings.TrimPrefix(last, "error ")), `{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`)
	})
	t.Run("read error before output writes nothing", func(t *testing.T) {
		var buf strings.Builder
		err := StreamChat(&buf, nil, iotest.ErrReader(io.ErrUnexpectedEOF), "m")
		if err == nil || buf.Len() != 0 {
			t.Fatalf("err = %v, output %q; want error and no output", err, buf.String())
		}
	})
	t.Run("read error mid-stream emits error event", func(t *testing.T) {
		r := io.MultiReader(strings.NewReader(`data: {"choices":[{"delta":{"content":"a"}}]}`+"\n\n"), iotest.ErrReader(io.ErrUnexpectedEOF))
		var buf strings.Builder
		if err := StreamChat(&buf, nil, r, "m"); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v", err)
		}
		got := parseSSE(t, buf.String())
		if !strings.HasPrefix(got[len(got)-1], "error ") {
			t.Fatalf("last event %q, want error", got[len(got)-1])
		}
	})
	t.Run("malformed chunk", func(t *testing.T) {
		var buf strings.Builder
		if err := StreamChat(&buf, nil, strings.NewReader("data: {nope\n\n"), "m"); err == nil {
			t.Fatal("want error")
		}
		if got := parseSSE(t, buf.String()); len(got) != 1 || !strings.HasPrefix(got[0], "error ") {
			t.Fatalf("events %v, want one error", got)
		}
	})
}

func TestFromChatError(t *testing.T) {
	tests := []struct {
		status   int
		body     string
		wantType string
		wantMsg  string
	}{
		{400, `{"error":{"message":"bad field","type":"invalid_request_error"}}`, "invalid_request_error", "bad field"},
		{401, `[{"error":{"code":401,"message":"API key not valid","status":"UNAUTHENTICATED"}}]`, "authentication_error", "API key not valid"},
		{403, `{"error":"forbidden model"}`, "permission_error", "forbidden model"},
		{404, `{"detail":"no route"}`, "not_found_error", "no route"},
		{413, ``, "request_too_large", "Request Entity Too Large"},
		{429, `slow down`, "rate_limit_error", "slow down"},
		{500, `<html>boom</html>`, "api_error", "<html>boom</html>"},
		{503, `{}`, "overloaded_error", "{}"},
		{529, `{"message":"busy"}`, "overloaded_error", "busy"},
		{422, `{"error":{"message":"unprocessable"}}`, "invalid_request_error", "unprocessable"},
	}
	for _, tt := range tests {
		got := FromChatError(tt.status, []byte(tt.body))
		want, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": tt.wantType, "message": tt.wantMsg}})
		jsonEqual(t, got, string(want))
	}
}

func TestCountTokens(t *testing.T) {
	tests := []struct {
		name, in string
		want     int
	}{
		{"string content", `{"system":"12345678","messages":[{"role":"user","content":"abcd"}]}`, 3},
		{"blocks, tools, image", `{"messages":[{"role":"user","content":[{"type":"text","text":"abcd"},{"type":"image","source":{}},
			{"type":"tool_result","tool_use_id":"x","content":[{"type":"text","text":"abcd"}]}]}],"tools":[{"name":"f"}]}`, (8 + imageChars + len(`[{"name":"f"}]`) + 3) / 4},
		{"empty", `{"messages":[]}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := CountTokens([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(map[string]int{"input_tokens": tt.want})
			jsonEqual(t, out, string(want))
		})
	}
	if _, err := CountTokens([]byte(`nope`)); err == nil {
		t.Error("invalid JSON: want error")
	}
}
