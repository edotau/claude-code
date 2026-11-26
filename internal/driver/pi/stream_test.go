package pi

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/contract"
)

type collectSink struct{ evs []wire.Event }

func (c *collectSink) Emit(ev wire.Event) error { c.evs = append(c.evs, ev); return nil }

func (c *collectSink) count(typ string) (n int) {
	for _, ev := range c.evs {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

func parseFixture(t *testing.T, data string) (wire.Result, *collectSink, int, error) {
	t.Helper()
	sink, seen := &collectSink{}, 0
	res, err := parseEvents(bufio.NewReader(strings.NewReader(data)), sink, nil, func(piEvent) error { seen++; return nil })
	return res, sink, seen, err
}

func wantUsage(t *testing.T, res wire.Result, want wire.Usage) {
	t.Helper()
	if res.Usage != want {
		t.Fatalf("usage %+v, want %+v (cacheWrite1h is part of cacheWrite, never added)", res.Usage, want)
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseEventsAnswerWithUsage(t *testing.T) {
	res, sink, seen, err := parseFixture(t, readFixture(t, "answer.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Answer + "|" + res.Reported; got != "ok|databricks-kimi-k3" || res.SessionID == "" {
		t.Fatalf("answer|model %q session %q", got, res.SessionID)
	}
	wantUsage(t, res, wire.Usage{InputTokens: 618, OutputTokens: 34})
	got := []int{sink.count(wire.EventStart), sink.count(wire.EventText), sink.count(wire.EventError)}
	if !slices.Equal(got, []int{1, 1, 0}) || seen < 10 {
		t.Fatalf("events %+v seen %d", sink.evs, seen)
	}
}

func TestParseEventsToolPair(t *testing.T) {
	res, sink, _, err := parseFixture(t, readFixture(t, "tool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if sink.count(wire.EventToolUse) != 1 || sink.count(wire.EventToolResult) != 1 {
		t.Fatalf("events %+v", sink.evs)
	}
	for _, ev := range sink.evs {
		if ev.Type == wire.EventToolResult && (ev.Tool.Name != "read" || ev.Status != "completed") {
			t.Fatalf("tool result %+v", ev)
		}
	}
	wantUsage(t, res, wire.Usage{InputTokens: 6, OutputTokens: 304, CacheCreationTokens: 11761, CacheReadTokens: 10138})
	if !strings.Contains(res.Answer, "\n") {
		t.Fatalf("turns are joined by a newline: %q", res.Answer)
	}
}

func TestParseEventsWrittenOnlyOnSuccess(t *testing.T) {
	call := func(id, tool, path, isErr string) string {
		return `{"type":"tool_execution_start","toolCallId":"` + id + `","toolName":"` + tool + `","args":{"path":"` + path + `"}}` + "\n" +
			`{"type":"tool_execution_end","toolCallId":"` + id + `","toolName":"` + tool + `","result":{"content":[{"type":"text","text":"ok"}]},"isError":` + isErr + `,"durationMs":3}` + "\n"
	}
	res, _, _, err := parseFixture(t, call("w1", "write", "/work/repo/a.txt", "false")+
		call("w2", "edit", "b.txt", "false")+call("w3", "edit", "err.txt", "true")+
		call("w4", "read", "r.txt", "false")+msgEnd("stop", "")+settled)
	if err != nil || !slices.Equal(res.Written, []string{"/work/repo/a.txt", "b.txt"}) {
		t.Fatalf("Written = %v, err = %v", res.Written, err)
	}
}

func TestParseEventsStopReasonErrorWithExitZero(t *testing.T) {
	res, sink, _, err := parseFixture(t, readFixture(t, "error.jsonl"))
	re, ok := err.(*retry.Error)
	if !ok || re.Kind != retry.KindFatal || re.Progressed {
		t.Fatalf("want a fatal classified error, got %#v", err)
	}
	if !strings.Contains(err.Error(), "databricks-gpt-5 is deprecated") || sink.count(wire.EventError) != 1 {
		t.Fatalf("error %v events %+v", err, sink.evs)
	}
	if res.Answer != "" {
		t.Fatalf("answer %q", res.Answer)
	}
}

func TestParseEventsNoAssistantMessageIsError(t *testing.T) {
	_, sink, _, err := parseFixture(t, `{"type":"session","id":"s1"}`+"\n"+`{"type":"agent_start"}`+"\n")
	if err == nil || !strings.Contains(err.Error(), "pi ended without an answer") || sink.count(wire.EventError) != 1 {
		t.Fatalf("err %v events %+v", err, sink.evs)
	}
}

func TestParseEventsSkipsUnknownType(t *testing.T) {
	data := `{"type":"brand_new_event","x":1}` + "\n" + readFixture(t, "answer.jsonl")
	res, _, _, err := parseFixture(t, data)
	if err != nil || res.Answer != "ok" {
		t.Fatalf("answer %q err %v", res.Answer, err)
	}
}

func TestParseEventsRetryThenSuccessClearsFailure(t *testing.T) {
	bad := `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"boom"}}` + "\n"
	again := `{"type":"agent_end","willRetry":true}` + "\n" + `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":10}` + "\n"
	good := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"output":2}}}` + "\n"
	_, sink, _, err := parseFixture(t, bad+again+good+settled)
	if err != nil || sink.count(wire.EventRetry) != 1 || sink.count(wire.EventError) != 0 {
		t.Fatalf("err %v events %+v", err, sink.evs)
	}
}

func TestParseEventsCacheWrite1hIsPartOfCacheWrite(t *testing.T) {
	line := `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":1,"output":2,"cacheWrite":100,"cacheWrite1h":60}}}` + "\n"
	res, _, _, err := parseFixture(t, line+settled)
	if err != nil {
		t.Fatal(err)
	}
	wantUsage(t, res, wire.Usage{InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 100})
}

const settled = `{"type":"agent_settled","aborted":false}` + "\n"

func msgEnd(stop, errMsg string) string {
	return `{"type":"message_end","message":{"role":"assistant","stopReason":"` + stop + `","errorMessage":"` + errMsg + `","usage":{"output":1}}}` + "\n"
}

func TestParseEventsOverflowRecoverySettlesAtAgentSettled(t *testing.T) {
	data := msgEnd("error", "context overflow") + `{"type":"agent_end","willRetry":false}` + "\n" +
		`{"type":"compaction_start"}` + "\n" + `{"type":"compaction_end"}` + "\n" + msgEnd("stop", "") + `{"type":"agent_end"}` + "\n" + settled
	if _, sink, _, err := parseFixture(t, data); err != nil || sink.count(wire.EventError) != 0 {
		t.Fatalf("recovered overflow failed the run: %v %+v", err, sink.evs)
	}
}

func TestParseEventsNonTextDeltaArray(t *testing.T) {
	data := `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","contentIndex":0,"delta":[{"type":"text","text":"x"}]}}` + "\n" +
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}` + "\n" +
		msgEnd("stop", "") + settled
	res, sink, _, err := parseFixture(t, data)
	if err != nil || res.Answer != "ok" || sink.count(wire.EventText) != 1 {
		t.Fatalf("answer %q err %v events %+v", res.Answer, err, sink.evs)
	}
}

func TestParseEventsRunMustFinishProperly(t *testing.T) {
	text := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"partial"}}` + "\n"
	for name, c := range map[string]struct{ data, want string }{
		"cut after toolUse":           {text + msgEnd("toolUse", "") + `{"type":"tool_execution_start","toolCallId":"c1","toolName":"read"}` + "\n", "ended before finishing"},
		"tool left open":              {text + msgEnd("stop", "") + `{"type":"tool_execution_start","toolCallId":"c1","toolName":"read"}` + "\n" + settled, "tool call running"},
		"stop but never settled":      {text + msgEnd("stop", ""), "ended before finishing"},
		"malformed last line":         {text + msgEnd("stop", "") + `{"type":"agent_set`, "ended before finishing"},
		"aborted stop":                {text + msgEnd("aborted", "") + settled, "aborted"},
		"length stop":                 {text + msgEnd("length", "") + settled, "truncated"},
		"unknown stop":                {text + msgEnd("brand-new", "") + settled, "unfinished stop reason"},
		"settled on toolUse":          {text + msgEnd("toolUse", "") + settled, "tool call pending"},
		"tool opened after settled":   {text + msgEnd("stop", "") + settled + `{"type":"tool_execution_start","toolCallId":"c1","toolName":"read"}` + "\n", "ended before finishing"},
		"toolUse after settled":       {text + msgEnd("stop", "") + settled + msgEnd("toolUse", ""), "ended before finishing"},
		"agent_start after settled":   {text + msgEnd("stop", "") + settled + `{"type":"agent_start"}` + "\n", "ended before finishing"},
		"turn_start after settled":    {text + msgEnd("stop", "") + settled + `{"type":"turn_start"}` + "\n", "ended before finishing"},
		"message_start after settled": {text + msgEnd("stop", "") + settled + `{"type":"message_start"}` + "\n", "ended before finishing"},
		"settled aborted":             {text + msgEnd("stop", "") + `{"type":"agent_settled","aborted":true}` + "\n", "run aborted"},
	} {
		res, _, _, err := parseFixture(t, c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if res.Answer == "" {
			t.Errorf("%s: partial text was dropped", name)
		}
	}
}

// An onEvent error stops the stream before that event is forwarded to the sink.
func TestParseEventsOnEventErrorStopsBeforeForwarding(t *testing.T) {
	sink, boom := &collectSink{}, errors.New("guard")
	_, err := parseEvents(bufio.NewReader(strings.NewReader(`{"type":"session","id":"s1"}`+"\n")), sink, nil,
		func(piEvent) error { return boom })
	if !errors.Is(err, boom) || len(sink.evs) != 0 {
		t.Errorf("err = %v, forwarded %v; want the guard error and no events", err, sink.evs)
	}
}

func TestParseEventsTextDeltaArray(t *testing.T) {
	data := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":[{"type":"text","text":"o"},{"type":"text","text":"k"}]}}` + "\n" +
		msgEnd("stop", "") + settled
	res, _, _, err := parseFixture(t, data)
	if err != nil || res.Answer != "ok" {
		t.Fatalf("answer %q err %v", res.Answer, err)
	}
}

func TestParseEventsLogCoalescesLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job.log")
	log := wire.NewLogSink(path)
	defer log.Close()
	data := textDelta("I") + textDelta(" had\na sec") + textDelta("ond line\nlast") +
		msgEnd("toolUse", "") +
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"read"}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read"}` + "\n" +
		textDelta("done") + msgEnd("stop", "") + settled
	sink := &collectSink{}
	res, err := parseEvents(bufio.NewReader(strings.NewReader(data)), sink, nil, nil,
		wire.EventLogFor(wire.WithLogEvents(t.Context(), log)))
	log.Close()
	if err != nil || res.Answer != "I had\na second line\nlast\ndone" {
		t.Fatalf("answer = %q, err = %v", res.Answer, err)
	}
	got, err := os.ReadFile(path)
	want := "▸ assistant: I had\n▸ assistant: a second line\n▸ assistant: last\n" +
		"▸ tool: read\n▸ tool: read: completed\n▸ assistant: done\n"
	if err != nil || string(got) != want {
		t.Fatalf("log = %q, err = %v; want %q", got, err, want)
	}
	var chunks []string
	for _, ev := range sink.evs {
		if ev.Type == wire.EventText {
			chunks = append(chunks, ev.Text)
			if ev.Sep != (ev.Text == "done") {
				t.Errorf("text event separator changed: %+v", ev)
			}
		}
	}
	counts := []int{sink.count(wire.EventToolUse), sink.count(wire.EventToolResult)}
	if !slices.Equal(chunks, []string{"I", " had\na sec", "ond line\nlast", "done"}) || !slices.Equal(counts, []int{1, 1}) {
		t.Fatalf("foreground events changed: %+v", sink.evs)
	}
}

func TestParseEventsLogFlushesPartialText(t *testing.T) {
	for name, tail := range map[string]string{
		"EOF": "", "decode error": `{"type":1}` + "\n",
		"delta error": `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":{}}}` + "\n",
		"retry":       `{"type":"auto_retry_start","attempt":1,"maxAttempts":2}` + "\n",
		"tool":        `{"type":"tool_execution_start","toolCallId":"c1","toolName":"read"}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			lg := func(kind, text string) { lines = append(lines, kind+": "+text) }
			_, err := parseEvents(bufio.NewReader(strings.NewReader(textDelta("par")+textDelta("tial")+tail)),
				wire.Discard, nil, nil, lg)
			if err == nil || len(lines) == 0 || lines[0] != "assistant: partial" {
				t.Fatalf("log = %q, err = %v; want partial text before any boundary or error", lines, err)
			}
			if strings.Count(strings.Join(lines, "\n"), "assistant: partial") != 1 {
				t.Fatalf("partial text logged more than once: %q", lines)
			}
		})
	}
}

func textDelta(text string) string {
	raw, _ := json.Marshal(text)
	return `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":` + string(raw) + "}}\n"
}

func TestDeltaTextShapes(t *testing.T) {
	for raw, want := range map[string]string{`"ok"`: "ok", `[{"type":"text","text":"o"},{"text":"k"}]`: "ok"} {
		if got, err := deltaText([]byte(raw)); err != nil || got != want {
			t.Errorf("deltaText(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := deltaText([]byte(`{"text":"x"}`)); err == nil {
		t.Error("object delta decoded, want an error")
	}
	var ev piEvent
	must := json.Unmarshal([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":{"a":1}}}`), &ev)
	if must != nil || isMeaningful(ev) {
		t.Error("undecodable delta is meaningful")
	}
}

func TestParseEventsDeltaArrayFixtureAndTruncation(t *testing.T) {
	data := readFixture(t, "delta_array.jsonl")
	if res, _, _, err := parseFixture(t, data); err != nil || res.Answer != "ok" {
		t.Fatalf("array delta: answer %q err %v", res.Answer, err)
	}
	cut := strings.TrimSuffix(data, "\n")
	cut = cut[:strings.LastIndex(cut, "\n")+len(`{"type":"agent_set`)+1]
	res, _, _, err := parseFixture(t, cut)
	if err == nil || !strings.Contains(err.Error(), "ended before finishing") {
		t.Fatalf("truncated last line: answer %q err %v, want a run failure, never success", res.Answer, err)
	}
}

func TestParseEventsLiveUsageAcrossWires(t *testing.T) {
	old := usageEvery
	usageEvery = 0
	defer func() { usageEvery = old }()
	upd := func(in, out int) string {
		return `{"type":"message_update","usage":{"input":` + strconv.Itoa(in) + `,"output":` + strconv.Itoa(out) +
			`,"cacheRead":4176,"cacheWrite":0,"cost":{"total":0},"cacheWrite1h":0,"reasoning":0},"assistantMessageEvent":{"type":"toolcall_delta","delta":""}}`
	}
	end := func(in, out int) string {
		return `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":` + strconv.Itoa(in) +
			`,"output":` + strconv.Itoa(out) + `,"cacheRead":4176,"cacheWrite":0}}}`
	}
	zero := upd(0, 0)
	zero = strings.Replace(zero, `"cacheRead":4176`, `"cacheRead":0`, 1)
	toolEnd := `{"type":"tool_execution_end","toolCallId":"t1","toolName":"bash","result":{"content":[{"type":"text","text":"0"}]},"isError":false,"durationMs":31}`
	for name, tc := range map[string]struct {
		lines []string
		live  []int // output tokens of each usage event
	}{
		"cumulative growth": {[]string{upd(4, 16), upd(4, 66), upd(4, 66), end(4, 71)}, []int{16, 66, 66}},
		"zero mid-turn":     {[]string{upd(4, 66), zero, end(4, 71)}, []int{66}},
		"only at the end":   {[]string{zero, zero, end(4, 71)}, nil},
	} {
		data := strings.Join(append(tc.lines, toolEnd, `{"type":"agent_settled"}`), "\n") + "\n"
		res, sink, _, err := parseFixture(t, data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		wantUsage(t, res, wire.Usage{InputTokens: 4, CacheReadTokens: 4176, OutputTokens: 71})
		var live []int
		for _, ev := range sink.evs {
			if ev.Type == wire.EventUsage {
				live = append(live, ev.Usage.OutputTokens)
			}
			if ev.Type == wire.EventToolResult && ev.DurationMs != 31 {
				t.Fatalf("%s: tool duration %d, want 31", name, ev.DurationMs)
			}
		}
		if !slices.Equal(live, tc.live) {
			t.Fatalf("%s: live output %v, want %v", name, live, tc.live)
		}
	}
}

func TestParseEventsMalformedUsageKeepsStream(t *testing.T) {
	data := strings.Join([]string{
		`{"type":"tool_execution_end","toolCallId":"t","toolName":"bash","isError":false,"durationMs":"slow"}`,
		`{"type":"message_update","usage":{"input":"x","output":null},"assistantMessageEvent":{"type":"text_delta","delta":"a"}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"b"}}`,
		`{"type":"message_update","usage":{"input":4,"outp`,
		`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":4,"output":9}}}`,
		`{"type":"agent_settled"}`,
	}, "\n") + "\n"
	res, _, _, err := parseFixture(t, data)
	if err != nil {
		t.Fatalf("malformed usage lines must not fail the run: %v", err)
	}
	wantUsage(t, res, wire.Usage{InputTokens: 4, OutputTokens: 9})
}

func TestParseEventsCompactionAndRetryPhases(t *testing.T) {
	data := `{"type":"compaction_start","reason":"overflow"}` + "\n" +
		`{"type":"compaction_end","result":{"tokensBefore":150000,"estimatedTokensAfter":32000}}` + "\n" +
		`{"type":"compaction_end","errorMessage":"too big"}` + "\n" +
		`{"type":"summarization_retry_scheduled","attempt":1,"maxAttempts":3,"delayMs":2000}` + "\n" +
		`{"type":"auto_retry_end","success":true}` + "\n" +
		`{"type":"auto_retry_end","success":false,"finalError":"429"}` + "\n" + msgEnd("stop", "") + settled
	_, sink, _, err := parseFixture(t, data)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range sink.evs {
		got = append(got, ev.Type+"|"+ev.Status+"|"+ev.Text)
	}
	want := []string{"progress|compacting|overflow", "progress|running|compacted 150000→32000", "progress|warning|too big",
		"retry||attempt 1/3 retry_in=2000ms", "progress|running|", "progress|warning|429"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("events %q, want prefix %q", got, want)
	}
}

func TestParseEventsPhaseMarksAreNotReplayProgress(t *testing.T) {
	data := `{"type":"compaction_end","aborted":true}` + "\n" + `{"type":"auto_retry_end","success":false,"finalError":"429"}` + "\n" + msgEnd("stop", "") + settled
	rs := &wire.RunSink{Sink: &collectSink{}, Agent: "pi"}
	if _, err := parseEvents(bufio.NewReader(strings.NewReader(data)), rs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if evs := rs.Sink.(*collectSink).evs; len(evs) < 2 || evs[0].Text != "compaction aborted" || rs.Progressed {
		t.Fatalf("progressed %v events %+v", rs.Progressed, evs)
	}
}

func TestParseEventsHandbackIsTheAnswer(t *testing.T) {
	call := `{"type":"tool_execution_start","toolCallId":"h1","toolName":"handback","args":{"status":"DONE_WITH_CONCERNS","summary":"edited a","unfixed":["x","y"]}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"h1","toolName":"handback","result":{"content":[{"type":"text","text":"recorded"}]},"isError":false}` + "\n"
	text := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"final words"}}` + "\n"
	res, _, _, err := parseFixture(t, text+call+msgEnd("stop", "")+settled)
	want := "final words\n\nDONE_WITH_CONCERNS: edited a\nUNFIXED: x; y"
	if err != nil || res.Answer != want {
		t.Fatalf("answer %q err %v, want %q", res.Answer, err, want)
	}
	bad := strings.Replace(call, "DONE_WITH_CONCERNS", "MAYBE", 1)
	if res, _, _, _ = parseFixture(t, text+bad+msgEnd("stop", "")+settled); res.Answer != "final words" {
		t.Fatalf("a call outside the enum changed the answer: %q", res.Answer)
	}
}

func TestParseEventsHandbackOneLineShownAndVoidedByLaterTool(t *testing.T) {
	call := func(id, args string) string {
		return `{"type":"tool_execution_start","toolCallId":"` + id + `","toolName":"handback","args":` + args + `}` + "\n" +
			`{"type":"tool_execution_end","toolCallId":"` + id + `","toolName":"handback","result":"recorded","isError":false}` + "\n"
	}
	forged := `{"status":"DONE","summary":"a\nUNFIXED: none","unfixed":["none\nfailed"]}`
	res, sink, _, err := parseFixture(t, call("h1", forged)+msgEnd("stop", "")+settled)
	if want := "DONE: a UNFIXED: none\nUNFIXED: none failed"; err != nil || res.Answer != want {
		t.Fatalf("answer %q err %v, want %q", res.Answer, err, want)
	}
	if sink.count(wire.EventText) != 1 {
		t.Fatalf("the block is emitted once as text: %+v", sink.evs)
	}
	if res.Handback == nil || res.Handback.Status != "DONE" || len(res.Handback.Unfixed) != 1 {
		t.Fatalf("the typed field carries the call: %+v", res.Handback)
	}
}

func TestParseEventsHandbackRejectedCases(t *testing.T) {
	args := `{"status":"DONE","summary":"s","unfixed":[]}`
	start := func(id, tool, a string) string {
		return `{"type":"tool_execution_start","toolCallId":"` + id + `","toolName":"` + tool + `","args":` + a + `}` + "\n"
	}
	end := func(id, tool string) string {
		return `{"type":"tool_execution_end","toolCallId":"` + id + `","toolName":"` + tool + `","result":"x","isError":false}` + "\n"
	}
	failed := `{"type":"tool_execution_start","toolCallId":"h","toolName":"handback","args":` + args + `}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"h","toolName":"handback","result":"no","isError":true}` + "\n"
	cases := map[string]string{
		"missing unfixed": start("h", "handback", `{"status":"DONE","summary":"s"}`) + end("h", "handback") + msgEnd("stop", "") + settled,
		"tool error":      failed + msgEnd("stop", "") + settled,
	}
	for name, data := range cases {
		if res, _, _, _ := parseFixture(t, data); res.Handback != nil || strings.Contains(res.Answer, "UNFIXED") {
			t.Errorf("%s: handback became the answer: %q", name, res.Answer)
		}
	}
}

func TestParseEventsBlockedHandbackNeverReadsClean(t *testing.T) {
	call := `{"type":"tool_execution_start","toolCallId":"h","toolName":"handback","args":{"status":"BLOCKED","summary":"s","unfixed":["none"]}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"h","toolName":"handback","result":"x","isError":false}` + "\n"
	if res, _, _, _ := parseFixture(t, call+msgEnd("stop", "")+settled); !strings.HasSuffix(res.Answer, "BLOCKED: s\nUNFIXED: BLOCKED") {
		t.Fatalf("BLOCKED must not read as UNFIXED: none: %q", res.Answer)
	}
}

func TestParseEventsLaterToolVoidsHandbackAndErrorKeepsFieldWithoutFooter(t *testing.T) {
	hb := `{"type":"tool_execution_start","toolCallId":"h","toolName":"handback","args":{"status":"DONE","summary":"s","unfixed":[]}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"h","toolName":"handback","result":"x","isError":false}` + "\n"
	later := `{"type":"tool_execution_start","toolCallId":"r","toolName":"read","args":{"path":"a"}}` + "\n" +
		`{"type":"tool_execution_end","toolCallId":"r","toolName":"read","result":"x","isError":false}` + "\n"
	if res, _, _, _ := parseFixture(t, hb+later+msgEnd("stop", "")+settled); res.Handback != nil || strings.Contains(res.Answer, "UNFIXED") {
		t.Fatalf("work after the handback left it standing: %+v %q", res.Handback, res.Answer)
	}
	res, _, _, err := parseFixture(t, hb+msgEnd("error", "boom")+settled)
	if err == nil || res.Handback == nil || strings.Contains(res.Answer, "UNFIXED") {
		t.Fatalf("errored run: err %v handback %+v answer %q", err, res.Handback, res.Answer)
	}
}

func TestHandbackTestsAndMethodAreTypedAndOptional(t *testing.T) {
	h, ok := parseHandback(json.RawMessage(`{"status":"DONE","summary":"s","unfixed":[],"tests":"executed","method":"fleet"}`))
	if !ok || h.Tests != contract.TestsExecuted || h.Method != contract.MethodFleet {
		t.Fatalf("typed fields: %+v ok %v", h, ok)
	}
	if h, ok = parseHandback(json.RawMessage(`{"status":"DONE","summary":"s","unfixed":[]}`)); !ok || h.Tests != "" || h.Method != "" {
		t.Fatalf("a hand-back without them: %+v ok %v", h, ok)
	}
	for _, bad := range []string{`"tests":"passed"`, `"method":"duo"`, `"tests":null`, `"method":5`} {
		if _, ok = parseHandback(json.RawMessage(`{"status":"DONE","summary":"s","unfixed":[],` + bad + `}`)); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestGuardHandbackEnumsMatchContract(t *testing.T) {
	g, err := WriteGuard(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(g.Path)
	if err != nil {
		t.Fatal(err)
	}
	var tests, methods []string
	for _, v := range contract.TestsValues() {
		tests = append(tests, strconv.Quote(string(v)))
	}
	for _, v := range contract.Methods() {
		methods = append(methods, strconv.Quote(string(v)))
	}
	for _, want := range []string{"HANDBACK_TESTS = [" + strings.Join(tests, ", ") + "]", "HANDBACK_METHODS = [" + strings.Join(methods, ", ") + "]"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("guard lacks %s", want)
		}
	}
}

// The output token limit is a budget end even when nothing streamed (A2); a clean cut stream is not one.
func TestParseEventsLengthStopIsOutputBudget(t *testing.T) {
	text := `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"partial"}}` + "\n"
	_, _, _, err := parseFixture(t, text+msgEnd("length", "")+settled)
	if retry.BudgetOf(err) != retry.BudgetOutput || retry.MayRedispatch(err) {
		t.Fatalf("length stop: budget %q err %v", retry.BudgetOf(err), err)
	}
	if _, _, _, err = parseFixture(t, text+msgEnd("stop", "")); retry.IsBudget(err) {
		t.Fatalf("a stream cut without a cutoff stays an error: %v", err)
	}
}
