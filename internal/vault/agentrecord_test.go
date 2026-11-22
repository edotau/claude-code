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
	"github.com/edotau/claude-code/internal/route"
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
	want := AgentRecord{Model: "new-model", Route: route.Route{Profile: "dev"}}
	if err := SetAgentRecord("claude", want); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("claude"); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored record = %+v, want %+v", got, want)
	}
}

func TestFallbackRoundTrip(t *testing.T) {
	useZeroRootVault(t)
	if err := SetAgentRecord("codex", AgentRecord{FallbackRoute: route.Route{Profile: "dev"}}); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex"); got.FallbackRoute.Profile != "dev" {
		t.Fatalf("fallback-only record dropped: %+v", got)
	}
}

func TestAgentNameListsTieToAgentid(t *testing.T) {
	keys := slices.Sorted(maps.Keys(OwnLoginProfiles))
	want := slices.DeleteFunc(slices.Sorted(slices.Values(agentid.External())), func(a string) bool { return agentid.Routing(a).NoOwnLogin })
	if !slices.Equal(keys, want) {
		t.Fatalf("OwnLoginProfiles keys = %v, want agentid.External() minus the no-own-login agents %v", keys, want)
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
		Route:       route.Route{Profile: "chatgpt"},
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
	b, err := json.Marshal(AgentRecord{Model: "m", Effort: "high", Aliases: map[string]string{"opus": "o"}, Route: route.Route{Profile: "dev"}, FallbackRoute: route.Route{Profile: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"model":"m","effort":"high","route":{"profile":"dev"},"fallbackRoute":{"profile":"x"},"aliases":{"opus":"o"}}`
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
	if err := SetAgentRecord("codex", AgentRecord{Route: route.Route{Profile: "a"}}); err != nil {
		t.Fatal(err)
	}
	opens := cachedRecords(t)
	for range 2 {
		if got := GetAgentRecord("codex").Route.Profile; got != "a" {
			t.Fatalf("profile = %q, want a", got)
		}
	}
	if *opens != 1 {
		t.Fatalf("two settled reads opened the vault %d times, want 1 (the cache)", *opens)
	}
	if err := SetAgentRecord("codex", AgentRecord{Route: route.Route{Profile: "b"}}); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex").Route.Profile; got != "b" {
		t.Errorf("read after a write = %q, want b", got)
	}
}

// A vault file replaced by another process (stamp changed) is read fresh, not served from the cache.
func TestReadAgentRecordsSeesAnExternalWrite(t *testing.T) {
	useZeroRootVault(t)
	if err := SetAgentRecord("codex", AgentRecord{Route: route.Route{Profile: "external"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := SetAgentRecord("codex", AgentRecord{Route: route.Route{Profile: "local"}}); err != nil {
		t.Fatal(err)
	}
	opens := cachedRecords(t)
	if got := GetAgentRecord("codex").Route.Profile; got != "local" || *opens != 1 {
		t.Fatalf("profile = %q after %d opens, want local", got, *opens)
	}
	if err := os.WriteFile(Path()+".new", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(Path()+".new", Path()); err != nil {
		t.Fatal(err)
	}
	if got := GetAgentRecord("codex").Route.Profile; got != "external" {
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

func TestLoginRenamesKeepsCollidingRows(t *testing.T) {
	renames, warns := LoginRenames(map[string]string{"a": "Octo", "b": "octo", "c": "prod", "d": "dee"}, []string{"a", "b", "c", "d", "prod"})
	if len(renames) != 2 || renames["a"] != "Octo" || renames["d"] != "dee" || len(warns) != 2 {
		t.Errorf("renames = %v, warns = %v; want a and d renamed, b and c kept with a warning each", renames, warns)
	}
}

// An agent record naming an old GitHub row key reads back under the account login.
func TestAgentRecordsRenameOldGithubProfile(t *testing.T) {
	useZeroRootVault(t)
	const provs = `{"providers":{"github":{"business":{"login":"octo-user"}}}}`
	const recs = `{"copilot":{"route":{"profile":"business"},"fallbackRoute":{"profile":"business"},"model":"business:gpt-5","aliases":{"opus":"business:o"}}}`
	if err := Update(func(tx *Tx) error {
		if err := tx.Put(Overlay, "providers", provs); err != nil {
			return err
		}
		return tx.Put(Overlay, AgentRecordsKey, recs)
	}); err != nil {
		t.Fatal(err)
	}
	want := AgentRecord{Model: "octo-user:gpt-5", Aliases: map[string]string{"opus": "octo-user:o"}, Route: route.Route{Profile: "octo-user"}, FallbackRoute: route.Route{Profile: "octo-user"}}
	if err := View(func(tx *Tx) error {
		got, err := tx.AgentRecords()
		if err == nil && !reflect.DeepEqual(got["copilot"], want) {
			t.Errorf("copilot record = %+v, want %+v", got["copilot"], want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveRouteReadsLegacy(t *testing.T) {
	db := route.Route{Provider: "databricks", Profile: "auto"}
	cases := []struct {
		name      string
		rec       string
		want, fbk route.Route
	}{
		{"pool pin", `{"profile":"databricks"}`, db, route.Route{}},
		{"copilot account+fallback", `{"profile":"copilot","account":"me","fallback":"dev"}`,
			route.Route{Provider: "github", Account: "me"}, route.Route{Profile: "dev"}},
		{"plain profile", `{"profile":"ultra"}`, route.Route{Profile: "ultra"}, route.Route{}},
		{"copilot account+profile", `{"profile":"qa","account":"me"}`,
			route.Route{Provider: "github", Account: "me"}, route.Route{Profile: "qa"}},
		{"route object wins", `{"profile":"ultra","route":{"provider":"databricks","profile":"auto"},"fallbackRoute":{"provider":"databricks","profile":"auto"},"fallback":"dev"}`, db, db},
	}
	for _, c := range cases {
		raw, err := migrateAgentRecord(json.RawMessage(c.rec))
		if err != nil {
			t.Fatal(err)
		}
		var rec AgentRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		if got, fbk := rec.EffectiveRoute(), rec.EffectiveFallbackRoute(); got != c.want || fbk != c.fbk {
			t.Errorf("%s: got %+v / %+v, want %+v / %+v", c.name, got, fbk, c.want, c.fbk)
		}
	}
	b, _ := json.Marshal(AgentRecord{Route: db})
	if string(b) != `{"route":{"provider":"databricks","profile":"auto"}}` {
		t.Errorf("marshal = %s", b)
	}
}
