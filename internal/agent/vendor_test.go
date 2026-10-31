package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
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
		c := newCLIRunner(name).Caps()
		if got := [3]Level{c.NoPersist, c.SafeMode, c.DebugFile}; got != want {
			t.Errorf("%s caps = %v, want %v", name, got, want)
		}
	}
}

// vendorTargetBase honors the request's model tier; ResolveVendorTarget always returns the session's opus slot.
func TestVendorTargetBaseDiffersFromResolveTargetOnTier(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	vaulttest.Use(t)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := schemaFor("codex").ResolveTarget(reg, sel, false)
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	bt, _, _, _, err := schemaFor("codex").targetBase(RunOptions{Prompt: "p", Model: "haiku"})
	if err != nil {
		t.Fatalf("vendorTargetBase: %v", err)
	}
	if rt.Model == bt.Model {
		t.Errorf("expected the tier to differ, both %q", rt.Model)
	}
	if want := bt.Provider.Models["haiku"]; bt.Model != want {
		t.Errorf("base model = %q, want the haiku slot %q", bt.Model, want)
	}
}
