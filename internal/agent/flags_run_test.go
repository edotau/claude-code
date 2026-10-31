package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeCLI installs a shell stand-in for name that records its argv (one per line) to the returned file, then runs body.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(t.TempDir(), name+".argv")
	script := "#!/bin/sh\n" + body + "\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argv + "'.tmp\n"
	if err := os.WriteFile(filepath.Join(paths.BinDir(), name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return argv
}

func readArgv(t *testing.T, argv string) []string {
	t.Helper()
	b, err := os.ReadFile(argv + ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// followedBy reports whether args holds flag immediately followed by val.
func followedBy(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

const claudeStructured = `{"type":"result","subtype":"success","is_error":false,"result":"{\"a\":1}","session_id":"s","total_cost_usd":0.25,` +
	`"structured_output":{"a":1},"usage":{"input_tokens":3,"output_tokens":2}}`

// claude gets the three flags verbatim and its result's cost and structured output reach the Result.
func TestClaudeRunMapsSystemPromptSchemaAndResult(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	registry(t, srv)
	argv := fakeCLI(t, "claude", "cat >/dev/null; printf '%s\\n' '"+claudeStructured+"'")
	const schema = `{"type":"object","properties":{"a":{"type":"integer"}}}`
	res, err := claudeRunner{}.Run(context.Background(), RunOptions{Prompt: "task", Provider: "tanth", Tools: true,
		SystemPrompt: "SYS", AppendSystemPrompt: "APP", OutputSchema: schema, Env: []string{"HARNESS_TEST_X=1"}, Launcher: testLauncher{}}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	args := readArgv(t, argv)
	for flag, val := range map[string]string{"--system-prompt": "SYS", "--append-system-prompt": "APP", "--json-schema": schema} {
		if !followedBy(args, flag, val) {
			t.Errorf("argv %q lacks %s %q", args, flag, val)
		}
	}
	if res.Usage.CostUSD != 0.25 || res.Usage.InputTokens != 3 {
		t.Errorf("usage = %+v, want cost 0.25", res.Usage)
	}
	if string(res.Structured) != `{"a":1}` {
		t.Errorf("structured = %s", res.Structured)
	}
}

type failSink struct{}

func (failSink) Emit(Event) error { return errors.New("consumer gone") }

// A sink that fails mid-stream kills the child at once instead of draining it until it exits on its own.
func TestClaudeRunKillsChildOnSinkError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	registry(t, srv)
	const text = `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`
	fakeCLI(t, "claude", "cat >/dev/null; printf '%s\\n' '"+text+"'; sleep 30")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err := claudeRunner{}.Run(ctx, RunOptions{Prompt: "task", Provider: "tanth", Launcher: testLauncher{}}, failSink{})
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Run took %s: the child was drained, not killed", took)
	}
	if err == nil || !strings.Contains(err.Error(), "consumer gone") {
		t.Errorf("err = %v, want the sink error", err)
	}
}

const codexFixture = `{"type":"thread.started","thread_id":"th-1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls","status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"final answer"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":7}}
`

type collectSink struct{ evs []Event }

func (c *collectSink) Emit(e Event) error { c.evs = append(c.evs, e); return nil }

func TestParseCodexStream(t *testing.T) {
	var sink collectSink
	res, err := parseCodexStream(newBR(codexFixture), &sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID != "th-1" || res.Answer != "final answer" {
		t.Errorf("session=%q answer=%q", res.SessionID, res.Answer)
	}
	if want := (Usage{InputTokens: 40, CacheReadTokens: 60, OutputTokens: 7}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v (input = input_tokens - cached_input_tokens)", res.Usage, want)
	}
	var kinds []string
	for _, e := range sink.evs {
		kinds = append(kinds, e.Type)
	}
	if want := []string{EventToolUse, EventToolResult, EventText, EventText}; !slices.Equal(kinds, want) {
		t.Fatalf("events = %v, want %v", kinds, want)
	}
	if e := sink.evs[0]; e.Tool == nil || e.Tool.ID != "i1" || e.Text != "ls" {
		t.Errorf("tool_use = %+v", e)
	}
	if e := sink.evs[1]; e.Tool == nil || e.Tool.ID != "i1" || e.Status != "completed" {
		t.Errorf("tool_result = %+v", e)
	}
	if !sink.evs[3].Sep || sink.evs[2].Sep {
		t.Errorf("second agent message must start a new turn: %+v", sink.evs[2:])
	}
}

func TestParseCodexStreamTurnFailed(t *testing.T) {
	in := `{"type":"thread.started","thread_id":"th-2"}` + "\n" + `{"type":"turn.failed","error":{"message":"boom"}}` + "\n"
	var sink collectSink
	res, err := parseCodexStream(newBR(in), &sink, nil)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	if res.SessionID != "th-2" || len(sink.evs) != 1 || sink.evs[0].Type != EventError || sink.evs[0].Text != "boom" {
		t.Errorf("res=%+v events=%+v", res, sink.evs)
	}
}

func TestCodexStrictRecursive(t *testing.T) {
	const schema = `{"type":"object","required":["a"],"properties":{"a":{"type":"object","properties":{"x":{"type":"string"},"y":{"type":"integer"}}},` +
		`"l":{"type":"array","items":{"type":"object","properties":{"z":{"type":"string"}}}}}}`
	file, err := codexSchemaFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(file)
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("schema file mode = %v err = %v, want 0600", fi.Mode(), err)
	}
	b, _ := os.ReadFile(file)
	var got struct {
		Required   []string
		AP         *bool `json:"additionalProperties"`
		Properties struct {
			A struct {
				Required []string
				AP       *bool `json:"additionalProperties"`
			}
			L struct {
				Items struct {
					Required []string
					AP       *bool `json:"additionalProperties"`
				}
			}
		}
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	isFalse := func(p *bool) bool { return p != nil && !*p }
	if !slices.Equal(got.Required, []string{"a", "l"}) || !isFalse(got.AP) ||
		!slices.Equal(got.Properties.A.Required, []string{"x", "y"}) || !isFalse(got.Properties.A.AP) ||
		!slices.Equal(got.Properties.L.Items.Required, []string{"z"}) || !isFalse(got.Properties.L.Items.AP) {
		t.Errorf("not strict at every object level: %s", b)
	}
	if _, err := codexSchemaFile(`{not json`); err == nil {
		t.Error("an unparseable schema must fail")
	}
}

// codex gets -c instructions / developer_instructions and a strict --output-schema file that is gone after the run.
func TestCodexRunFlagsAndSchemaFileCleanup(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	registry(t, srv)
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	capture := filepath.Join(t.TempDir(), "schema.cap")
	// help prints no --json: codexHasJSON is cached per process and must not flip for other tests
	body := `cat >/dev/null
if [ "$1" = exec ] && [ "$2" = --help ]; then exit 0; fi
prev=
for a in "$@"; do
  if [ "$prev" = --output-schema ]; then cp "$a" '` + capture + `'; printf '%s\n' "$a" > '` + capture + `.path'; ls -l "$a" | cut -c1-10 > '` + capture + `.mode'; fi
  prev=$a
done
echo '{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}'`
	argv := fakeCLI(t, "codex", body)
	_, err := newVendorRunner("codex").Run(context.Background(), RunOptions{Prompt: "task", Provider: "toai", Tools: true,
		SystemPrompt: `be "terse"`, AppendSystemPrompt: "APP", OutputSchema: `{"type":"object","properties":{"a":{"type":"string"}}}`}, Discard)
	if err != nil {
		t.Fatal(err)
	}
	args := readArgv(t, argv)
	if !followedBy(args, "-c", `instructions="be \"terse\""`) || !followedBy(args, "-c", `developer_instructions="APP"`) {
		t.Errorf("argv %q lacks the -c instructions overrides", args)
	}
	if args[0] != "exec" || !slices.Contains(args, "--output-schema") {
		t.Errorf("argv %q: want exec first and --output-schema", args)
	}
	schema, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(schema), `"additionalProperties":false`) || !strings.Contains(string(schema), `"required":["a"]`) {
		t.Errorf("schema handed to codex = %s", schema)
	}
	if mode, _ := os.ReadFile(capture + ".mode"); strings.TrimSpace(string(mode)) != "-rw-------" {
		t.Errorf("schema file mode = %q, want -rw-------", mode)
	}
	path, _ := os.ReadFile(capture + ".path")
	if _, err := os.Stat(strings.TrimSpace(string(path))); !os.IsNotExist(err) {
		t.Errorf("schema temp file %s survived the run (err=%v)", path, err)
	}
}

// opencode's system prompt becomes a merged harness-system agent selected with --agent; other config keys survive.
func TestOpencodeSystemPromptMergesConfig(t *testing.T) {
	env := []string{"A=1", `OPENCODE_CONFIG_CONTENT={"model":"m","agent":{"mine":{"prompt":"keep"}}}`}
	env, argv := opencodeSystemPrompt(env, []string{"run", "hello"}, "SYS")
	if !slices.Equal(argv, []string{"run", "--agent", "harness-system", "hello"}) {
		t.Errorf("argv = %q", argv)
	}
	var cfg struct {
		Model string
		Agent map[string]struct{ Mode, Prompt string }
	}
	var n int
	for _, kv := range env {
		if body, ok := strings.CutPrefix(kv, "OPENCODE_CONFIG_CONTENT="); ok {
			n++
			if err := json.Unmarshal([]byte(body), &cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n != 1 || cfg.Model != "m" || cfg.Agent["mine"].Prompt != "keep" ||
		cfg.Agent["harness-system"].Prompt != "SYS" || cfg.Agent["harness-system"].Mode != "primary" || env[0] != "A=1" {
		t.Errorf("env = %q", env)
	}
	// no pre-existing config: the variable is added
	env, _ = opencodeSystemPrompt(nil, []string{"run"}, "S2")
	if len(env) != 1 || !strings.Contains(env[0], `"harness-system"`) {
		t.Errorf("fresh env = %q", env)
	}
}
