package gemini

import (
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
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// gemChatBody is the wire shape the api agent's openai-dialect chatStream sends.
type gemChatBody struct {
	Model    string `json:"model"`
	Messages []struct {
		Content string `json:"content"`
	} `json:"messages"`
}

// gotRequest captures one upstream chat-completions call for assertions.
type gotRequest struct {
	body string
	auth string
}

// newServer spins an openai-dialect SSE server that echoes "hello" with usage {5,2}, recording each
// request into hits. status != 0 makes every call fail with that HTTP status instead.
func newServer(hits *[]gotRequest, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*hits = append(*hits, gotRequest{body: string(b), auth: r.Header.Get("Authorization")})
		if status != 0 {
			http.Error(w, "upstream down", status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"model":"g-flash","choices":[{"delta":{"role":"assistant","content":"hel"}}]}`,
			`{"model":"g-flash","choices":[{"delta":{"content":"lo"}}]}`,
			`{"model":"g-flash","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
}

// setupRegistry overlays a "tgem" openai-dialect provider onto srv, keyed by TEST_GEM_KEY, and points
// CLAUDE_CONFIG_DIR at a scratch dir for the duration of t.
func setupRegistry(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TEST_GEM_KEY", "gem-key")
	body := fmt.Sprintf(`{"providers":{
	  "google":{"tgem":{"protocol":"openai","base_url":%q,"auth":{"type":"key","env":"TEST_GEM_KEY"},"models":{"opus":"g-pro","sonnet":"g-flash","haiku":"g-lite"}}}}}`,
		srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

// decodeBody unmarshals one recorded request's JSON body.
func decodeBody(t *testing.T, req gotRequest) gemChatBody {
	t.Helper()
	var b gemChatBody
	if err := json.Unmarshal([]byte(req.body), &b); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, req.body)
	}
	return b
}

// promptContent returns the decoded prompt text sent as the user message.
func promptContent(t *testing.T, req gotRequest) string {
	t.Helper()
	b := decodeBody(t, req)
	if len(b.Messages) == 0 {
		t.Fatalf("no messages in body: %s", req.body)
	}
	return b.Messages[0].Content
}

// taskSection extracts the text inside <task>...</task> from a rendered prompt.
func taskSection(prompt string) string {
	_, rest, ok := strings.Cut(prompt, "<task>\n")
	if !ok {
		return ""
	}
	body, _, _ := strings.Cut(rest, "\n</task>")
	return body
}

// TestBridgeRunPrintCommand: --print-command sends nothing over the wire and logs the request it would
// have made.
func TestBridgeRunPrintCommand(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	var log strings.Builder
	_, err := RunGemini(context.Background(), GeminiOptions{
		Provider: "tgem", Cwd: dir, Task: "t", PrintCommand: true,
	}, io.Discard, &log)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (print-command sends nothing)", len(hits))
	}
	want := fmt.Sprintf("POST %s/v1/chat/completions model=g-flash", srv.URL)
	if !strings.Contains(log.String(), want) {
		t.Errorf("log missing %q:\n%s", want, log.String())
	}
}

// TestChangedFilesVsHEADErrorsOutsideRepo: called outside a git work tree, the error mentions git rather
// than masking the failure as an empty diff.
func TestChangedFilesVsHEADErrorsOutsideRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	_, err := ChangedFilesVsHEAD(t.Context(), dir)
	if err == nil {
		t.Fatal("expected an error outside a git work tree")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("err = %v, want it to mention git", err)
	}
}

// setupGeminiProviderOverride overlays the built-in google·antigravity profile itself (not a side name) onto srv,
// so a bridge call with Provider=="" (the no-explicit-flag case) still resolves to a fake endpoint.
func setupGeminiProviderOverride(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	body := fmt.Sprintf(`{"providers":{
	  "google":{"antigravity":{"protocol":"openai","base_url":%q,"auth":{"type":"cli","login":"gemini"},"models":{"opus":"g-pro","sonnet":"g-flash","haiku":"g-lite"}}}}}`,
		srv.URL+"/v1")
	vaulttest.Put(t, vault.Overlay, "providers", body)
}

// fakeAgyScript installs agy with a sentinel and a canned stream-json result without step_update.
func fakeAgyScript(t *testing.T, sentinel string) {
	t.Helper()
	// printf, not echo: /bin/sh echo expands \n to a literal newline, breaking the JSON line in two.
	script := "cat > /dev/null\n: > " + sentinel + "\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"ok","num_turns":1,"usage":{"input_tokens":10,"output_tokens":1}}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)
}

// loginHome returns a temp HOME carrying (or not) agy's cached-login marker.
func loginHome(t *testing.T, present bool) string {
	t.Helper()
	home := t.TempDir()
	if !present {
		return home
	}
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestBridgeRunNoLoginErrors: Provider=="" (no explicit --provider) with no cached agy login and no
// PrintCommand now refuses outright — the bridge's default path never falls back to an API key.
func TestBridgeRunNoLoginErrors(t *testing.T) {
	t.Setenv("HOME", loginHome(t, false))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no bin/gemini either
	t.Setenv("PATH", t.TempDir())
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupGeminiProviderOverride(t, srv)

	dir := t.TempDir()
	_, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bin/gemini") || !strings.Contains(err.Error(), "claude-code install") {
		t.Fatalf("err = %v, want the bin/gemini-missing error naming `claude-code install`", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (no API fallback)", len(hits))
	}
}

// TestBridgeRunNoCachedLoginErrors: bin/gemini is installed but agy has never signed in — the
// preflight must still name that case, distinct from the missing-binary one above.
func TestBridgeRunNoCachedLoginErrors(t *testing.T) {
	t.Setenv("HOME", loginHome(t, false))
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupGeminiProviderOverride(t, srv) // sets CLAUDE_CONFIG_DIR; bin/gemini must land under that same dir
	fakeAgyScript(t, filepath.Join(t.TempDir(), "sentinel"))

	dir := t.TempDir()
	_, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agy is not signed in") {
		t.Fatalf("err = %v, want an agy-not-signed-in error", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (no API fallback)", len(hits))
	}
}

// TestBridgeRunAgyBackendWhenLoginPresent: Provider=="" with a cached agy login and installed agy routes
// through RunAgy instead of the api agent — zero HTTP hits, the agy backend log line, and the
// result event's conversation id surfacing as SessionID even though the fixture carries no step_update.
func TestBridgeRunAgyBackendWhenLoginPresent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupGeminiProviderOverride(t, srv)
	sentinel := filepath.Join(t.TempDir(), "ran")
	fakeAgyScript(t, sentinel)

	dir := t.TempDir()
	var log strings.Builder
	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t"}, io.Discard, &log)
	// The canned fixture has no step_update delta: the answer must come from result.response.
	if err != nil || res.Answer != "ok" {
		t.Errorf("Run = (%q, %v), want (\"ok\", nil)", res.Answer, err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (agy path makes no HTTP call)", len(hits))
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Error("agy sentinel missing: the fake agy binary never ran")
	}
	if !strings.Contains(log.String(), "gemini bridge: agy") {
		t.Errorf("log missing agy backend line:\n%s", log.String())
	}
	if res.SessionID != "c1" {
		t.Errorf("SessionID = %q, want c1 (from the result event's conversation_id)", res.SessionID)
	}
}

// TestBridgeRunPrintCommandAgyBackend: --print-command on the agy path prints the resolved agy argv
// (model, stream-json both ways) instead of the api agent's POST line, and sends nothing over the wire.
func TestBridgeRunPrintCommandAgyBackend(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupGeminiProviderOverride(t, srv)
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))

	dir := t.TempDir()
	var log strings.Builder
	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: dir, Task: "t", PrintCommand: true}, io.Discard, &log)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (print-command sends nothing)", len(hits))
	}
	if !strings.Contains(log.String(), "agy --model "+AgyDefaultModel) || !strings.Contains(log.String(), "stream-json") {
		t.Errorf("log missing the resolved agy argv:\n%s", log.String())
	}
	if res.Provider != "agy" {
		t.Errorf("provider = %q, want agy", res.Provider)
	}
}

// dispatchRecords lists the dispatch log Record files under CLAUDE_CONFIG_DIR/state/harness/dispatch.
func dispatchRecords(t *testing.T) []agent.Record {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "state", "harness", "dispatch")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []agent.Record
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r agent.Record
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestBridgeRunAgyTimeoutWithinDeadline: a stub agy that sleeps past GeminiOptions.Timeout must surface a
// timeout error well inside the caller's own context deadline, not hang for the full retry ladder.
func TestBridgeRunAgyTimeoutWithinDeadline(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	pathstest.FakeVendor(t, "agy", "cat > /dev/null\nexec "+sleep+" 5\n")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = RunGemini(ctx, GeminiOptions{Cwd: t.TempDir(), Task: "t", Timeout: 50 * time.Millisecond}, io.Discard, io.Discard)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("elapsed = %s, want well inside the 150ms outer deadline (plus one 50ms attempt)", elapsed)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to mention the deadline", err)
	}
}

// TestBridgeRunAgyRetries503ThenSucceeds: a stub agy that fails its first invocation with a 503-shaped
// stderr line and succeeds on the second must be retried (not surfaced as a fatal error on attempt 1).
func TestBridgeRunAgyRetries503ThenSucceeds(t *testing.T) {
	fastBackoff(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	counter := filepath.Join(t.TempDir(), "attempts")
	script := `cat > /dev/null
n=0
[ -f ` + counter + ` ] && n=$(cat ` + counter + `)
n=$((n+1))
echo $n > ` + counter + `
if [ "$n" -eq 1 ]; then
  echo "503 Service Unavailable" 1>&2
  exit 1
fi
printf '%s\n' '{"event":"result","result":{"conversation_id":"r2","status":"SUCCESS","response":"ok","usage":{"input_tokens":1,"output_tokens":1}}}'
`
	pathstest.FakeVendor(t, "agy", script)

	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Run after one retry = %v, want nil", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want ok", res.Answer)
	}
	got, readErr := os.ReadFile(counter)
	if readErr != nil || strings.TrimSpace(string(got)) != "2" {
		t.Errorf("attempt counter = %q (err %v), want exactly 2 invocations (one retry)", got, readErr)
	}
}

// TestBridgeRunAgyNoRetryOn400: a stub agy that always fails with a 400-shaped stderr line is fatal on
// the first attempt — never retried, unlike the transient 503/429 cases.
func TestBridgeRunAgyNoRetryOn400(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	counter := filepath.Join(t.TempDir(), "attempts")
	script := `cat > /dev/null
n=0
[ -f ` + counter + ` ] && n=$(cat ` + counter + `)
n=$((n+1))
echo $n > ` + counter + `
echo "400 Bad Request: invalid argument" 1>&2
exit 1
`
	pathstest.FakeVendor(t, "agy", script)

	_, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Kind != agent.KindFatal {
		t.Fatalf("err = %v, want a fatal *Error (400 must not be retried)", err)
	}
	got, readErr := os.ReadFile(counter)
	if readErr != nil || strings.TrimSpace(string(got)) != "1" {
		t.Errorf("attempt counter = %q (err %v), want exactly 1 invocation (no retry on a 400)", got, readErr)
	}
}

// TestBridgeRunAgyWritesDispatchRecord: a successful agy bridge run leaves a dispatch Record behind, so
// `ask --log` surfaces bridge runs too, labeled distinctly from a plain `ask --agent gemini` dispatch.
func TestBridgeRunAgyWritesDispatchRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))

	if _, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	if recs[0].Agent != "gemini-bridge" {
		t.Errorf("record.Agent = %q, want gemini-bridge", recs[0].Agent)
	}
	if recs[0].Status != "ok" {
		t.Errorf("record.Status = %q, want ok", recs[0].Status)
	}
}

// TestBridgeRunAgyOverMaxPromptBytesSucceeds: a sweep whose inlined files push the composed prompt well
// over MaxPromptBytes (96 KiB, the argv-element cap that bounds a vendor-exec agent's prompt) must
// still succeed — the bridge drives agy over its own stdin-NDJSON path (RunAgy), not the argv-bound
// vendorRunner, so that cap does not apply here. Regression guard for the CRITICAL that reintroduced it.
func TestBridgeRunAgyOverMaxPromptBytesSucceeds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))

	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 300<<10)) // 300 KiB, over the 96 KiB argv cap

	res, err := RunGemini(context.Background(), GeminiOptions{
		Cwd: dir, Dirs: []string{"."}, Task: "t", MaxFiles: -1, MaxFileBytes: 400 << 10,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("a >96 KiB inlined sweep over agy must not be capped by MaxPromptBytes: %v", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want ok", res.Answer)
	}
}

// TestBridgeRunAgyDispatchRecordIsCompact: the dispatch Record's Prompt field must stay a small summary
// (task text plus a file list), never the full inlined source — so a sentinel string planted deep inside
// an inlined file never reaches the on-disk dispatch log, and the log never balloons to MiBs per run.
func TestBridgeRunAgyDispatchRecordIsCompact(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	fakeAgyScript(t, filepath.Join(t.TempDir(), "ran"))

	dir := t.TempDir()
	const sentinel = "SENTINEL-DO-NOT-LEAK-7f3a9c"
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 50<<10)+sentinel+strings.Repeat("y", 50<<10))

	const task = "summarize the repo"
	if _, err := RunGemini(context.Background(), GeminiOptions{
		Cwd: dir, Dirs: []string{"."}, Task: task, MaxFiles: -1, MaxFileBytes: 200 << 10,
	}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	prompt := recs[0].Request.Prompt
	if len(prompt) >= 16<<10 {
		t.Errorf("record prompt = %d bytes, want < 16 KiB (compact summary only)", len(prompt))
	}
	if !strings.Contains(prompt, task) {
		t.Errorf("record prompt = %q, want it to contain the task text %q", prompt, task)
	}
	if strings.Contains(prompt, sentinel) {
		t.Errorf("record prompt leaked the inlined file's sentinel: %q", prompt)
	}
}

// fastBackoff shrinks the shared retry backoff for the test and restores it on cleanup.
func fastBackoff(t *testing.T) {
	t.Helper()
	b, m := agent.DefaultBaseDelay, agent.DefaultMaxDelay
	agent.DefaultBaseDelay, agent.DefaultMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { agent.DefaultBaseDelay, agent.DefaultMaxDelay = b, m })
}
