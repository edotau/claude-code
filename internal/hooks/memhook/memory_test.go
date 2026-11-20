package memhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/memtest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
	"github.com/edotau/claude-code/internal/worktree/worktreetest"
)

// TestMain stubs every detached spawn and the process scan: no memhook test starts a process or reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	scan := worktree.ProcessScan
	worktreetest.StubProcessScanForTest(scan.In, scan.Up, scan.Procs, scan.Agents)
	tb := &hosttest.MainTB{}
	memtest.StubSpawn(tb, true)
	orig := spawnServed
	spawnServed = func(string) bool { return true }
	code := m.Run()
	spawnServed = orig
	for _, f := range tb.Cleanups {
		f()
	}
	cleanup()
	os.Exit(code)
}

// resumeSession is a hermetic repo whose handoffs dir holds a fresh and a stale note.
func resumeSession(t *testing.T) (cwd, fresh string) {
	t.Helper()
	vaulttest.Use(t)
	memtest.StubSpawn(t, true)
	swap(t, &stewardWait, 50*time.Millisecond)
	cwd = t.TempDir()
	dir := paths.MemorySubDir("handoffs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "old.md")
	fresh = filepath.Join(dir, "2026-11-07-session-s1.md")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("<!-- session: s1 -->\nx"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.MemoryStateDir(), "handoff-s1.done"), []byte("s1\n"+fresh+"\nrequest"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cwd, fresh
}

func startInput(cwd, source string) *core.Input {
	return core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"source":%q}`, cwd, source)))
}

func TestPreCompactRecordRoundTripsIntoSessionStart(t *testing.T) {
	cwd, fresh := resumeSession(t)
	if err := os.WriteFile(fresh, []byte("<!-- session: s1 -->\n"+strings.Repeat("x", 20000)), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":182000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	pre := fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q,"trigger":"auto"}`, cwd, tr)
	Compact(core.ParseInput(strings.NewReader(pre)))
	rec := filepath.Join(paths.StateDir(), "compact", "s1.json")
	if fi, err := os.Stat(rec); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("compact record missing or not 0600: %v", err)
	}
	r := Start(startInput(cwd, "compact"))
	if !strings.Contains(r.SystemMessage, "compacted (auto, 91%) at ") || !strings.Contains(r.SystemMessage, "; handoff: "+fresh) {
		t.Errorf("SystemMessage = %q", r.SystemMessage)
	}
	if !strings.Contains(r.Context, r.SystemMessage) {
		t.Errorf("the line must land in Context too: %q", r.Context)
	}
	if !strings.Contains(r.Context, "Session handoff ("+fresh+"):\n<!-- session: s1 -->\nx") || strings.Index(r.Context, "Session handoff") > strings.Index(r.Context, "compacted") {
		t.Errorf("Context must open with this session's handoff body: %q", r.Context)
	}
	if !strings.Contains(r.Context, "[handoff truncated; read the path above]") || len(r.Context) > core.ContextCapChars {
		t.Error("handoff budget lost its notice or exceeded context cap")
	}
	if strings.Contains(r.SystemMessage, "old.md") {
		t.Error("a handoff older than 24h must be ignored")
	}
	if _, err := os.Stat(rec); err == nil {
		t.Error("SessionStart(compact) must delete the record")
	}
}

func TestSessionStartResumeNotes(t *testing.T) {
	cwd, fresh := resumeSession(t)
	if got := Start(startInput(cwd, "clear")).SystemMessage; got != "handoff: "+fresh {
		t.Errorf("clear = %q", got)
	}
	for _, src := range []string{"startup", "resume"} {
		if got := Start(startInput(cwd, src)).SystemMessage; strings.Contains(got, "handoff") {
			t.Errorf("%s must be unchanged, got %q", src, got)
		}
	}
}

func recall(t *testing.T, prompt, cwd string) string {
	t.Helper()
	return recallAs(t, "", prompt, cwd)
}

func recallAs(t *testing.T, session, prompt, cwd string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"prompt": prompt, "cwd": cwd, "session_id": session})
	res := Recall(core.ParseInput(bytes.NewReader(raw)))
	if res.Decision == core.Block {
		t.Fatalf("Recall blocked: %q", res.Reason)
	}
	return res.Context
}

func TestMemoryRecall(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cwd := t.TempDir()
	long := strings.Repeat("alpha bravo charlie delta padding ", 40)
	body := "# Conventions\n\n- alpha bravo charlie delta strong match " + long + "\n- alpha weak\n- unrelated rule entirely\n"
	writeBank(t, "conventions.md", body)
	for _, p := range []string{"", "/compact alpha bravo charlie delta", "alpha bravo charlie", "zzz yyy xxx www vvv"} {
		if got := recall(t, p, cwd); got != "" {
			t.Errorf("prompt %q must be silent, got %q", p, got)
		}
	}
	got := recall(t, "alpha bravo charlie delta", cwd)
	if !strings.HasPrefix(got, "<!-- memory-recall;") || !strings.Contains(got, "[conventions.md] z") || !strings.Contains(got, " · alpha bravo charlie delta strong match") {
		t.Fatalf("recall block: %q", got)
	}
	if strings.Contains(got, "alpha weak") || len([]rune(got)) > 1500 || !strings.Contains(got, "…") {
		t.Errorf("floor, total cap or per-hit cap not applied (%d chars): %q", len([]rune(got)), got)
	}
	t.Setenv(memory.UpdateChildEnv, "1")
	if recall(t, "alpha bravo charlie delta", cwd) != "" {
		t.Error("an update worker's prompts must not recall")
	}
}

func TestMemoryRecallSkipsWhatIsInContext(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cwd := t.TempDir()
	for name, body := range map[string]string{
		"activeContext.md": "# Active\n\n## Current Focus\n\n- alpha bravo charlie delta already in context\n",
		"conventions.md":   "# C\n\n- alpha bravo charlie delta echo fresh\n- echo only one shared term\n",
	} {
		if err := os.MkdirAll(paths.MemoryStoreDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.MemoryStoreDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// session_id must be UUID-shaped: resetSeen only fires for a real session id (memory.LooksLikeSessionID).
	const sessionS = "44444444-4444-4444-8444-444444444444"
	const sessionT = "55555555-5555-4555-8555-555555555555"
	raw, _ := json.Marshal(map[string]string{"cwd": cwd, "session_id": sessionS})
	if Start(core.ParseInput(bytes.NewReader(raw))).Decision == core.Block {
		t.Fatal("SessionStart failed")
	}
	got := recallAs(t, sessionS, "alpha bravo charlie delta echo", cwd)
	if !strings.Contains(got, "echo fresh") || strings.Contains(got, "already in context") || strings.Contains(got, "one shared term") {
		t.Fatalf("want only the fresh, multi-term block: %q", got)
	}
	if again := recallAs(t, sessionS, "alpha bravo charlie delta echo", cwd); again != "" {
		t.Errorf("a block recalled once must not be re-injected: %q", again)
	}
	if other := recallAs(t, sessionT, "alpha bravo charlie delta echo", cwd); !strings.Contains(other, "already in context") {
		t.Errorf("another session's context is not this one's: %q", other)
	}
}

func writeBank(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(paths.MemoryStoreDir(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func agentIn(t *testing.T, payload string) *core.Input {
	t.Helper()
	return core.ParseInput(strings.NewReader(payload))
}

func TestAgentInputClaudePassesThrough(t *testing.T) {
	in := agentIn(t, `{"session_id":"s9","cwd":"/r","transcript_path":"/t.jsonl"}`)
	if out := AgentInput(in); out != in {
		t.Fatal("a Claude-shaped input must come back untouched")
	}
}

func TestAgentInputFailOpenReturnsInput(t *testing.T) {
	cwd := hosttest.FakeOpencode(t, "exit 1")
	missing := filepath.Join(t.TempDir(), "events.jsonl")
	for name, payload := range map[string]string{
		"opencode export fails":  `{"session_id":"ses_x2","cwd":"` + cwd + `","harness_agent":"opencode"}`,
		"copilot events missing": `{"sessionId":"stable-copilot","cwd":"/r","transcriptPath":"` + missing + `"}`,
	} {
		if in := agentIn(t, payload); AgentInput(in) != in {
			t.Errorf("%s: a failed load must return the same input", name)
		}
	}
}

func TestAgentKind(t *testing.T) {
	cases := []struct {
		name, env, payload, want string
	}{
		{"env opencode", memory.SourceOpencode, `{"session_id":"s","transcript_path":"/t"}`, memory.SourceOpencode},
		{"harness_agent opencode", "", `{"session_id":"s","harness_agent":"opencode"}`, memory.SourceOpencode},
		{"copilot with transcript", "", `{"sessionId":"c","transcriptPath":"/e.jsonl"}`, memory.SourceCopilot},
		{"copilot without transcript", "", `{"sessionId":"c"}`, ""},
		{"plain claude", "", `{"session_id":"s","transcript_path":"/t"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(settings.AgentEnv, tc.env)
			if got := agentKind(agentIn(t, tc.payload)); got != tc.want {
				t.Fatalf("agentKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// An agent hand-back is not a human turn: recall stays silent, while typed text beside it still recalls.
func TestMemoryRecallSkipsAutomatedDelivery(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	cwd := t.TempDir()
	writeBank(t, "conventions.md", "# Conventions\n\n- alpha bravo charlie delta strong match\n")
	msg := "Another Claude session sent a message:\n<agent-message from=\"a1\">\nalpha bravo charlie delta\n</agent-message>"
	if got := recall(t, msg, cwd); got != "" {
		t.Errorf("hand-back must be silent, got %q", got)
	}
	if got := recall(t, msg+"\n\nalpha bravo charlie delta", cwd); got == "" {
		t.Error("a prompt with typed text must still recall")
	}
}

func updatePayload(tr, cwd string) string {
	return fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q}`, cwd, tr)
}

// hookSession is a hermetic session whose transcript the caller fills; payload is its Stop/End/Compact input.
func hookSession(t *testing.T) (tr string, payload func() *core.Input) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(memory.UpdateChildEnv, "")
	tr, cwd := filepath.Join(t.TempDir(), "t.jsonl"), t.TempDir()
	return tr, func() *core.Input { return core.ParseInput(strings.NewReader(updatePayload(tr, cwd))) }
}

// A failed Stop spawn never blocks: it proceeds with the not-saved note and leaves the work for SessionEnd.
func TestFailedWorkerSpawnProceedsWithNote(t *testing.T) {
	tr, payload := hookSession(t)
	hosttest.WriteEdits(t, tr, 5)
	memtest.StubSpawn(t, false)
	for i := range 3 {
		if r := Stop(payload()); r.Decision == core.Block || r.SystemMessage != updateNotSaved {
			t.Fatalf("Stop %d = %+v, want proceed with %q", i, r, updateNotSaved)
		}
	}
	spawns := memtest.StubSpawn(t, true)
	End(payload())
	if got := spawns.Update.Load(); got != 1 {
		t.Fatalf("SessionEnd dispatched %d updates after failed Stops, want 1", got)
	}
}

func TestEndAndCompactDispatchOnce(t *testing.T) {
	for name, hook := range map[string]func(*core.Input) core.Result{"End": End, "Compact": Compact} {
		t.Run(name, func(t *testing.T) {
			tr, payload := hookSession(t)
			hosttest.WriteEdits(t, tr, 3)
			spawns := memtest.StubSpawn(t, true)
			swap(t, &stewardWait, 50*time.Millisecond)
			for range 2 {
				if r := hook(payload()); !reflect.DeepEqual(r, core.Result{}) {
					t.Fatalf("%s = %+v, want an empty result", name, r)
				}
			}
			if got := spawns.Update.Load(); got != 1 {
				t.Fatalf("%s dispatched %d updates, want 1", name, got)
			}
		})
	}
}

// An update in a linked worktree dispatches the background worker and logs one ledger line.
func TestUpdateAppendsWorktreeLedger(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", home+"/.claude")
	t.Setenv(memory.UpdateChildEnv, "")
	main := filepath.Join(home, "repo")
	for _, a := range [][]string{{"init", "-q", "-b", "main", main}, {"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-q", "-m", "i"}} {
		if out, err := exec.Command("git", a...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", a, err, out)
		}
	}
	repo, err := worktree.ResolveRepo(main)
	if err != nil {
		t.Fatal(err)
	}
	dest, err := worktree.Create(repo, "wt-h", worktree.CreateOptions{Log: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	spawns := memtest.StubSpawn(t, true)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	hosttest.WriteEdits(t, tr, 3)
	if res := Stop(core.ParseInput(strings.NewReader(updatePayload(tr, dest)))); res.Decision == core.Block || spawns.Update.Load() != 1 {
		t.Fatalf("update = %+v, spawns=%d; want not blocked and one background worker", res, spawns.Update.Load())
	}
	data, err := os.ReadFile(paths.MemorySub("worktrees", time.Now().Format("2006-01-02")+"-wt-h.md"))
	if err != nil || !strings.Contains(string(data), "update (") || !strings.Contains(string(data), "3 edits, session s1") {
		t.Fatalf("no update line in ledger: %v\n%s", err, data)
	}
}

func TestStopBelowUpdateThresholdRefreshesOnce(t *testing.T) {
	tr, payload := hookSession(t)
	hosttest.WriteEdits(t, tr, 1)
	writeBank(t, "conventions.md", "# Conventions\n\n- Keep recall fresh.\n")
	spawns := memtest.StubSpawn(t, true)
	for range 2 {
		Stop(payload())
	}
	if updates, refreshes := spawns.Update.Load(), spawns.Refresh.Load(); updates != 0 || refreshes != 1 {
		t.Fatalf("updates=%d refreshes=%d; want 0 and 1", updates, refreshes)
	}
}

func TestOpenPlansLineNamesOnlyOpenPlans(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, ".git"), 0o755); err != nil || os.WriteFile(filepath.Join(cfg, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644) != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format(time.DateOnly)
	plan := func(slug, body string) {
		if err := os.WriteFile(filepath.Join(cfg, "plans", day+"-"+slug+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plan("open", "- [x] Phase 1.1 one\n- [ ] Phase 1.2 two\n")
	plan("closed", "- [x] Phase 1.1 one\n")
	got := openPlansLine(cfg)
	if want := "plans open: " + day + "-open 1/2 → Phase 1.2"; got != want {
		t.Fatalf("openPlansLine = %q, want %q", got, want)
	}
}
