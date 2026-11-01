package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/claude" // registers the "claude" engine the bridge's --provider path runs on
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// runBridge runs RunGemini on provider "tgem" against a fresh upstream (status 0 = healthy).
func runBridge(t *testing.T, status int, o GeminiOptions) (agent.Result, []gotRequest, string, error) {
	t.Helper()
	var hits []gotRequest
	srv := newServer(&hits, status)
	t.Cleanup(srv.Close)
	setupRegistry(t, srv)
	var log strings.Builder
	o.Provider = "tgem"
	res, err := RunGemini(context.Background(), o, io.Discard, &log)
	return res, hits, log.String(), err
}

// TestBridgeRequestContract: one --provider call sends the table model, bearer auth, the inlined file, the rubric-framed
// task and the index naming the file that did not fit, and returns answer, usage and the inlined-path log.
func TestBridgeRequestContract(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")
	rubric := filepath.Join(t.TempDir(), "rubric.md")
	write(t, rubric, "no bare excepts")

	res, hits, log, err := runBridge(t, 0, GeminiOptions{
		Cwd: dir, Dirs: []string{"."}, Task: "TASKMARKER987", Rubric: rubric, Index: true, MaxFiles: 1,
	})
	if err != nil || len(hits) != 1 {
		t.Fatalf("err = %v, hits = %d, want nil and 1", err, len(hits))
	}
	prompt := promptContent(t, hits[0])
	for _, want := range []string{`<file path="a.go"`, "Repository index", "b.go", "TASKMARKER987"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, `<file path="b.go"`) || !strings.HasPrefix(taskSection(prompt), "You are reviewing against a fixed standard") {
		t.Errorf("b.go must be indexed only and the task must lead with the rubric framing:\n%s", prompt)
	}
	if m := decodeBody(t, hits[0]).Model; m != "g-flash" || hits[0].auth != "Bearer gem-key" {
		t.Errorf("model = %q, auth = %q, want g-flash and Bearer gem-key", m, hits[0].auth)
	}
	if res.Answer != "hello" || res.Usage != (agent.Usage{InputTokens: 5, OutputTokens: 2}) {
		t.Errorf("answer = %q, usage = %+v, want hello and {5 2}", res.Answer, res.Usage)
	}
	if !strings.Contains(log, "2 path(s) indexed, 1 inlined") || !strings.Contains(log, "  a.go\n") {
		t.Errorf("log missing the stats line and inlined path:\n%s", log)
	}
}

// TestBridgeIndexOnlyInlinesNoBodies: IndexOnly sends the index of every path and no <file> body.
func TestBridgeIndexOnlyInlinesNoBodies(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	write(t, filepath.Join(dir, "b.go"), "package b")
	_, hits, _, err := runBridge(t, 0, GeminiOptions{Cwd: dir, Dirs: []string{"."}, Task: "t", Index: true, IndexOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	prompt := promptContent(t, hits[0])
	if strings.Contains(prompt, "<file path=") || !strings.Contains(prompt, "a.go") || !strings.Contains(prompt, "b.go") {
		t.Errorf("want both paths in the index and no file body:\n%s", prompt)
	}
}

// TestBridgeDiffInlinesOnlyChanged: --diff sends only files changed vs HEAD and refuses once nothing changed.
func TestBridgeDiffInlinesOnlyChanged(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	write(t, filepath.Join(repo, "base.txt"), "base content")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "T")
	git("add", ".")
	git("commit", "-m", "base")
	write(t, filepath.Join(repo, "new.txt"), "untracked content")

	_, hits, _, err := runBridge(t, 0, GeminiOptions{Cwd: repo, Task: "t", Diff: true, MaxFiles: -1})
	if err != nil {
		t.Fatal(err)
	}
	if p := promptContent(t, hits[0]); !strings.Contains(p, `<file path="new.txt"`) || strings.Contains(p, `<file path="base.txt"`) {
		t.Errorf("want only new.txt inlined:\n%s", p)
	}
	git("add", ".")
	git("commit", "-m", "new")
	if _, _, _, err = runBridge(t, 0, GeminiOptions{Cwd: repo, Task: "t", Diff: true}); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Errorf("err = %v, want 'nothing changed'", err)
	}
}

// TestBridgeModelAliasPin: the default model ignores the vault sonnet pin; an explicit --model sonnet takes it.
func TestBridgeModelAliasPin(t *testing.T) {
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv)
	vaulttest.SetRecord(t, "claude", "aliases.sonnet", "claude-sonnet-5")
	for i, tc := range []struct{ model, want string }{{"", "g-flash"}, {"sonnet", "claude-sonnet-5"}} {
		o := GeminiOptions{Provider: "tgem", Model: tc.model, Cwd: t.TempDir(), Task: "t"}
		if _, err := RunGemini(context.Background(), o, io.Discard, io.Discard); err != nil || len(hits) != i+1 {
			t.Fatalf("Run(%q) = %v, hits = %d", tc.model, err, len(hits))
		}
		if got := decodeBody(t, hits[i]).Model; got != tc.want {
			t.Errorf("--model %q sent model %q, want %q", tc.model, got, tc.want)
		}
	}
}

const bigFile = 200 << 10

// TestBridgeLargeFileAccepted: a 200 KiB inlined file is sent whole, not capped by the argv prompt limit.
func TestBridgeLargeFileAccepted(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.txt"), strings.Repeat("x", bigFile))
	res, hits, _, err := runBridge(t, 0, GeminiOptions{Cwd: dir, Dirs: []string{"."}, Task: "t", MaxFiles: -1, MaxFileBytes: 2 * bigFile})
	if err != nil || res.Answer != "hello" {
		t.Fatalf("Run = (%q, %v), want (hello, nil)", res.Answer, err)
	}
	if p := promptContent(t, hits[0]); !strings.Contains(p, `<file path="big.txt"`) || len(p) < bigFile {
		t.Errorf("big.txt not inlined whole (prompt %d bytes)", len(p))
	}
}

// TestBridgeUpstream502IsTransient: an upstream 502 is retried and surfaces as a KindTransient *agent.Error.
func TestBridgeUpstream502IsTransient(t *testing.T) {
	fastBackoff(t)
	_, hits, _, err := runBridge(t, http.StatusBadGateway, GeminiOptions{Cwd: t.TempDir(), Task: "t", Timeout: 2 * time.Second})
	var ae *agent.Error
	if !errors.As(err, &ae) || ae.Kind != agent.KindTransient {
		t.Fatalf("err = %v, want a transient *agent.Error", err)
	}
	if len(hits) < 2 {
		t.Errorf("hits = %d, want a retry (>= 2)", len(hits))
	}
}

// TestBridgeExplicitProviderSkipsAgy: --provider stays on the API path even with a cached agy login and agy installed.
func TestBridgeExplicitProviderSkipsAgy(t *testing.T) {
	t.Setenv("HOME", loginHome(t, true))
	sentinel := filepath.Join(t.TempDir(), "ran")
	var hits []gotRequest
	srv := newServer(&hits, 0)
	defer srv.Close()
	setupRegistry(t, srv) // sets CLAUDE_CONFIG_DIR, so the fake agy lands under it
	fakeAgyScript(t, sentinel)

	res, err := RunGemini(context.Background(), GeminiOptions{Provider: "tgem", Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil || len(hits) != 1 || res.Provider != "tgem" {
		t.Fatalf("Run = (%q, %v), hits = %d, want provider tgem, nil, 1 hit", res.Provider, err, len(hits))
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Error("agy ran despite an explicit --provider")
	}
}
