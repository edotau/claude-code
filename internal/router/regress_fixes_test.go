package router

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAliasOfColonBearingModelIDTriesFullID(t *testing.T) {
	vaulttest.Use(t)
	tb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := tb.AliasOf("claude-opus-5-5:0"); got != "opus" {
		t.Errorf("AliasOf(claude-opus-5-5:0) = %q, want opus (colon is part of the id)", got)
	}
}

func TestStatusExhaustedWithoutResetGoesStaleToUnknown(t *testing.T) {
	now := time.Now()
	stale := Entry{UsedPct: 100, Seen: now.Add(-2 * StaleAfter)}
	if got := stale.Status(now); got != StateUnknown {
		t.Errorf("stale 100%% with no reset = %q, want %q so gateAccount refreshes it", got, StateUnknown)
	}
	fresh := Entry{UsedPct: 100, Seen: now}
	if got := fresh.Status(now); got != StateExhausted {
		t.Errorf("fresh 100%% with no reset = %q, want exhausted", got)
	}
}

func TestUpdateKeepsOnlyOneCorruptBackup(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := path + ".corrupt-1"
	if err := os.WriteFile(old, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(State) {}); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".corrupt-*")
	if len(backups) != 1 {
		t.Fatalf("backups = %v, want exactly the newest one", backups)
	}
	if got, _ := os.ReadFile(backups[0]); string(got) != "{not json" {
		t.Errorf("backup holds %q, want the corrupt bytes", got)
	}
}

func TestSkipDiscoveryReportsPersistenceFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(StatePath(), 0o700); err != nil { // a directory where quota.json belongs: reads fail
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	got := SkipDiscovery("opencode", poolProvider(), "m-a")
	os.Stderr = orig
	w.Close()
	out, _ := io.ReadAll(r)
	if got != "" || !strings.Contains(string(out), "could not persist model skip") {
		t.Errorf("got %q, stderr %q; want empty result and a persistence message", got, out)
	}
}
