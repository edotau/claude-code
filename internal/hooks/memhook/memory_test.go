package memhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"github.com/edotau/claude-code/internal/worktree"
)

// TestMain stubs every detached spawn and the process scan: no memhook test starts a process or reads the machine.
func TestMain(m *testing.M) {
	cleanup := hosttest.Setup()
	worktree.StubProcessScanForTest()
	tb := &hosttest.MainTB{}
	memory.StubSpawn(tb, true)
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

// servedSession seeds a hermetic vault and a dev Databricks session; it returns the served-refresh spawn count.
func servedSession(t *testing.T, on bool, fetched time.Time, updateChild string) *atomic.Int64 {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv(memory.UpdateChildEnv, updateChild)
	row := `{"routing":{"servedAware":%t},"providers":{"databricks":{"dev":{"preset":"databricks",` +
		`"host":"https://dev.cloud.databricks.com","auth":{"type":"pat"}}}}}`
	vaulttest.Put(t, vault.Overlay, "providers", fmt.Sprintf(row, on))
	vaulttest.Secret(t, "DATABRICKS_DEV_PAT", "dapi-test")
	t.Setenv(providers.EnvSessionProfile, "dev")
	if !fetched.IsZero() {
		if err := router.SaveServed(router.Served{Profile: "dev", FetchedAt: fetched}); err != nil {
			t.Fatal(err)
		}
	}
	calls := new(atomic.Int64)
	orig := spawnServed
	spawnServed = func(string) bool { calls.Add(1); return true }
	t.Cleanup(func() { spawnServed = orig })
	return calls
}

// A start re-lists only with the switch on, a stale pinned Databricks cache, and outside an update child.
func TestSessionStartRelistsServedOnlyForStaleSessionProfile(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name        string
		on          bool
		fetched     time.Time
		updateChild string
		want        int64
	}{
		{"stale cache, switch on", true, time.Time{}, "", 1},
		{"expired cache, switch on", true, now.Add(-48 * time.Hour), "", 1},
		{"fresh cache", true, now.Add(-time.Minute), "", 0},
		{"switch off", false, time.Time{}, "", 0},
		{"update child", true, time.Time{}, "1", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := servedSession(t, c.on, c.fetched, c.updateChild)
			reg, _ := providers.Load()
			refreshServed(reg, t.TempDir(), now)
			if got := calls.Load(); got != c.want {
				t.Fatalf("spawnServed calls = %d, want %d", got, c.want)
			}
		})
	}
}

// A failing refresh leaves the cache stale; the next SessionStart inside the retry window must not spawn again.
func TestSessionStartClaimsServedRefreshOncePerRetryWindow(t *testing.T) {
	now := time.Now()
	calls := servedSession(t, true, time.Time{}, "")
	reg, _ := providers.Load()
	refreshServed(reg, t.TempDir(), now)
	refreshServed(reg, t.TempDir(), now.Add(time.Minute))
	if got := calls.Load(); got != 1 {
		t.Fatalf("spawnServed calls = %d, want 1", got)
	}
}

// resumeSession is a hermetic repo whose handoffs dir holds a fresh and a stale note.
func resumeSession(t *testing.T) (cwd, fresh string) {
	t.Helper()
	vaulttest.Use(t)
	memory.StubSpawn(t, true)
	cwd = t.TempDir()
	dir := memory.HandoffsDir(memory.Dir(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "old.md")
	fresh = filepath.Join(dir, "2026-11-07-new.md")
	for _, p := range []string{stale, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	return cwd, fresh
}

func startInput(cwd, source string) *core.Input {
	return core.ParseInput(strings.NewReader(fmt.Sprintf(`{"session_id":"s1","cwd":%q,"source":%q}`, cwd, source)))
}

func TestPreCompactRecordRoundTripsIntoSessionStart(t *testing.T) {
	cwd, fresh := resumeSession(t)
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	line := `{"type":"assistant","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":182000,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(tr, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	pre := fmt.Sprintf(`{"session_id":"s1","cwd":%q,"transcript_path":%q,"trigger":"auto"}`, cwd, tr)
	Compact(core.ParseInput(strings.NewReader(pre)))
	rec := filepath.Join(paths.StateDir(), "compact", "s1.json")
	if fi, err := os.Stat(rec); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("compact record missing or not 0600: %v", err)
	}
	r := Start(startInput(cwd, "compact"))
	if !strings.Contains(r.SystemMessage, "compacted (auto, 91%) at ") || !strings.Contains(r.SystemMessage, "; handoff: "+fresh) {
		t.Errorf("SystemMessage = %q", r.SystemMessage)
	}
	if !strings.Contains(r.Context, r.SystemMessage) {
		t.Errorf("the line must land in Context too: %q", r.Context)
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

// TestSessionStartLoadsProvidersOnce: the served refresh and the pin check share one registry load per start.
func TestSessionStartLoadsProvidersOnce(t *testing.T) {
	calls := servedSession(t, true, time.Time{}, "")
	loads := 0
	orig := loadProviders
	loadProviders = func() (*providers.Registry, error) { loads++; return orig() }
	t.Cleanup(func() { loadProviders = orig })
	Start(startInput(t.TempDir(), "startup"))
	if loads != 1 || calls.Load() != 1 {
		t.Fatalf("registry loads = %d, served refreshes = %d; want 1 and 1", loads, calls.Load())
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
	if !strings.HasPrefix(got, "<!-- memory-recall;") || !strings.Contains(got, "[home/conventions.md] z") || !strings.Contains(got, " · alpha bravo charlie delta strong match") {
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
		if err := os.MkdirAll(memory.HomeBankDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(memory.HomeBankDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// session_id must be UUID-shaped: memoryContext's resetSeen (which seeds the dedup set this test
	// exercises) only fires for a real session id, not a bare probe/test token (memory.LooksLikeSessionID).
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
	p := filepath.Join(memory.HomeBankDir(), rel)
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
