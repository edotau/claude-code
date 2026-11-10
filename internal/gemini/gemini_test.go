package gemini

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
)

// agyArgv maps Autonomous onto "--mode auto"; --dangerously-skip-permissions rides on Tools alone, so
// an autonomous-but-tool-less run must never carry it (agy must never auto-grant write access it wasn't
// asked for).
func TestAgyArgvAutonomous(t *testing.T) {
	cases := []struct {
		name               string
		tools, autonomous  bool
		wantMode, wantSkip bool
	}{
		{"neither", false, false, false, false},
		{"tools only", true, false, false, true},
		{"autonomous only", false, true, true, false},
		{"both", true, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", Tools: c.tools, Autonomous: c.autonomous})
			hasMode := strings.Contains(strings.Join(argv, " "), "--mode auto")
			hasSkip := slices.Contains(argv, "--dangerously-skip-permissions")
			if hasMode != c.wantMode || hasSkip != c.wantSkip {
				t.Errorf("argv = %q, mode=%v skip=%v, want mode=%v skip=%v", argv, hasMode, hasSkip, c.wantMode, c.wantSkip)
			}
		})
	}
}

// AgyRequest.SessionID rides as "--conversation <id>"; an empty SessionID must never emit the flag
// (agy has no bare --conversation value to resume against).
func TestAgyArgvConversation(t *testing.T) {
	cases := []struct{ name, session string }{{"unset", ""}, {"set", "conv-123"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", SessionID: c.session})
			idx := slices.Index(argv, "--conversation")
			if has := idx >= 0; has != (c.session != "") {
				t.Fatalf("argv = %q, --conversation present = %v, want %v", argv, has, c.session != "")
			}
			if c.session != "" && argv[idx+1] != c.session {
				t.Errorf("argv = %q, --conversation value = %q, want %q", argv, argv[idx+1], c.session)
			}
		})
	}
}

func TestAgyArgvDebugFile(t *testing.T) {
	if slices.Contains(agyArgv(agyRequest{Model: "m"}), "--log-file") {
		t.Fatal("--log-file without DebugFile")
	}
	argv := agyArgv(agyRequest{Model: "m", DebugFile: "/tmp/l"})
	if i := slices.Index(argv, "--log-file"); i < 0 || argv[i+1] != "/tmp/l" {
		t.Fatalf("argv = %v", argv)
	}
}

// Deltas already parsed survive a later typed-decode failure.
func TestParseAgyStreamDecodeErrorKeepsText(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "hi" {
		t.Fatalf("res = %+v, err = %v; want the decode error with the text kept", res, err)
	}
}

type agySinkRec struct{ types []string }

func (s *agySinkRec) Emit(ev wire.Event) error { s.types = append(s.types, ev.Type); return nil }

// A tool step marks progress (one progress mark per step, never a tool_use) before a later decode failure; planning steps do not.
func TestParseAgyToolStepMarksProgress(t *testing.T) {
	run := func(stepType string) []string {
		stream := `{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"DONE"}}
{"event":123}`
		sink := &agySinkRec{}
		if _, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), sink, nil); err == nil {
			t.Fatal("want the decode error")
		}
		if slices.Contains(sink.types, wire.EventToolUse) {
			t.Errorf("%s step: placeholder tool_use reached the sink: %v", stepType, sink.types)
		}
		return slices.DeleteFunc(sink.types, func(ty string) bool { return ty != wire.EventProgress })
	}
	if got := run("tool"); len(got) != 1 {
		t.Errorf("tool step: %d progress marks, want 1", len(got))
	}
	if got := run("planner"); len(got) != 0 {
		t.Errorf("planning step: %d progress marks, want 0", len(got))
	}
}

// A result carrying only structured_output (no response text) is a success and keeps the structure.
func TestParseAgyStructuredOnlySucceeds(t *testing.T) {
	stream := `{"event":"result","result":{"status":"SUCCESS","structured_output":{"k":1}}}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err != nil || string(res.Structured) != `{"k":1}` {
		t.Fatalf("res = %+v, err = %v; want no error and Structured kept", res, err)
	}
}

// A result.response-only answer survives a decode error that follows the result event.
func TestParseAgyResponseSurvivesLaterDecodeError(t *testing.T) {
	stream := `{"event":"result","result":{"status":"SUCCESS","response":"R"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "R" {
		t.Fatalf("res = %+v, err = %v; want the decode error with Answer R", res, err)
	}
}

// A tool step a pre-tool hook refused ends in ERROR; the answer that follows is a success, not a partial result.
func TestParseAgyRefusedToolStepIsNotPartial(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR"}}
{"event":"result","result":{"status":"SUCCESS","response":"blocked as asked"}}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err != nil || res.Answer != "blocked as asked" {
		t.Fatalf("res.Answer = %q, err = %v; want the answer and no partial error", res.Answer, err)
	}
}

// agy refuses --effort beside an effort-suffixed id ("conflicts with --effort"); a bare id keeps the flag.
func TestAgyArgvEffortSuffixedModelDropsEffort(t *testing.T) {
	for model, want := range map[string]bool{"gemini-3.8-flash-high": false, "gemini-3.8-flash": true} {
		argv := agyArgv(agyRequest{Model: model, Effort: "medium"})
		if got := slices.Contains(argv, "--effort"); got != want {
			t.Errorf("%s: --effort present = %v, want %v (%v)", model, got, want, argv)
		}
	}
}

// A non-Gemini model (and whatever the pin is) must never leave the agy gateway model empty.
func TestAgyGatewayArgNonGeminiNeverEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base, arg := agyGatewayArg("claude-opus-4-1")
	if base == "" || arg == "" {
		t.Fatalf("agyGatewayArg(non-Gemini) = (%q, %q), want a non-empty Gemini base", base, arg)
	}
}

// An unreadable login marker is an error, not "not signed in", and the probe reports it.
func TestGeminiLoginUnreadableMarkerIsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if ok, err := geminiLoginProbe(); ok || err == nil {
		t.Fatalf("geminiLoginProbe() = (%v, %v), want (false, error)", ok, err)
	}
	_, _, err := geminiLogin("gemini-3.8-flash", nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("geminiLogin err = %v, want an unreadable-marker error", err)
	}
}

// A turn that errors before any result event must not reset the usage baseline: the next delta stays a delta.
func TestAgyLiveErroredTurnKeepsUsageBaseline(t *testing.T) {
	result := func(in int) string {
		return `{"event":"result","result":{"status":"SUCCESS","response":"r","usage":{"input_tokens":` + strconv.Itoa(in) + `}}}`
	}
	b := &agyLive{}
	parse := func(stream string) (wire.Result, error) {
		return b.Parse(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	}
	if _, err := parse(result(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(`{"event":123}`); err == nil {
		t.Fatal("want the decode error on the errored turn")
	}
	res, err := parse(result(1500))
	if err != nil || res.Usage.InputTokens != 500 {
		t.Fatalf("turn 3 InputTokens = %d, err = %v; want the 500 delta", res.Usage.InputTokens, err)
	}
}

func TestGeminiGatewayInteractiveModelMatchesAdvertised(t *testing.T) {
	t.Cleanup(agent.StubRouter("http://127.0.0.1:19876"))
	origMint := agent.MintSession
	agent.MintSession = func(context.Context, string, int) (string, error) { return "test-session", nil }
	t.Cleanup(func() { agent.MintSession = origMint })
	p := &providers.Provider{Name: t.Name(), Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "pat"}}
	env, argv, err := geminiGateway(context.Background(), p, "gemini-3.8-flash-high", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "AGY_LLM_GATEWAY_MODELS=gemini-3.8-flash") {
		t.Fatalf("gateway must advertise the base model: %v", env)
	}
	want := []string{"--model", "gemini-3.8-flash", "--effort", "high"}
	if !slices.Equal(argv, want) {
		t.Fatalf("interactive gateway argv = %v, want %v to match the advertised model", argv, want)
	}
}

// On a Databricks gateway row agy lists only the base id, so the effort suffix must ride --effort, not --model.
func TestAgyGatewayModelIsBaseWithEffort(t *testing.T) {
	gw := providers.Target{Provider: &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "pat"}},
		Model: "gemini-3.8-flash-high"}
	req := agent.RunOptions{Model: "gemini-3.8-flash-high"}
	if m := agyModel(req, gw); strings.HasSuffix(m, "-high") || m == "" {
		t.Fatalf("gateway --model = %q, want the base id", m)
	}
	if e := agyGatewayEffort(req, gw); e != "high" {
		t.Fatalf("gateway --effort = %q, want high", e)
	}
}

// A cached router token older than gatewayTokenMaxAge is re-minted; a fresh one is reused.
func TestGatewayTokenRemintsWhenStale(t *testing.T) {
	origMint := agent.MintSession
	mints := 0
	agent.MintSession = func(context.Context, string, int) (string, error) {
		mints++
		return "tok" + strconv.Itoa(mints), nil
	}
	t.Cleanup(func() { agent.MintSession = origMint })

	const base, profile = "http://router.test-stale", "p"
	ctx := context.Background()
	first, err := gatewayToken(ctx, base, profile)
	if err != nil || first != "tok1" {
		t.Fatalf("first mint = %q, %v", first, err)
	}
	if again, _ := gatewayToken(ctx, base, profile); again != "tok1" || mints != 1 {
		t.Fatalf("fresh token not reused: %q after %d mints", again, mints)
	}
	gatewayMu.Lock()
	e := gatewayTokens[base+"|"+profile]
	gatewayMu.Unlock()
	e.mu.Lock()
	e.minted = time.Now().Add(-2 * gatewayTokenMaxAge)
	e.mu.Unlock()
	got, err := gatewayToken(ctx, base, profile)
	if err != nil || got != "tok2" {
		t.Fatalf("stale token not re-minted: %q, %v", got, err)
	}
}
