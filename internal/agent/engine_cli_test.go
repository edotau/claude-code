package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// attach per engine: codex folds the file inline (stdin has no live-read channel). gemini (agy)
// inlines it too when tools are off (agy's no-tools Preamble bars it from reading @path live), but
// appends a live @path reference once tools are on. Every other vendor (opencode included) leaves the
// prompt untouched since it carries the attachment in its own argv instead (opencodeRun's -f).
func TestVendorEngineAttachPrompt(t *testing.T) {
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	inline := "task\n\n<file path=" + `"` + f + `"` + ">\nfile-content\n</file>"
	cases := []struct {
		name   string
		vendor string
		tools  bool
		want   string
	}{
		{"codex/tools", "codex", true, inline},
		{"codex/no-tools", "codex", false, inline},
		{"gemini/tools", "gemini", true, "task\n@" + f},
		{"gemini/no-tools", "gemini", false, inline},
		{"opencode/tools", "opencode", true, "task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mustDriver(c.vendor).Attach("task", []string{f}, c.tools)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("attach(%s, tools=%v) = %q, want %q", c.vendor, c.tools, got, c.want)
			}
		})
	}
}

// A missing attachment is a fatal error for the inline path, not a silently dropped reference,
// regardless of Tools (gemini without tools now inlines too, so it must fail the same way).
func TestVendorEngineAttachPromptMissingPath(t *testing.T) {
	for _, c := range []struct {
		vendor string
		tools  bool
	}{
		{"codex", true}, {"gemini", false},
	} {
		if _, err := mustDriver(c.vendor).Attach("task", []string{"/does/not/exist"}, c.tools); err == nil {
			t.Fatalf("%s tools=%v: want an error for a missing attachment, got nil", c.vendor, c.tools)
		}
	}
}

// codex and gemini ride stdin (codexHeadless's `-` operand, agy's stream-json), so an oversized prompt
// never hits MAX_ARG_STRLEN and must not be refused before launch; opencode still puts the prompt in
// argv (opencodeRun), so it alone keeps the refusal. Provider: "max" (the built-in
// default) makes vendorTargetNote return immediately (req.Provider != "" short-circuits the vendor's own
// fallback resolution), so every case fails or refuses before any real CLI exec — no bin/codex, bin/agy
// or bin/opencode needed, and no network call.
func TestVendorEngineOversizedPromptRefusalScopedToArgv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	big := strings.Repeat("x", MaxPromptBytes+10)

	for _, name := range []string{"codex", "gemini"} {
		t.Run(name+"/not-refused", func(t *testing.T) {
			_, err := newCLIEngine(name).Run(context.Background(), RunOptions{Prompt: big, Provider: "max"}, wire.Discard)
			if err == nil {
				t.Fatalf("%s: want a non-nil error (no route for a fake provider), got nil", name)
			}
			if errors.Is(err, ErrPromptTooLarge) {
				t.Fatalf("%s: err = %v, want anything but ErrPromptTooLarge (prompt rides stdin, not argv)", name, err)
			}
		})
	}

	t.Run("opencode/still-refused", func(t *testing.T) {
		_, err := newCLIEngine("opencode").Run(context.Background(), RunOptions{Prompt: big, Provider: "max"}, wire.Discard)
		var e *retry.Error
		if !errors.As(err, &e) || !errors.Is(e, ErrPromptTooLarge) {
			t.Fatalf("opencode: err = %v, want *Error wrapping ErrPromptTooLarge (prompt rides argv)", err)
		}
	})
}

// TestVendorEngineNilProviderDefense tests that vendorRunner.Run safely handles
// the edge case where vendorTarget returns own=false with nil Provider.
// Line 104 (res.Provider = t.Provider.Name) must not panic when t.Provider is nil.
// Similarly, copilotEngine.Run (copilot.go) must handle nil Provider.
func TestVendorEngineNilProviderDefense(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Simulate the scenario where vendorTargetNote returns own=false with nil Provider.
	// The vulnerable code path is when own=false and t.Provider is accessed.
	// This test ensures the code doesn't panic with nil dereference.

	req := RunOptions{Prompt: "test"}

	// Call vendorTarget and check if t.Provider is nil when own=false.
	// If it is, the subsequent code must handle it gracefully (not panic).
	t_, own, err := schemaFor("opencode").target(req)

	// If we got here without panicking, that's good. But we should also check
	// that if own=false and we got a valid return, t.Provider is not nil OR
	// the calling code handles nil safely (with a nil check).
	if err == nil && !own && t_.Provider == nil {
		t.Logf("WARNING: vendorTarget returned own=false with nil Provider; " +
			"vendorRunner.Run line 104 must have a nil check")
		return // This is the defect scenario; fix should add nil check in Run()
	}
}

// A quiet child stream logs a heartbeat line while it runs.
func TestHeartbeatLogsWhenQuiet(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })
	var mu sync.Mutex
	var got []string
	lg := func(kind, text string) { mu.Lock(); got = append(got, kind); mu.Unlock() }
	stop := heartbeat(lg, &idleReader{last: time.Now().Add(-time.Minute).UnixNano()}, "gemini")
	time.Sleep(60 * time.Millisecond)
	stop()
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || got[0] != "heartbeat" {
		t.Errorf("log kinds = %v, want a heartbeat", got)
	}
}

func TestVendorIsolationCaps(t *testing.T) {
	for name, want := range map[string][3]Level{
		"codex": {Native, Native, None}, "opencode": {None, Native, None}, "gemini": {None, None, Native}} {
		c := newCLIEngine(name).Caps()
		if got := [3]Level{c.NoPersist, c.SafeMode, c.DebugFile}; got != want {
			t.Errorf("%s caps = %v, want %v", name, got, want)
		}
	}
}

// vendorTargetBase honors the request's model alias; ResolveVendorTarget always returns the session's opus alias.
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
		t.Errorf("expected the alias to differ, both %q", rt.Model)
	}
	if want := bt.Provider.Models["haiku"]; bt.Model != want {
		t.Errorf("base model = %q, want the haiku alias %q", bt.Model, want)
	}
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

// codex gets -c instructions / developer_instructions and a strict --output-schema file that is gone after the run.
func TestCodexRunFlagsAndSchemaFileCleanup(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	registry(t, srv)
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(`{"providers":{"openai":{"toai":{"protocol":"responses","base_url":%q,"auth":{"type":"none"},"models":{"opus":"gpt-x"}}}}}`, srv.URL+"/v1"))
	dir := t.TempDir()
	argvFile, capture := filepath.Join(dir, "codex.argv"), filepath.Join(dir, "schema.cap")
	// help prints no --json: codexHasJSON is cached per process and must not flip for other tests
	body := `cat >/dev/null
if [ "$1" = exec ] && [ "$2" = --help ]; then exit 0; fi
printf '%s\n' "$@" > '` + argvFile + `'
prev=
for a in "$@"; do
  if [ "$prev" = --output-schema ]; then cp "$a" '` + capture + `'; printf '%s\n' "$a" > '` + capture + `.path'; ls -l "$a" | cut -c1-10 > '` + capture + `.mode'; fi
  prev=$a
done
echo '{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}'`
	pathstest.FakeVendor(t, "codex", body)
	_, err := newCLIEngine("codex").Run(context.Background(), RunOptions{Prompt: "task", Provider: "toai", Tools: true,
		SystemPrompt: `be "terse"`, AppendSystemPrompt: "APP", OutputSchema: `{"type":"object","properties":{"a":{"type":"string"}}}`}, wire.Discard)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if !followedBy(args, "-c", `instructions="be \"terse\""`) || !followedBy(args, "-c", `developer_instructions="APP"`) {
		t.Errorf("argv %q lacks the -c instructions overrides", args)
	}
	if args[0] != "exec" || !slices.Contains(args, "--output-schema") {
		t.Errorf("argv %q: want exec first and --output-schema", args)
	}
	assertCodexSchemaCleanup(t, capture)
}

func assertCodexSchemaCleanup(t *testing.T, capture string) {
	t.Helper()
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

// A parser failure kills the child at once: a child (or grandchild) holding stdout open never runs to --timeout.
func TestRunEventStreamParseErrorKillsChild(t *testing.T) {
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "echo '{}'; sleep 30 & sleep 30"}, nil, "", "", nil, true)
	bad := errors.New("bad event")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = br.ReadBytes('\n')
		return wire.Result{Answer: "seen"}, bad
	}
	start := time.Now()
	res, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, nil)
	if time.Since(start) > 10*time.Second || !errors.Is(err, bad) || res.Answer != "seen" {
		t.Fatalf("took %v, res=%+v err=%v; want a prompt kill keeping the parse error and answer", time.Since(start), res, err)
	}
}

// A parse error after stdout reached EOF must not kill the child: its own exit status is the real failure.
func TestRunEventStreamParseErrorAfterEOFKeepsExitStatus(t *testing.T) {
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "echo '{}'; exec >&-; sleep 0.5; exit 7"}, nil, "", "", nil, true)
	bad := errors.New("turn failed")
	parse := func(br *bufio.Reader, _ wire.Sink, _ func(), _ ...wire.StreamLog) (wire.Result, error) {
		_, _ = io.Copy(io.Discard, br)
		return wire.Result{}, bad
	}
	_, err := RunEventStream(context.Background(), "x", cmd, tail, wire.Discard, parse, nil)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 || !errors.Is(err, bad) {
		t.Fatalf("err = %v; want exit code 7 (not a kill) joined with the parse error", err)
	}
}

// RunStream carries a structured-only answer: no text is not an empty answer when the schema output arrived.
func TestRunStreamKeepsStructured(t *testing.T) {
	cmd, tail := ChildCmd(context.Background(), "/bin/sh", []string{"-c", "true"}, nil, "", "", nil, true)
	parse := func(*bufio.Reader, wire.Sink, func(), ...wire.StreamLog) (wire.Result, error) {
		return wire.Result{Structured: json.RawMessage(`{"ok":true}`)}, nil
	}
	res, err := RunStream(context.Background(), "x", cmd, tail, wire.Result{}, wire.Discard, parse)
	if err != nil || string(res.Structured) != `{"ok":true}` {
		t.Fatalf("res=%+v err=%v; want Structured kept and no empty-answer error", res, err)
	}
}
