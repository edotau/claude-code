package memhook

import (
	"fmt"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/claude" // registers the claude row
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// servedSession seeds a hermetic vault and a CLAUDE_PROFILE=dev Databricks session (switch on or off), optionally with
// a listing fetched at fetched, and returns the count of detached refresh spawns a SessionStart makes.
func servedSession(t *testing.T, on bool, fetched time.Time, updateChild string) *atomic.Int64 {
	t.Helper()
	vaulttest.Use(t)
	t.Setenv(bank.UpdateChildEnv, updateChild)
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

// SessionStart re-lists detached only when the switch is on, a pinned Databricks profile's cache is stale, and the
// session is not a memory update child.
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
			memoryContext(t.TempDir(), "s1", "", now)
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
	memoryContext(t.TempDir(), "s1", "", now)
	memoryContext(t.TempDir(), "s2", "", now.Add(time.Minute))
	if got := calls.Load(); got != 1 {
		t.Fatalf("spawnServed calls = %d, want 1", got)
	}
}

// resumeSession is a hermetic repo whose handoffs dir holds a fresh and a stale note.
func resumeSession(t *testing.T) (cwd, fresh string) {
	t.Helper()
	vaulttest.Use(t)
	StubSpawnForTest(t, true)
	cwd = t.TempDir()
	dir := memstore.HandoffsDir(memstore.Dir(cwd))
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
