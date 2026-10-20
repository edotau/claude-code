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
	"github.com/edotau/claude-code/internal/vendors"
)

// gemChatBody is the wire shape the api runner's openai-dialect chatStream sends.
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
	  "tgem":{"kind":"openai","base_url":%q,"auth":{"type":"bearer","env":"TEST_GEM_KEY"},"models":{"opus":"g-pro","sonnet":"g-flash","haiku":"g-lite"}}}}`,
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

// TestBridgeRunDefaultModel: default provider+model resolution, file inlining, task text, auth header,
// answer/usage passthrough, and the "N indexed, M inlined" log line — all on one call.
func TestBridgeRunDefaultModel(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")

	var stream, log strings.Builder
	res, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "describe TASKMARKER987", MaxFiles: -1,
	}, &stream, &log)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(hits))
	}
	body := decodeBody(t, hits[0])
	if body.Model != "g-flash" {
		t.Errorf("model = %q, want table sonnet g-flash (not opus g-pro)", body.Model)
	}
	prompt := promptContent(t, hits[0])
	if !strings.Contains(prompt, `<file path="a.go"`) {
		t.Errorf("prompt missing inlined file tag:\n%s", prompt)
	}
	if !strings.Contains(prompt, "TASKMARKER987") {
		t.Errorf("prompt missing task text:\n%s", prompt)
	}
	if hits[0].auth != "Bearer gem-key" {
		t.Errorf("authorization = %q, want Bearer gem-key", hits[0].auth)
	}
	if res.Answer != "hello" {
		t.Errorf("answer = %q, want hello", res.Answer)
	}
	if res.Usage != (agent.Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Errorf("usage = %+v, want {5 2}", res.Usage)
	}
	if !strings.Contains(log.String(), "1 path(s) indexed, 1 inlined") {
		t.Errorf("log missing indexed/inlined stats: %q", log.String())
	}
}

// TestBridgeRunModelPin: the bridge's default model comes from the provider's own sonnet slot, bypassing
// HARNESS_SONNET_MODEL — but an explicit --model sonnet still goes through the pinned resolution.
func TestBridgeRunModelPin(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)
	t.Setenv("HARNESS_SONNET_MODEL", "claude-sonnet-5")

	dir := t.TempDir()

	if _, err := Run(context.Background(), Options{Provider: "tgem", Cwd: dir, Task: "t"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := decodeBody(t, hits[len(hits)-1]).Model; got != "g-flash" {
		t.Errorf("default model with HARNESS_SONNET_MODEL set = %q, want g-flash (bridge default bypasses slot pins)", got)
	}

	if _, err := Run(context.Background(), Options{Provider: "tgem", Model: "sonnet", Cwd: dir, Task: "t"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := decodeBody(t, hits[len(hits)-1]).Model; got != "claude-sonnet-5" {
		t.Errorf("explicit --model sonnet = %q, want claude-sonnet-5 pin", got)
	}
}

// TestBridgeRunIndex: --index with a tight max-files appends the "Repository index" section and still
// names the file that got skipped, even though its content was not inlined.
func TestBridgeRunIndex(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")

	_, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "t", Index: true, MaxFiles: 1,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptContent(t, hits[len(hits)-1])
	if !strings.Contains(prompt, "Repository index") {
		t.Errorf("prompt missing index heading:\n%s", prompt)
	}
	if !strings.Contains(prompt, "b.go") {
		t.Errorf("prompt missing skipped path b.go in the index:\n%s", prompt)
	}
}

// TestBridgeRunLargeFileAccepted: the api runner does not enforce agent.MaxPromptBytes (only exec
// runners do), so a 200 KiB inlined file must sail through, not error.
func TestBridgeRunLargeFileAccepted(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", 200<<10))

	res, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "t", MaxFiles: -1, MaxFileBytes: 300 << 10,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("200 KiB file should not be capped: %v", err)
	}
	if res.Answer != "hello" {
		t.Errorf("answer = %q, want hello", res.Answer)
	}
	prompt := promptContent(t, hits[len(hits)-1])
	if !strings.Contains(prompt, `<file path="big.txt"`) {
		t.Errorf("big.txt was not actually inlined (MaxFiles must be -1, not the 0 default = index-only):\n%s", prompt)
	}
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
	_, err := Run(context.Background(), Options{
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

// TestBridgeRunRubric: --rubric prefixes the task with the fixed-standard framing from RubricPrompt.
func TestBridgeRunRubric(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	rubric := filepath.Join(t.TempDir(), "rubric.md")
	write(t, rubric, "no bare excepts")

	dir := t.TempDir()
	_, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Task: "review this", Rubric: rubric,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptContent(t, hits[len(hits)-1])
	task := taskSection(prompt)
	if !strings.HasPrefix(task, "You are reviewing against a fixed standard") {
		t.Errorf("task section = %q, want it to start with the rubric framing", task)
	}
}

// TestBridgeRunDiff: --diff inlines only files changed vs HEAD (here, one untracked file), and errors
// with "nothing changed" once that file is committed and nothing else has moved.
func TestBridgeRunDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	repo := t.TempDir()
	write(t, filepath.Join(repo, "base.txt"), "base content")
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "-m", "base"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// ChangedFilesVsHEAD now surfaces git errors instead of masking them, so a HEAD commit must
	// already exist — "git diff ... HEAD" fails outright in a zero-commit repo.
	write(t, filepath.Join(repo, "new.txt"), "untracked content")

	_, err := Run(context.Background(), Options{Provider: "tgem", Cwd: repo, Task: "t", Diff: true, MaxFiles: -1}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptContent(t, hits[len(hits)-1])
	if !strings.Contains(prompt, "new.txt") || !strings.Contains(prompt, "untracked content") {
		t.Errorf("prompt missing the untracked file:\n%s", prompt)
	}

	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "add new.txt"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_, err = Run(context.Background(), Options{Provider: "tgem", Cwd: repo, Task: "t", Diff: true, MaxFiles: -1}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("err = %v, want it to mention 'nothing changed'", err)
	}
}

// TestBridgeIndexOnly: IndexOnly inlines no file bodies (only the index lists every path), and the
// zero-value Options{} (MaxFiles==0) still inlines, using the package default cap.
func TestBridgeIndexOnly(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")

	_, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "t", Index: true, IndexOnly: true,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptContent(t, hits[len(hits)-1])
	if strings.Contains(prompt, "<file path=") {
		t.Errorf("IndexOnly must inline no file bodies:\n%s", prompt)
	}
	if !strings.Contains(prompt, "a.go") || !strings.Contains(prompt, "b.go") {
		t.Errorf("prompt missing both paths in the index:\n%s", prompt)
	}

	// Zero-value Options (MaxFiles == 0, not set) must still inline, using DefaultMaxFiles — the
	// zero value must not collapse to IndexOnly's maxFiles=0 behavior.
	_, err = Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "t",
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	prompt = promptContent(t, hits[len(hits)-1])
	if !strings.Contains(prompt, `<file path="a.go"`) || !strings.Contains(prompt, `<file path="b.go"`) {
		t.Errorf("zero-value MaxFiles should default to inlining, got:\n%s", prompt)
	}
}

// TestBridgeLogListsInlinedPaths: the log names each inlined path on its own line after the stats line.
func TestBridgeLogListsInlinedPaths(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")

	var log strings.Builder
	_, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Dirs: []string{"."}, Task: "t", MaxFiles: -1,
	}, io.Discard, &log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "  a.go\n") {
		t.Errorf("log missing inlined path line \"  a.go\":\n%s", log.String())
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

// TestBridgeRunUpstream502IsTransient: an upstream 5xx classifies as agent.KindTransient (retried,
// eventually surfaced) rather than fatal.
func TestBridgeRunUpstream502IsTransient(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, http.StatusBadGateway)
	defer srv.Close()
	setupRegistry(t, srv)

	dir := t.TempDir()
	_, err := Run(context.Background(), Options{
		Provider: "tgem", Cwd: dir, Task: "t", Timeout: 2 * time.Second,
	}, io.Discard, io.Discard)
	var ae *agent.Error
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an *agent.Error", err)
	}
	if ae.Kind != agent.KindTransient {
		t.Errorf("kind = %v, want KindTransient", ae.Kind)
	}
}

// setupGeminiProviderOverride overlays the built-in "gemini" provider itself (not a side name) onto srv,
// so a bridge call with Provider=="" (the no-explicit-flag case) still resolves to a fake endpoint.
func setupGeminiProviderOverride(t *testing.T, srv *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("TEST_GEMINI_DEFAULT_KEY", "gem-default-key")
	body := fmt.Sprintf(`{"providers":{
	  "gemini":{"kind":"openai","base_url":%q,"auth":{"type":"bearer","env":"TEST_GEMINI_DEFAULT_KEY"},"models":{"opus":"g-pro","sonnet":"g-flash","haiku":"g-lite"}}}}`,
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
	marker := vendors.AgyLoginMarker(home)
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
	_, err := Run(context.Background(), Options{Cwd: dir, Task: "t"}, io.Discard, io.Discard)
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
	_, err := Run(context.Background(), Options{Cwd: dir, Task: "t"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "agy is not signed in") {
		t.Fatalf("err = %v, want an agy-not-signed-in error", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (no API fallback)", len(hits))
	}
}

// TestBridgeRunExplicitProviderSkipsAgy: an explicit --provider stays on the api runner even with a
// cached agy login and installed agy — only the no-flag default (Provider=="") considers agy at all.
func TestBridgeRunExplicitProviderSkipsAgy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))

	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)
	sentinel := filepath.Join(t.TempDir(), "ran")
	fakeAgyScript(t, sentinel)

	dir := t.TempDir()
	res, err := Run(context.Background(), Options{Provider: "tgem", Cwd: dir, Task: "t"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want 1 (explicit provider stays on api)", len(hits))
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Error("agy sentinel exists: the fake agy binary ran despite an explicit --provider")
	}
	if res.Provider != "tgem" {
		t.Errorf("provider = %q, want tgem", res.Provider)
	}
}

// TestBridgeRunAgyBackendWhenLoginPresent: Provider=="" with a cached agy login and installed agy routes
// through agent.RunAgy instead of the api runner — zero HTTP hits, the agy backend log line, and the
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
	res, err := Run(context.Background(), Options{Cwd: dir, Task: "t"}, io.Discard, &log)
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
// (model, stream-json both ways) instead of the api runner's POST line, and sends nothing over the wire.
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
	res, err := Run(context.Background(), Options{Cwd: dir, Task: "t", PrintCommand: true}, io.Discard, &log)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 (print-command sends nothing)", len(hits))
	}
	if !strings.Contains(log.String(), "agy --model "+agent.AgyDefaultModel) || !strings.Contains(log.String(), "stream-json") {
		t.Errorf("log missing the resolved agy argv:\n%s", log.String())
	}
	if res.Provider != "agy" {
		t.Errorf("provider = %q, want agy", res.Provider)
	}
}
