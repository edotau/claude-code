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
	res, err := claudeAgent.Run(context.Background(), RunOptions{Prompt: "task", Provider: "tanth", Tools: true,
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
	_, err := claudeAgent.Run(ctx, RunOptions{Prompt: "task", Provider: "tanth", Launcher: testLauncher{}}, failSink{})
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
	_, err := newCLIEngine("codex").Run(context.Background(), RunOptions{Prompt: "task", Provider: "toai", Tools: true,
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
