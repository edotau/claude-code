package router

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestAccountKeysUseProviderDotProfile(t *testing.T) {
	tb := testTable(t)
	want := map[string]string{"codex": "openai·chatgpt", "gemini": "google·antigravity", "copilot": "github·work-user"}
	for agent, acct := range want {
		if got := tb.Rows[agent]["sonnet"].Account; got != acct {
			t.Errorf("%s account = %q, want %q", agent, got, acct)
		}
	}
	if got := ProviderAccount("no-such-profile"); got != "databricks·no-such-profile" {
		t.Errorf("unregistered profile = %q, want databricks fallback", got)
	}
}

func TestCandidateAndSkipJSONUseAgentField(t *testing.T) {
	out, err := json.Marshal(PickResult{
		Candidates: []Candidate{{Agent: "codex"}},
		Skipped:    []Skip{{Agent: "copilot", Gate: GateExhausted, Reason: "r"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Count(s, `"agent":`) != 2 || strings.Contains(s, `"runner"`) {
		t.Errorf("JSON = %s, want two agent fields and no runner", s)
	}
}

// Negative control: marks under the retired "<agent>:<profile>" keys no longer gate or match.
func TestOldColonKeysNoLongerMatch(t *testing.T) {
	tb := testTable(t)
	old := State{"codex:chatgpt": {AuthReason: "auth rejected", AuthAt: now0}}
	if !strings.Contains(runners(pick(tb, "sonnet", "codex", old, FallbackAgents)), "codex") {
		t.Error("an old-format auth mark still gated codex")
	}
	cur := State{"openai·chatgpt": {AuthReason: "auth rejected", AuthAt: now0}}
	if strings.Contains(runners(pick(tb, "sonnet", "codex", cur, FallbackAgents)), "codex") {
		t.Error("the new-format auth mark did not gate codex")
	}
	if _, ok := old.AccountAuthMarked("openai·chatgpt", now0); ok {
		t.Error("old key matched the new account")
	}
}

func useConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestMarkAuthRoundTripAndClear(t *testing.T) {
	useConfigDir(t)
	acct := ProviderAccount("dbx-oai")
	if acct != "databricks·dbx-oai" {
		t.Fatalf("ProviderAccount = %q", acct)
	}
	if err := MarkAuth(acct, "auth needs_login"); err != nil {
		t.Fatal(err)
	}
	reason, ok := LoadState().AuthMarked(acct, time.Now())
	if !ok || reason != "auth needs_login" {
		t.Fatalf("AuthMarked = %q, %v", reason, ok)
	}
	if err := ClearAuth(acct); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadState().AuthMarked(acct, time.Now()); ok {
		t.Fatal("mark survived ClearAuth")
	}
}

func TestAuthMarkExpiresAtTTL(t *testing.T) {
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	s := State{"openai·chatgpt": {AuthReason: "r", AuthAt: at}}
	for _, c := range []struct {
		age  time.Duration
		want bool
	}{{0, true}, {AuthMarkTTL - time.Second, true}, {AuthMarkTTL, false}, {AuthMarkTTL + time.Second, false}} {
		if _, ok := s.AuthMarked("openai·chatgpt", at.Add(c.age)); ok != c.want {
			t.Errorf("age %v: marked = %v, want %v", c.age, ok, c.want)
		}
	}
	if AuthMarkTTL != 6*time.Hour {
		t.Errorf("AuthMarkTTL = %v, want 6h", AuthMarkTTL)
	}
	if _, ok := s.AuthMarked("nobody", at); ok {
		t.Error("unknown account marked")
	}
	if _, ok := (State{"x": {}}).AuthMarked("x", at); ok {
		t.Error("zero AuthAt must not read as a mark")
	}
}

func TestClearAuthNoWriteWhenUnmarked(t *testing.T) {
	useConfigDir(t)
	if err := ClearAuth("openai·chatgpt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(StatePath()); !os.IsNotExist(err) {
		t.Fatalf("ClearAuth on an empty state created quota.json (stat err %v)", err)
	}
	if err := ClearProviderAuth("dbx"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(StatePath()); !os.IsNotExist(err) {
		t.Fatal("ClearProviderAuth on an empty state created quota.json")
	}
	// An existing file with an entry but no mark stays byte-identical, mtime untouched.
	if err := Record("openai·chatgpt", 10, "5h", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(StatePath())
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(StatePath(), old, old); err != nil {
		t.Fatal(err)
	}
	_ = ClearAuth("openai·chatgpt")
	_ = ClearAuth("never-seen")
	_ = ClearProviderAuth("chatgpt")
	after, _ := os.ReadFile(StatePath())
	fi, _ := os.Stat(StatePath())
	if string(before) != string(after) || fi.ModTime().After(old.Add(time.Second)) {
		t.Fatalf("an unmarked clear rewrote quota.json (mtime %v)", fi.ModTime())
	}
}

func TestClearProviderAuthClearsRouterAndDispatchMarks(t *testing.T) {
	useConfigDir(t)
	for _, a := range []string{ProviderAccount("dbx"), "anthropic·dbx", "openai·dbx", "anthropic·other", ProviderAccount("dbx2")} {
		if err := MarkAuth(a, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if err := ClearProviderAuth("dbx"); err != nil {
		t.Fatal(err)
	}
	s, now := LoadState(), time.Now()
	for _, a := range []string{ProviderAccount("dbx"), "anthropic·dbx", "openai·dbx"} {
		if _, ok := s.AuthMarked(a, now); ok {
			t.Errorf("%s still marked", a)
		}
	}
	for _, a := range []string{"anthropic·other", ProviderAccount("dbx2")} {
		if _, ok := s.AuthMarked(a, now); !ok {
			t.Errorf("%s was cleared but belongs to another provider", a)
		}
	}
}

func TestMarkAuthKeepsQuotaFields(t *testing.T) {
	useConfigDir(t)
	now := time.Now()
	if err := Record("openai·chatgpt", 40, "5h", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	_ = MarkAuth("openai·chatgpt", "r")
	_ = ClearAuth("openai·chatgpt")
	if e := LoadState()["openai·chatgpt"]; e.UsedPct != 40 {
		t.Fatalf("mark/clear dropped quota fields: %+v", e)
	}
}

func TestPickSkipsAuthMarkedAccountWithReason(t *testing.T) {
	tb := testTable(t)
	marked := State{"openai·chatgpt": {AuthReason: "auth needs_login: run `claude-code databricks login --profile chatgpt`", AuthAt: now0.Add(-time.Hour)}}
	r := pick(tb, "sonnet", "codex", marked, FallbackAgents)
	if strings.Contains(runners(r), "codex") {
		t.Fatalf("auth-marked codex picked: %s", runners(r))
	}
	var skip *Skip
	for i := range r.Skipped {
		if r.Skipped[i].Gate == GateAuth {
			skip = &r.Skipped[i]
		}
	}
	if skip == nil || skip.Agent != "codex" || !strings.Contains(skip.Reason, "databricks login") {
		t.Fatalf("skips = %+v, want a codex auth skip carrying the login hint", r.Skipped)
	}
	if len(r.Candidates) == 0 {
		t.Fatal("other runners must remain candidates")
	}

	// An expired mark no longer gates; an unmarked state never does.
	for name, st := range map[string]State{
		"expired": {"openai·chatgpt": {AuthReason: "r", AuthAt: now0.Add(-AuthMarkTTL - time.Minute)}},
		"cleared": {"openai·chatgpt": {UsedPct: 5, Seen: now0}},
	} {
		if !strings.Contains(runners(pick(tb, "sonnet", "codex", st, FallbackAgents)), "codex") {
			t.Errorf("%s: codex wrongly gated", name)
		}
	}
}

func TestPickAuthGateComesBeforeExhausted(t *testing.T) {
	tb := testTable(t)
	st := State{"openai·chatgpt": {AuthReason: "auth rejected", AuthAt: now0, ExhaustedUntil: now0.Add(time.Hour), Reason: "402"}}
	r := pick(tb, "sonnet", "codex", st, FallbackAgents)
	for _, s := range r.Skipped {
		if s.Agent == "codex" && s.Gate != GateAuth {
			t.Fatalf("codex skipped at %q, want the auth gate first: %+v", s.Gate, s)
		}
	}
}

func TestPickAuthGateOnOpencodeSubCandidate(t *testing.T) {
	tb := testTable(t)
	st := State{"opencode·go": {AuthReason: "auth rejected", AuthAt: now0}}
	r := pickRepo(tb, "sonnet", st, true)
	found := false
	for _, s := range r.Skipped {
		if s.Gate == GateAuth {
			found = true
		}
	}
	if !found {
		t.Fatalf("an auth-marked opencode account must show an auth skip: %+v", r.Skipped)
	}
}

func TestAccountAuthMarkedFallsBackToTheProviderMark(t *testing.T) {
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	s := State{
		"databricks·dev": {AuthReason: "router mark", AuthAt: at},
		"anthropic·qa":   {AuthReason: "own mark", AuthAt: at},
		"anthropic·old":  {AuthReason: "own", AuthAt: at.Add(-AuthMarkTTL - time.Hour)},
	}
	for _, c := range []struct {
		account string
		want    string
		ok      bool
	}{
		{"anthropic·dev", "router mark", true},
		{"openai·dev", "router mark", true},
		{"anthropic·qa", "own mark", true},
		{"anthropic·old", "", false},
		{"anthropic·prod", "", false},
		{"databricks·dev", "router mark", true},
		{"nodot", "", false},
		{"anthropic·", "", false},
	} {
		got, ok := s.AccountAuthMarked(c.account, at.Add(time.Minute))
		if got != c.want || ok != c.ok {
			t.Errorf("AccountAuthMarked(%q) = %q, %v; want %q, %v", c.account, got, ok, c.want, c.ok)
		}
	}
	if _, ok := s.AccountAuthMarked("anthropic·dev", at.Add(AuthMarkTTL)); ok {
		t.Error("an expired provider mark still gates the dispatch account")
	}
}

// A router mark on databricks·<p> gates a pick whose account is "<provider>·<p>".
func TestPickGatesDispatchAccountOnRouterProviderMark(t *testing.T) {
	tb := testTable(t)
	st := State{ProviderAccount("chatgpt"): {AuthReason: "auth rejected: run `claude-code databricks login --profile chatgpt`", AuthAt: now0}}
	r := pick(tb, "sonnet", "codex", st, FallbackAgents)
	if strings.Contains(runners(r), "codex") {
		t.Fatalf("openai·chatgpt picked despite databricks·chatgpt mark: %s", runners(r))
	}
	got := false
	for _, s := range r.Skipped {
		got = got || (s.Agent == "codex" && s.Gate == GateAuth)
	}
	if !got {
		t.Fatalf("skips = %+v, want a codex auth skip", r.Skipped)
	}
}

// vault set NAME clears the marks of providers whose sealed secret OR auth.env is NAME, and only those.
func TestClearProviderAuthMatchesSealedSecretAndEnv(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{
		"clr-env":{"protocol":"anthropic","base_url":"https://e.example","auth":{"type":"key","header":"x-api-key","env":"CLR_ENV_KEY"},"models":{"opus":"m"}},
		"clr-other":{"protocol":"anthropic","base_url":"https://o.example","auth":{"type":"key","header":"x-api-key","env":"CLR_OTHER_KEY"},"models":{"opus":"m"}}},
		"databricks":{
		"clr-pat":{"protocol":"anthropic","base_url":"https://p.example","auth":{"type":"pat"},"models":{"opus":"m"}}}}}`)
	for _, a := range []string{ProviderAccount("clr-env"), "github·clr-env", ProviderAccount("clr-pat"), ProviderAccount("clr-other")} {
		if err := MarkAuth(a, "r"); err != nil {
			t.Fatal(err)
		}
	}
	live := func(a string) bool { _, ok := LoadState().AuthMarked(a, time.Now()); return ok }

	ClearSecretAuth("CLR_ENV_KEY")
	if live(ProviderAccount("clr-env")) || live("github·clr-env") {
		t.Error("vault set of the auth.env name did not clear that provider's marks")
	}
	if !live(ProviderAccount("clr-pat")) || !live(ProviderAccount("clr-other")) {
		t.Error("clearing one env name touched other providers")
	}

	ClearSecretAuth(providers.SecretName(providers.GroupDatabricks, "clr-pat", providers.AuthPAT))
	if live(ProviderAccount("clr-pat")) {
		t.Error("vault set of the sealed secret name did not clear its provider")
	}
	if !live(ProviderAccount("clr-other")) {
		t.Error("an unrelated mark was cleared")
	}
	ClearSecretAuth("UNRELATED_NAME")
	if !live(ProviderAccount("clr-other")) {
		t.Error("an unmatched name cleared a mark")
	}
}

const v0Quota = `{"anthropic·a":{"used_pct":42.5,"window":"5h","seen":"2026-10-28T10:00:00Z"},` +
	`"codex·b":{"used_pct":10,"seen":"2026-10-28T11:00:00Z"}}`

func quotaFixture(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(paths.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StatePath(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return StatePath()
}

func wantEntries(t *testing.T, s State) {
	t.Helper()
	if len(s) != 2 || s["anthropic·a"].UsedPct != 42.5 || s["anthropic·a"].Window != "5h" || s["codex·b"].UsedPct != 10 {
		t.Fatalf("entries = %+v", s)
	}
}

// A v0 bare-map quota.json reads, install-time migration leaves it untouched (an old daemon still reads it), the
// {schema,accounts} wrapper reads too, and Update writes the bare map back.
func TestQuotaV0ReadsAndStaysBareMap(t *testing.T) {
	path := quotaFixture(t, v0Quota)
	wantEntries(t, LoadState())

	if _, err := schemas.MigrateAll(false); err != nil {
		t.Fatalf("MigrateAll: %v", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != v0Quota {
		t.Fatalf("quota.json rewritten: %s", raw)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir(), "migrations.jsonl")); err == nil {
		t.Fatal("migration ledger written for quota")
	}

	wrapped := `{"schema":1,"accounts":` + v0Quota + `}`
	if err := os.WriteFile(path, []byte(wrapped), 0o600); err != nil {
		t.Fatal(err)
	}
	wantEntries(t, LoadState())
	if err := Update(func(State) {}); err != nil {
		t.Fatal(err)
	}
	var bare map[string]Entry
	if raw, _ := os.ReadFile(path); json.Unmarshal(raw, &bare) != nil || len(bare) != 2 || bare["codex·b"].UsedPct != 10 {
		t.Fatalf("Update wrote %s, want the bare map", raw)
	}
}

// A newer-schema document reads as empty but Update refuses and leaves the file byte-identical.
func TestQuotaNewerSchemaIsNotOverwritten(t *testing.T) {
	const newer = `{"schema":2,"accounts":{"a":{"used_pct":1,"seen":"2026-10-28T10:00:00Z"}}}`
	path := quotaFixture(t, newer)
	if len(LoadState()) != 0 {
		t.Fatal("newer document should read empty")
	}
	if err := Record("x", 5, "5h", time.Time{}, time.Now()); !errors.Is(err, schemas.ErrNewer) {
		t.Fatalf("Update err = %v, want ErrNewer", err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != newer {
		t.Fatalf("newer file overwritten: %s", raw)
	}
}

func TestParseResetHugeNClampsToFuture(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, text := range []string{
		"try again in 99999999999999999999 days",
		"try again in 9223372036854775807 days",
		"try again in 9999999999 hours",
	} {
		got, ok := ParseReset(text, now)
		if !ok || !got.After(now) {
			t.Errorf("ParseReset(%q) = %v, %v; want a reset after now", text, got, ok)
		}
	}
}
