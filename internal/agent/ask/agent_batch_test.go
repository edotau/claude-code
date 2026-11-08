package ask

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runCLI runs `claude-code agents ask <argv>` in-process (argv[0] must be "ask") and returns exit code, stdout and stderr.
func runCLI(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := Cmd(argv[1:])
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	o, _ := io.ReadAll(outR)
	e, _ := io.ReadAll(errR)
	return code, string(o), string(e)
}

// cliEnv is a hermetic config dir + HOME with the agy login marker, so every fake CLI looks logged in.
func cliEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("OPENCODE_API_KEY", "test-key") // an own-login route for opencode
	for name, profile := range map[string]string{"codex": "chatgpt", "gemini": "antigravity", "opencode": "nvidia"} {
		if err := vault.SetAgentRecord(name, vault.AgentRecord{Profile: profile}); err != nil { // an external agent with no profile is off
			t.Fatal(err)
		}
	}
	marker := gemini.AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cliFakes installs a stand-in child per CLI agent (copilot rides the SDK and is covered in copilot_gate_test.go).
func cliFakes(t *testing.T) {
	t.Helper()
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sc","model":"fake-claude"}'`+"\n"+
		`printf '%s\n' '{"type":"assistant","message":{"model":"fake-claude","content":[{"type":"text","text":"hello"}]}}'`+"\n"+
		`printf '%s\n' '{"type":"result","result":"hello","usage":{"input_tokens":3,"output_tokens":2}}'`)
	pathstest.FakeVendor(t, "codex", `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+"cat > /dev/null; sleep 0.05; echo hello")
	pathstest.FakeVendor(t, "agy", "cat > /dev/null; sleep 0.05\n"+
		`printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello"}}'`+"\n"+
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"hello","num_turns":1,"usage":{"input_tokens":3,"output_tokens":2}}}'`)
	pathstest.FakeVendor(t, "opencode", "sleep 0.05\n"+
		`printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`+"\n"+
		`printf '%s\n' '{"type":"step_finish","sessionID":"so","part":{"tokens":{"Input":3,"Output":2}}}'`)
}

// cliMarker is a fake child that records its invocation (never a `login` probe) by touching a file.
func cliMarker(t *testing.T, vendor, body string) string {
	t.Helper()
	marker := filepath.Join(t.TempDir(), vendor+".ran")
	pathstest.FakeVendor(t, vendor, `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\n"+
		"touch "+marker+"\n"+body)
	return marker
}

// batchFile writes one JSONL task per line and returns the path.
func batchFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// envelopes decodes one envelope per non-empty stdout line.
func envelopes(t *testing.T, out string) []map[string]any {
	t.Helper()
	var all []map[string]any
	for ln := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if ln == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("stdout line is not an envelope: %v\n%s", err, ln)
		}
		all = append(all, e)
	}
	return all
}

func taskLine(t *testing.T, m map[string]any) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func errKind(e map[string]any) string {
	if x, ok := e["error"].(map[string]any); ok {
		s, _ := x["kind"].(string)
		return s
	}
	return ""
}

// Spanning: mixed ok/error tasks stream one envelope each in completion order, tagged, with a non-zero exit.
func TestBatchMixedStreamsEnvelopesAndExitsNonZero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	pathstest.FakeVendor(t, "claude", `p=$(cat); case "$p" in *SLOW*) sleep 0.3;; esac`+"\n"+
		`printf '%s\n' '{"type":"system","subtype":"init","session_id":"sc","model":"fake-claude"}'`+"\n"+
		`printf '%s\n' '{"type":"result","result":"hello","usage":{"input_tokens":3,"output_tokens":2}}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "slow", "agent": "claude", "prompt": "SLOW task", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "fast", "agent": "claude", "prompt": "quick task", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "bad", "agent": "no-such-agent", "prompt": "x", "workdir": t.TempDir()}),
		`{not json`,
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code == 0 {
		t.Fatalf("exit 0 with failing tasks\nstdout: %s\nstderr: %s", out, stderr)
	}
	envs := envelopes(t, out)
	if len(envs) != 4 {
		t.Fatalf("got %d envelopes, want 4 (one per input line, bad lines included)\n%s", len(envs), out)
	}
	byID := indexBatchEnvelopes(t, envs)
	checkMixedBatchResults(t, byID, envs, out)
}

func indexBatchEnvelopes(t *testing.T, envs []map[string]any) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	batchIDs := map[string]bool{}
	dispatchIDs := map[string]bool{}
	for _, e := range envs {
		if e["schema"] != float64(1) {
			t.Errorf("schema = %v, want 1", e["schema"])
		}
		if b, _ := e["batch_id"].(string); b != "" {
			batchIDs[b] = true
		} else {
			t.Errorf("envelope without batch_id: %v", e)
		}
		if id, _ := e["task_id"].(string); id != "" {
			byID[id] = e
		}
		if d, _ := e["dispatch_id"].(string); d != "" {
			if dispatchIDs[d] {
				t.Errorf("dispatch_id %q repeated across tasks", d)
			}
			dispatchIDs[d] = true
		}
	}
	if len(batchIDs) != 1 {
		t.Errorf("batch_ids = %v, want exactly one shared id", batchIDs)
	}
	return byID
}

func checkMixedBatchResults(t *testing.T, byID map[string]map[string]any, envs []map[string]any, out string) {
	t.Helper()
	for _, id := range []string{"slow", "fast", "bad"} {
		if byID[id] == nil {
			t.Fatalf("no envelope tagged task_id %q\n%s", id, out)
		}
	}
	for _, id := range []string{"slow", "fast"} {
		if byID[id]["status"] != "ok" || byID[id]["answer"] != "hello" {
			t.Errorf("task %s = %v, want status ok / answer hello", id, byID[id])
		}
	}
	if byID["bad"]["status"] == "ok" || errKind(byID["bad"]) != "usage" {
		t.Errorf("bad agent = %v, want a non-ok usage-kind envelope", byID["bad"])
	}
	// completion order: the SLOW task finishes after the fast one
	pos := map[string]int{}
	for i, e := range envs {
		id, _ := e["task_id"].(string)
		pos[id] = i
	}
	if pos["fast"] > pos["slow"] {
		t.Errorf("fast task printed after the slow one: streaming must be completion order\n%s", out)
	}
}

// All-ok batch exits 0.
func TestBatchAllOkExitsZero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "a", "agent": "claude", "prompt": "one", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "b", "agent": "claude", "prompt": "two", "workdir": t.TempDir(), "tools": true}),
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	for _, e := range envelopes(t, out) {
		if e["status"] != "ok" {
			t.Errorf("envelope %v, want ok", e)
		}
	}
}

// --fail-fast: the first fatal failure cancels the batch; a queued task reports cancelled and never spawns.
func TestBatchFailFastNeverSpawnsQueuedTask(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	pathstest.FakeVendor(t, "claude", "cat > /dev/null; echo boom >&2; exit 1")
	marker := cliMarker(t, "opencode", `printf '%s\n' '{"type":"text","sessionID":"so","part":{"text":"hello"}}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "first", "agent": "claude", "prompt": "fail", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "queued", "agent": "opencode", "prompt": "never", "workdir": t.TempDir(), "tools": true}),
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "1", "--fail-fast")
	if code == 0 {
		t.Fatalf("exit 0 after a fatal failure\nstdout: %s\nstderr: %s", out, stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("queued task spawned its child after --fail-fast cancelled the batch")
	}
	var queued map[string]any
	for _, e := range envelopes(t, out) {
		if e["task_id"] == "queued" {
			queued = e
		}
	}
	if queued == nil {
		t.Fatalf("queued task got no envelope\n%s", out)
	}
	msg, _ := queued["error"].(map[string]any)["message"].(string)
	if queued["status"] != "error" || !strings.Contains(strings.ToLower(msg), "cancel") {
		t.Errorf("queued = %v, want status error with a cancelled message", queued)
	}
}

func fakeJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// Guarantee 1: concurrent tasks on two profiles; each child's env holds only its own route and token.
func TestBatchConcurrentProfilesKeepEnvsApart(t *testing.T) {
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	t.Setenv("PB_BATCH_KEY", "TOKEN-BBBB-2222")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:11111","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m-a","sonnet":"m-a","haiku":"m-a"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:22222","auth":{"type":"key","env":"PB_BATCH_KEY"},"models":{"opus":"m-b","sonnet":"m-b","haiku":"m-b"}}}}}`)
	dump := t.TempDir()
	// each child announces itself, then waits until both are running: proof the two really overlap
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\n"+
		"env > "+dump+"/$$.env\ntouch "+dump+"/$$.up\n"+
		"for i in $(seq 1 100); do [ $(ls "+dump+"/*.up 2>/dev/null | wc -l) -ge 2 ] && break; sleep 0.05; done\n"+
		`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "a", "agent": "claude", "profile": "pa", "prompt": "x", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "b", "agent": "claude", "profile": "pb", "prompt": "y", "workdir": t.TempDir(), "tools": true}),
	)
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	dumps, _ := filepath.Glob(filepath.Join(dump, "*.env"))
	if len(dumps) != 2 {
		t.Fatalf("got %d child env dumps, want 2 (the children never overlapped?)", len(dumps))
	}
	seen := map[string]bool{}
	for _, f := range dumps {
		raw, _ := os.ReadFile(f)
		recordBatchChildProfile(t, seen, string(raw))
	}
	if !seen["a"] || !seen["b"] {
		t.Errorf("children seen = %v, want one per profile", seen)
	}
}

func recordBatchChildProfile(t *testing.T, seen map[string]bool, env string) {
	t.Helper()
	a, b := strings.Contains(env, "TOKEN-AAAA-1111"), strings.Contains(env, "TOKEN-BBBB-2222")
	ra, rb := strings.Contains(env, "127.0.0.1:11111"), strings.Contains(env, "127.0.0.1:22222")
	switch {
	case a && !b && ra && !rb:
		seen["a"] = true
	case b && !a && rb && !ra:
		seen["b"] = true
	default:
		t.Errorf("child env mixes profiles (token a=%v b=%v, route a=%v b=%v)", a, b, ra, rb)
	}
}

// Guarantee 2: a credential issued for another host than the route's fails as auth before the child starts.
func TestBatchHostMismatchFailsAuthBeforeExec(t *testing.T) {
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HM_BATCH_KEY", fakeJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks","host":"https://adb-1.azuredatabricks.net",`+
		`"auth":{"type":"key","env":"HM_BATCH_KEY"},"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
	marker := cliMarker(t, "claude", `printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, taskLine(t, map[string]any{"id": "m", "agent": "claude", "profile": "hm", "prompt": "x", "workdir": t.TempDir(), "tools": true}))
	code, out, stderr := runCLI(t, "ask", "--batch", file)
	if code == 0 {
		t.Fatalf("exit 0 on a host mismatch\nstdout: %s\nstderr: %s", out, stderr)
	}
	envs := envelopes(t, out)
	if len(envs) != 1 || envs[0]["status"] == "ok" || errKind(envs[0]) != "auth" {
		t.Fatalf("envelopes = %v, want one non-ok envelope of kind auth", envs)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the child started although the credential's host does not match the route")
	}
	if strings.Contains(out+stderr, "adb-2") && strings.Contains(out+stderr, ".sig") {
		t.Error("output carries the raw credential")
	}
}

// Every --batch task goes through RunRecorded: it leaves an <id>.events.jsonl running start to result.
func TestBatchTasksWriteEventFiles(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t,
		taskLine(t, map[string]any{"id": "one", "agent": "claude", "prompt": "p1", "workdir": t.TempDir(), "tools": true}),
		taskLine(t, map[string]any{"id": "two", "agent": "claude", "prompt": "p2", "workdir": t.TempDir(), "tools": true}),
	)
	if code, out, stderr := runCLI(t, "ask", "--batch", file, "--jobs", "2"); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	files, _ := filepath.Glob(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "dispatch", "*.events.jsonl"))
	if len(files) != 2 {
		t.Fatalf("event files = %v, want 2 (one per task)", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) < 2 || !strings.Contains(lines[0], `"type":"start"`) || !strings.Contains(lines[len(lines)-1], `"type":"result"`) {
			t.Errorf("%s: want start...result, got\n%s", f, b)
		}
	}
}

// The vault's custom env holds every profile's vars; a batch child on pa gets pa's and never pb's credential var.
func TestBatchChildDropsOtherProfilesCustomCredentialVar(t *testing.T) {
	cliEnv(t)
	vaulttest.Use(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PA_BATCH_KEY", "TOKEN-AAAA-1111")
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:11111","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m-a","sonnet":"m-a","haiku":"m-a"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:22222","auth":{"type":"key","env":"PB_BATCH_KEY"},"models":{"opus":"m-b","sonnet":"m-b","haiku":"m-b"}}}}}`)
	if err := providers.SetCustomEnv("PB_BATCH_KEY", "PB-CUSTOM-SECRET"); err != nil {
		t.Fatal(err)
	}
	if err := providers.SetCustomEnv("PA_CUSTOM_NOTE", "pa-note"); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "child.env")
	pathstest.FakeVendor(t, "claude", "cat > /dev/null\nenv > "+dump+"\n"+`printf '%s\n' '{"type":"result","result":"hello"}'`)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "profile": "pa", "prompt": "x", "workdir": t.TempDir(), "tools": true}))
	if code, out, stderr := runCLI(t, "ask", "--batch", file); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := string(raw)
	if strings.Contains(env, "PB-CUSTOM-SECRET") {
		t.Error("pa's child holds pb's custom credential var")
	}
	if !strings.Contains(env, "TOKEN-AAAA-1111") || !strings.Contains(env, "pa-note") {
		t.Errorf("pa's child lost its own token or an unrelated custom var:\n%s", env)
	}
}

var profileFor = map[string]string{"claude": "", "codex": "chatgpt", "gemini": "", "opencode": ""}

func decodeEnvelope(t *testing.T, out string) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\n%s", err, out)
	}
	return env
}

// Every agent's --format json success emits the one standard envelope.
func TestEnvelopeContractSuccess(t *testing.T) {
	noUsage := map[string]bool{"codex": true}
	for _, name := range []string{"claude", "codex", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, "ask", "--agent", name, "--profile", profileFor[name], "--fallback", "none", "--native", "--format", "json", "say hello")
			if code != 0 {
				t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, stderr)
			}
			env := decodeEnvelope(t, out)
			checkSuccessEnvelopeFields(t, env, name)
			checkSuccessEnvelopeUsage(t, env, name, noUsage[name])
		})
	}
}

func checkSuccessEnvelopeFields(t *testing.T, env map[string]any, name string) {
	t.Helper()
	if env["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", env["schema"])
	}
	if env["status"] != "ok" {
		t.Errorf("status = %v, want ok", env["status"])
	}
	if env["agent"] != name {
		t.Errorf("agent = %v, want %s", env["agent"], name)
	}
	// opencode on its own login picks its own model and reports none; the key must still be present.
	if m, ok := env["model"].(string); !ok || (m == "" && name != "opencode") {
		t.Errorf("model = %v, want non-empty", env["model"])
	}
	if id, _ := env["dispatch_id"].(string); id == "" {
		t.Errorf("dispatch_id = %v, want non-empty", env["dispatch_id"])
	}
}

func checkSuccessEnvelopeUsage(t *testing.T, env map[string]any, name string, noUsage bool) {
	t.Helper()
	if v, ok := env["fallback"]; !ok || v != nil {
		t.Errorf("fallback = %v (present=%v), want explicit null", v, ok)
	}
	if d, _ := env["duration_ms"].(float64); d <= 0 {
		t.Errorf("duration_ms = %v, want > 0", env["duration_ms"])
	}
	if noUsage {
		if v, ok := env["usage"]; !ok || v != nil {
			t.Errorf("usage = %v (present=%v), want explicit null: %s reports none", v, ok, name)
		}
	} else if _, ok := env["usage"].(map[string]any); !ok {
		t.Errorf("usage = %v, want an object", env["usage"])
	}
}

// A resume whose launch fails returns an error and never runs another agent's engine.
func TestResumeNeverFallsBack(t *testing.T) {
	cliEnv(t)
	codexRan := cliMarker(t, "codex", "echo 'codex: boom' >&2; exit 1")
	others := map[string]string{
		"claude":   cliMarker(t, "claude", "exit 1"),
		"agy":      cliMarker(t, "agy", "exit 1"),
		"opencode": cliMarker(t, "opencode", "exit 1"),
	}
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--session", "sess-1", "continue please")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero: the resumed session's launch failed\nstdout: %s", out)
	}
	if _, err := os.Stat(codexRan); err != nil {
		t.Fatalf("the codex agent was never reached (%v); the test proves nothing\nstderr: %s", err, stderr)
	}
	for name, marker := range others {
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("resume fell back and ran %s; a session belongs to one agent\nstderr: %s", name, stderr)
		}
	}
}

// --profile given to resume reaches the agent's target resolution instead of being dropped.
func TestResumeForwardsProfile(t *testing.T) {
	cliEnv(t)
	cliMarker(t, "codex", "echo ok")
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--session", "sess-1", "--profile", "no-such-profile", "continue")
	if code == 0 {
		t.Errorf("exit = 0, want non-zero for an unknown --profile\nstdout: %s", out)
	}
	if !strings.Contains(stderr+out, "no-such-profile") {
		t.Errorf("output never names the --profile given to resume:\nstdout: %s\nstderr: %s", out, stderr)
	}
}

// Every usage failure in --format json mode prints one parseable error envelope and keeps its exit code.
func TestJSONModeFailuresPrintEnvelope(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		code int
	}{
		{"conflicting tools flags", []string{"ask", "--format", "json", "--tools", "--no-tools", "hi"}, 1},
		{"invalid fallback", []string{"ask", "--format", "json", "--fallback", "bogus", "hi"}, 1},
		{"missing prompt", []string{"ask", "--format", "json"}, 2},
		{"retry of unknown id", []string{"ask", "--format", "json", "--retry", "no-such-id"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, c.argv...)
			if code == 0 {
				t.Fatalf("exit = 0, want a failure\nstdout: %s", out)
			}
			if c.code == 2 && code != 2 {
				t.Errorf("exit = %d, want 2\nstderr: %s", code, stderr)
			}
			env := decodeEnvelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error: %s", env["status"], out)
			}
			e, ok := env["error"].(map[string]any)
			if !ok || e["message"] == "" {
				t.Fatalf("error = %#v, want {kind,message}", env["error"])
			}
			if c.name != "retry of unknown id" && e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage", e["kind"])
			}
		})
	}
}

// An unknown --profile is a caller error: all five agents report an error envelope of kind usage, not fatal.
func TestUnknownProfileErrorEnvelopeIsUsageKindForAllAgents(t *testing.T) {
	for _, name := range []string{"claude", "codex", "copilot", "gemini", "opencode"} {
		t.Run(name, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			code, out, stderr := runCLI(t, "ask", "--agent", name, "--profile", "nosuch", "--fallback", "none", "--format", "json", "x")
			if code != 1 {
				t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
			}
			env := decodeEnvelope(t, out)
			if env["status"] != "error" {
				t.Errorf("status = %v, want error", env["status"])
			}
			e, _ := env["error"].(map[string]any)
			if e["kind"] != "usage" {
				t.Errorf("error.kind = %v, want usage: %s", e["kind"], out)
			}
			if m, _ := e["message"].(string); m == "" {
				t.Errorf("error.message = %v, want non-empty", e["message"])
			}
		})
	}
}

// --batch validates --provider like a single ask does: an unknown provider exits 2 before any task runs.
func TestBatchRejectsUnknownProvider(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "prompt": "one", "workdir": t.TempDir(), "tools": true}))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--provider", "nonexistent")
	if code != 2 || !strings.Contains(stderr, "nonexistent") || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, want 2 naming the provider with no envelopes\nstdout: %s\nstderr: %s", code, out, stderr)
	}
}
