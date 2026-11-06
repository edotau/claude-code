package gemini

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	"github.com/edotau/claude-code/internal/agent"
)

// D6: agy's model is flag > process env > the GEMINI_MODEL vault pin > agy's default.
func TestAgyModelPrecedence(t *testing.T) {
	vaulttest.Use(t)
	for _, k := range modelEnvKeys {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := providers.Select(reg, "")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != AgyDefaultModel {
		t.Errorf("nothing set = %q, want the default %q", got, AgyDefaultModel)
	}
	vaulttest.SetRecord(t, "gemini", "model", "gemini-pinned")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-pinned" {
		t.Errorf("vault pin = %q, want gemini-pinned", got)
	}
	t.Setenv("GEMINI_MODEL", "gemini-env")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-env" {
		t.Errorf("process env = %q, want it to beat the pin", got)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], "gemini-flag"); got != "gemini-flag" {
		t.Errorf("flag = %q, want it to beat env and pin", got)
	}
}

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

// A bridge dispatch on the agy backend leaves a 0600 <id>.log (record.Log) holding the redacted agy output.
func TestBridgeLogCapturesRedactedAgyOutput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	script := "cat > /dev/null\n" +
		"echo 'agy-stderr-line' >&2\n" +
		"echo 'export AWS_SECRET=AKIAIOSFODNN7EXAMPLE' >&2\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"bridge-answer","num_turns":1}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)

	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil || res.Answer != "bridge-answer" {
		t.Fatalf("Run = (%q, %v), want (bridge-answer, nil)", res.Answer, err)
	}
	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	logPath := recs[0].Log
	if logPath == "" {
		t.Fatal("record.Log is empty: the bridge dispatch did not record a log path")
	}
	if logPath != recs[0].LogPath() {
		t.Errorf("record.Log = %q, want %q", logPath, recs[0].LogPath())
	}
	checkBridgeLogFile(t, logPath)
}

// checkBridgeLogFile requires a 0600 log holding the agy output with the secret redacted and no duplicates.
func checkBridgeLogFile(t *testing.T, logPath string) {
	t.Helper()
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("log file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 0600", perm)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"agy-stderr-line", "bridge-answer", "AWS_SECRET=REDACTED"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("log leaks an unredacted secret:\n%s", got)
	}
	if n := strings.Count(got, "agy-stderr-line"); n != 1 {
		t.Errorf("stderr line appears %d times, want exactly once:\n%s", n, got)
	}
}

// Deltas already parsed survive a later typed-decode failure.
func TestParseAgyStreamDecodeErrorKeepsText(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), agent.Discard, nil)
	if err == nil || res.Answer != "hi" {
		t.Fatalf("res = %+v, err = %v; want the decode error with the text kept", res, err)
	}
}

type agySinkRec struct{ types []string }

func (s *agySinkRec) Emit(ev agent.Event) error { s.types = append(s.types, ev.Type); return nil }

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
		if slices.Contains(sink.types, agent.EventToolUse) {
			t.Errorf("%s step: placeholder tool_use reached the sink: %v", stepType, sink.types)
		}
		return slices.DeleteFunc(sink.types, func(ty string) bool { return ty != agent.EventProgress })
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
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), agent.Discard, nil)
	if err != nil || string(res.Structured) != `{"k":1}` {
		t.Fatalf("res = %+v, err = %v; want no error and Structured kept", res, err)
	}
}

// A result.response-only answer survives a decode error that follows the result event.
func TestParseAgyResponseSurvivesLaterDecodeError(t *testing.T) {
	stream := `{"event":"result","result":{"status":"SUCCESS","response":"R"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), agent.Discard, nil)
	if err == nil || res.Answer != "R" {
		t.Fatalf("res = %+v, err = %v; want the decode error with Answer R", res, err)
	}
}

// A tool step a pre-tool hook refused ends in ERROR; the answer that follows is a success, not a partial result.
func TestParseAgyRefusedToolStepIsNotPartial(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR"}}
{"event":"result","result":{"status":"SUCCESS","response":"blocked as asked"}}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), agent.Discard, nil)
	if err != nil || res.Answer != "blocked as asked" {
		t.Fatalf("res.Answer = %q, err = %v; want the answer and no partial error", res.Answer, err)
	}
}

// TestResolveModel: explicit arg wins; default when nothing set; GEMINI_MODEL fallback;
// CLAUDE_CODE_GEMINI_MODEL beats GEMINI_MODEL.
func TestResolveModel(t *testing.T) {
	t.Setenv("CLAUDE_CODE_GEMINI_MODEL", "")
	t.Setenv("GEMINI_MODEL", "")

	if got := ResolveModel("explicit-model", modelEnvKeys, "def-model"); got != "explicit-model" {
		t.Errorf("explicit arg: got %q", got)
	}
	if got := ResolveModel("", modelEnvKeys, "def-model"); got != "def-model" {
		t.Errorf("default: got %q, want %q", got, "def-model")
	}

	t.Setenv("GEMINI_MODEL", "gemini-env-model")
	if got := ResolveModel("", modelEnvKeys, "def-model"); got != "gemini-env-model" {
		t.Errorf("env fallback: got %q", got)
	}

	t.Setenv("CLAUDE_CODE_GEMINI_MODEL", "gemini-primary-model")
	if got := ResolveModel("", modelEnvKeys, "def-model"); got != "gemini-primary-model" {
		t.Errorf("env precedence: got %q", got)
	}
}

// TestBridgeRunAgyModelFilled: the agy bridge envelope carries the resolved model, not an empty field.
func TestBridgeRunAgyModelFilled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t", Files: []string{"a.go"}}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if res.Model != AgyDefaultModel || res.Provider != "agy" {
		t.Errorf("model/provider = %q/%q, want %q/agy", res.Model, res.Provider, AgyDefaultModel)
	}
}

// write creates path (and its parent dirs) with body, mode 0644.
func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
