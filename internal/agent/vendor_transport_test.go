package agent

import (
	"strings"
	"testing"
	"time"
)

func tickClock(t *testing.T, ms ...int) {
	t.Helper()
	base, i, old := time.Unix(1000, 0), 0, clock
	clock = func() time.Time { d := time.Duration(ms[i]) * time.Millisecond; i++; return base.Add(d) }
	t.Cleanup(func() { clock = old })
}

func TestAgyAndOpenCodeStreamTiming(t *testing.T) {
	agy := `{"event":"init","conversation_id":"c1"}` + "\n" +
		`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi"}}` + "\n" +
		`{"event":"result","result":{"status":"SUCCESS","response":"hi"}}` + "\n"
	oc := `{"type":"text","sessionID":"s1","part":{"text":"hi"}}` + "\n" +
		`{"type":"step_finish","sessionID":"s1","part":{"tokens":{"output":1}}}` + "\n" +
		`{"type":"text","sessionID":"s1","part":{"text":"!"}}` + "\n"
	tickClock(t, 0, 100, 150, 950) // start, then one tick per agy event
	res, err := parseAgyStream(newBR(agy), NewTextSink(&strings.Builder{}), nil)
	if err != nil || res.Answer != "hi" {
		t.Fatalf("agy res=%+v err=%v", res, err)
	}
	if tr := res.Transport; tr == nil || tr.FirstEventMs != 100 || tr.MaxGapMs != 800 || tr.Retries != 0 || tr.RetryStatuses != nil {
		t.Fatalf("agy transport = %+v", tr)
	}
	tickClock(t, 0, 200, 300, 400)
	res, err = parseOpenCodeStream(newBR(oc), NewTextSink(&strings.Builder{}), nil)
	if err != nil || res.Answer != "hi\n!" {
		t.Fatalf("opencode res=%+v err=%v", res, err)
	}
	if tr := res.Transport; tr == nil || tr.FirstEventMs != 200 || tr.MaxGapMs != 100 || tr.Retries != 0 || tr.RetryStatuses != nil {
		t.Fatalf("opencode transport = %+v", tr)
	}
	tickClock(t, 0, 50, 70) // the error return path keeps timing
	bad := `{"event":"init"}` + "\n" + `{"event":"result","result":{"status":"FAILED"}}` + "\n"
	res, err = parseAgyStream(newBR(bad), NewTextSink(&strings.Builder{}), nil)
	if err == nil || res.Transport == nil || res.Transport.FirstEventMs != 50 || res.Transport.MaxGapMs != 20 {
		t.Fatalf("agy error transport = %+v err=%v", res.Transport, err)
	}
}

func TestVendorIsolationCaps(t *testing.T) {
	for name, want := range map[string][3]Level{
		"codex": {Native, Native, None}, "opencode": {None, Native, None}, "gemini": {None, None, Native}} {
		c := newVendorRunner(name).Caps()
		if got := [3]Level{c.NoPersist, c.SafeMode, c.DebugFile}; got != want {
			t.Errorf("%s caps = %v, want %v", name, got, want)
		}
	}
}
