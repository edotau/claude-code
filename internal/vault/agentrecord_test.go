package vault

import (
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
)

func TestAgentRecordsNullRemainsWritable(t *testing.T) {
	useZeroRootVault(t)
	if err := Update(func(tx *Tx) error { return tx.Put(Overlay, AgentRecordsKey, "null") }); err != nil {
		t.Fatal(err)
	}
	if err := View(func(tx *Tx) error {
		recs, err := tx.AgentRecords()
		if err != nil {
			return err
		}
		if recs == nil || len(recs) != 0 {
			t.Fatalf("AgentRecords = %#v, want an empty, writable map", recs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := AgentRecord{Profile: "dev", Model: "new-model"}
	if err := SetAgentRecord("claude", want); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("claude"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored record = %+v, want %+v", got, want)
	}
}

func TestFallbackRoundTrip(t *testing.T) {
	useZeroRootVault(t)
	if err := SetAgentRecord("codex", AgentRecord{Fallback: "dev"}); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex"); got.Fallback != "dev" {
		t.Fatalf("fallback-only record dropped: %+v", got)
	}
}

func TestAgentNameListsTieToAgentid(t *testing.T) {
	keys := slices.Sorted(maps.Keys(OwnLoginProfiles))
	if want := slices.Sorted(slices.Values(agentid.External())); !slices.Equal(keys, want) {
		t.Fatalf("OwnLoginProfiles keys = %v, want agentid.External() %v", keys, want)
	}
}

func TestEmptyLaunchBlockSurvivesRoundTrip(t *testing.T) {
	useZeroRootVault(t)
	want := AgentRecord{Launch: &AgentLaunch{}}
	if want.IsZero() {
		t.Fatal("a record with a non-nil empty launch block is zero")
	}
	if err := SetAgentRecord("codex", want); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex"); got.Launch == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored record = %+v, want %+v", got, want)
	}
}

func TestSettingsBlocksRoundTrip(t *testing.T) {
	useZeroRootVault(t)
	want := AgentRecord{
		Profile:     "chatgpt",
		Permissions: &AgentPolicy{Mode: "auto", Allow: []string{"Bash(ls:*)"}, Ask: []string{"Edit"}, Deny: []string{"Bash(rm:*)"}},
		Launch:      &AgentLaunch{Env: []string{"X=1"}, Args: []string{"--flag"}},
		Extras:      map[string]string{"approvalPolicy": "never", "sandboxMode": ""},
	}
	if err := SetAgentRecord("codex", want); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored record = %+v, want %+v", got, want)
	}
}

func TestRecordWithoutNewBlocksKeepsItsBytes(t *testing.T) {
	b, err := json.Marshal(AgentRecord{Profile: "dev", Model: "m", Effort: "high", Fallback: "x", Aliases: map[string]string{"opus": "o"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"profile":"dev","model":"m","effort":"high","fallback":"x","aliases":{"opus":"o"}}`
	if string(b) != want {
		t.Fatalf("record JSON = %s, want %s", b, want)
	}
}

// cachedRecords counts the vault opens behind ReadAgentRecords and advances past the stamp cache's racy window.
func cachedRecords(t *testing.T) *int {
	t.Helper()
	opens, orig := new(int), loadAgentRecords
	loadAgentRecords = func() (map[string]AgentRecord, error) { *opens++; return orig() }
	t.Cleanup(func() { loadAgentRecords = orig })
	t.Cleanup(paths.SetStampNowForTest(func() time.Time { return time.Now().Add(time.Hour) }))
	return opens
}

// A record written in this process is what the next read returns, though the earlier read was cached.
func TestReadAgentRecordsSeesAWrite(t *testing.T) {
	useZeroRootVault(t)
	if err := SetAgentRecord("codex", AgentRecord{Profile: "a"}); err != nil {
		t.Fatal(err)
	}
	opens := cachedRecords(t)
	for range 2 {
		if got := GetAgentRecord("codex").Profile; got != "a" {
			t.Fatalf("profile = %q, want a", got)
		}
	}
	if *opens != 1 {
		t.Fatalf("two settled reads opened the vault %d times, want 1 (the cache)", *opens)
	}
	if err := SetAgentRecord("codex", AgentRecord{Profile: "b"}); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex").Profile; got != "b" {
		t.Errorf("read after a write = %q, want b", got)
	}
}

// A vault file replaced by another process (stamp changed) is read fresh, not served from the cache.
func TestReadAgentRecordsSeesAnExternalWrite(t *testing.T) {
	useZeroRootVault(t)
	if err := SetAgentRecord("codex", AgentRecord{Profile: "external"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAgentRecord("codex", AgentRecord{Profile: "local"}); err != nil {
		t.Fatal(err)
	}
	opens := cachedRecords(t)
	if got := GetAgentRecord("codex").Profile; got != "local" || *opens != 1 {
		t.Fatalf("profile = %q after %d opens, want local", got, *opens)
	}
	if err := os.WriteFile(Path()+".new", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(Path()+".new", Path()); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex").Profile; got != "external" {
		t.Errorf("read after the file was replaced = %q, want external", got)
	}
}

// A caller editing a returned record's maps, slices or pointers never changes what the cache hands out next.
func TestReadAgentRecordsReturnsACopy(t *testing.T) {
	useZeroRootVault(t)
	want := AgentRecord{Aliases: map[string]string{"opus": "o"}, Extras: map[string]string{"k": "v"},
		Permissions: &AgentPolicy{Allow: []string{"Read"}, Ask: []string{"Edit"}, Deny: []string{"Bash"}},
		Launch:      &AgentLaunch{Env: []string{"X=1"}, Args: []string{"--a"}}}
	if err := SetAgentRecord("claude", want); err != nil {
		t.Fatal(err)
	}
	opens := cachedRecords(t)
	got := GetAgentRecord("claude")
	got.Aliases["opus"], got.Extras["k"] = "edited", "edited"
	got.Permissions.Allow[0], got.Permissions.Ask[0], got.Permissions.Deny[0] = "edited", "edited", "edited"
	got.Permissions.Mode, got.Launch.Env[0], got.Launch.Args[0] = "edited", "edited", "edited"
	if again := GetAgentRecord("claude"); !reflect.DeepEqual(again, want) || *opens != 1 {
		t.Errorf("cached record after a caller's edits = %+v (%d opens), want %+v from the cache", again, *opens, want)
	}
}
