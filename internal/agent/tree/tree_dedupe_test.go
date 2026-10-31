package tree

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

const (
	dedupeCwd    = "/Users/fake/dedupe-repo"
	dedupeSlug   = "-Users-fake-dedupe-repo"
	headlessPrmt = "Summarize the flaky retry logic in the router and propose a fix"
)

type dedupeEnv struct {
	cfg string
	now time.Time
}

// newDedupeEnv points CLAUDE_CONFIG_DIR at a temp dir holding projects/ and state/harness/dispatch.
func newDedupeEnv(t *testing.T) dedupeEnv {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	return dedupeEnv{cfg: cfg, now: time.Date(2026, 10, 18, 3, 10, 0, 0, time.UTC)}
}

// session writes an active transcript (first user prompt = prompt); withSub adds a running subagent.
func (e dedupeEnv) session(t *testing.T, id, prompt string, withSub bool) {
	t.Helper()
	dir := filepath.Join(e.cfg, "projects", dedupeSlug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": dedupeCwd, "sessionId": id, "timestamp": e.now.Format(time.RFC3339),
		"message": map[string]any{"role": "user", "content": prompt},
	})
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, e.now, e.now); err != nil {
		t.Fatal(err)
	}
	if !withSub {
		return
	}
	sub := filepath.Join(dir, id, "subagents")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"agent-sub1.jsonl":     `{"type":"user","timestamp":"` + e.now.Format(time.RFC3339) + `"}` + "\n",
		"agent-sub1.meta.json": `{"finished":false}`,
	} {
		p := filepath.Join(sub, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, e.now, e.now); err != nil {
			t.Fatal(err)
		}
	}
}

// dispatch saves a live (this pid) running record started a minute before now; returns its id.
func (e dedupeEnv) dispatch(t *testing.T, name, prompt, workDir, repo, sessionID string) string {
	t.Helper()
	req := agent.RunOptions{Prompt: prompt, WorkDir: workDir}
	rec := agent.NewRecord(name, req, e.now.Add(-time.Minute))
	rec.PID, rec.Repo, rec.SessionID = os.Getpid(), repo, sessionID
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func (e dedupeEnv) render(t *testing.T, format string) string {
	t.Helper()
	var out, errw bytes.Buffer
	opts := Options{Days: 60, Running: true, Now: pinNow(e.now)}
	if code := renderOnce(e.cfg, opts, format, true, &out, &errw); code != 0 {
		t.Fatalf("renderOnce exit %d: %s", code, errw.String())
	}
	return out.String()
}

func (e dedupeEnv) tree(t *testing.T) Tree {
	t.Helper()
	var tree Tree
	if err := json.Unmarshal([]byte(e.render(t, "json")), &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

func hasSession(tree Tree, id string) bool {
	for _, s := range tree.Sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

// A headless claude dispatch matched by cwd + time + prompt renders once, as its dispatch row (text and JSON);
// an unrelated session in the same dir (different prompt) stays.
func TestRunningDedupesHeadlessClaudeByPrompt(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-aaaa", headlessPrmt, false)
	e.session(t, "human-bbbb", "Add a new feature to the statusline", false)
	id := e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "")

	tree := e.tree(t)
	if hasSession(tree, "headless-aaaa") {
		t.Error("matched headless session should be dropped from sessions")
	}
	if !hasSession(tree, "human-bbbb") {
		t.Error("unrelated session in the same dir must not be dropped")
	}
	if len(tree.Dispatches) != 1 || tree.Dispatches[0].ID != id {
		t.Errorf("dispatches = %+v, want only %s", tree.Dispatches, id)
	}

	text := e.render(t, "text")
	if strings.Contains(text, "headless-aaaa") || strings.Count(text, id) != 1 {
		t.Errorf("text should show the dispatch once (dispatch row only):\n%s", text)
	}
	if !strings.Contains(text, "Add a new feature") {
		t.Errorf("text lost the unrelated session:\n%s", text)
	}
}

// The record's SessionID alone matches, even when the prompt differs.
func TestRunningDedupesBySessionID(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-cccc", "something else entirely", false)
	e.dispatch(t, "claude", headlessPrmt, "", "/elsewhere", "headless-cccc")
	if hasSession(e.tree(t), "headless-cccc") {
		t.Error("session whose id equals the dispatch SessionID should be dropped")
	}
}

// A matching session that has subagents is real work and is never dropped.
func TestRunningKeepsMatchedSessionWithSubagents(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-dddd", headlessPrmt, true)
	e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "headless-dddd")
	tree := e.tree(t)
	if !hasSession(tree, "headless-dddd") || len(tree.Dispatches) != 1 {
		t.Errorf("session with subagents must stay: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}

// A non-claude dispatch never drops a session, even with a matching cwd and prompt.
func TestRunningCodexDispatchDoesNotDropSession(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-eeee", headlessPrmt, false)
	e.dispatch(t, "codex", headlessPrmt, dedupeCwd, dedupeCwd, "headless-eeee")
	tree := e.tree(t)
	if !hasSession(tree, "headless-eeee") || len(tree.Dispatches) != 1 {
		t.Errorf("codex dispatch must not dedupe: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}

// A session last written before the dispatch started (but still active) cannot be its child.
func TestRunningKeepsSessionOlderThanDispatch(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-ffff", headlessPrmt, false)
	old := e.now.Add(-100 * time.Second) // active (<2m) yet before the dispatch start (-60s)
	if err := os.Chtimes(filepath.Join(e.cfg, "projects", dedupeSlug, "headless-ffff.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	e.dispatch(t, "claude", headlessPrmt, dedupeCwd, dedupeCwd, "")
	if !hasSession(e.tree(t), "headless-ffff") {
		t.Error("session older than the dispatch start must stay")
	}
}

// Known gap: WorkDir empty and the job ran in a linked worktree, so Repo (main checkout) != session cwd;
// the dispatch fails safe and stays listed twice.
func TestRunningWorktreeGapStaysListedTwice(t *testing.T) {
	e := newDedupeEnv(t)
	e.session(t, "headless-gggg", headlessPrmt, false)
	e.dispatch(t, "claude", headlessPrmt, "", "/Users/fake/main-checkout", "")
	tree := e.tree(t)
	if !hasSession(tree, "headless-gggg") || len(tree.Dispatches) != 1 {
		t.Errorf("gap case should list twice: sessions=%d dispatches=%d", len(tree.Sessions), len(tree.Dispatches))
	}
}
