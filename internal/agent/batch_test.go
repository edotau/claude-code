package agent_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

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
		env := string(raw)
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
	if !seen["a"] || !seen["b"] {
		t.Errorf("children seen = %v, want one per profile", seen)
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
