package agent

import (
	"github.com/edotau/claude-code/internal/router"
	"os"
	"strings"
	"testing"
	"time"
)

func useConfigDir(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

func TestMarkAuthRoundTripAndClear(t *testing.T) {
	useConfigDir(t)
	acct := router.ProviderAccount("dbx-oai")
	if acct != "databricks·dbx-oai" {
		t.Fatalf("ProviderAccount = %q", acct)
	}
	if err := router.MarkAuth(acct, "auth needs_login"); err != nil {
		t.Fatal(err)
	}
	reason, ok := router.LoadState().AuthMarked(acct, time.Now())
	if !ok || reason != "auth needs_login" {
		t.Fatalf("AuthMarked = %q, %v", reason, ok)
	}
	if err := router.ClearAuth(acct); err != nil {
		t.Fatal(err)
	}
	if _, ok := router.LoadState().AuthMarked(acct, time.Now()); ok {
		t.Fatal("mark survived ClearAuth")
	}
}

func TestAuthMarkExpiresAtTTL(t *testing.T) {
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	s := router.State{"openai·chatgpt": {AuthReason: "r", AuthAt: at}}
	for _, c := range []struct {
		age  time.Duration
		want bool
	}{{0, true}, {router.AuthMarkTTL - time.Second, true}, {router.AuthMarkTTL, false}, {router.AuthMarkTTL + time.Second, false}} {
		if _, ok := s.AuthMarked("openai·chatgpt", at.Add(c.age)); ok != c.want {
			t.Errorf("age %v: marked = %v, want %v", c.age, ok, c.want)
		}
	}
	if router.AuthMarkTTL != 6*time.Hour {
		t.Errorf("AuthMarkTTL = %v, want 6h", router.AuthMarkTTL)
	}
	if _, ok := s.AuthMarked("nobody", at); ok {
		t.Error("unknown account marked")
	}
	if _, ok := (router.State{"x": {}}).AuthMarked("x", at); ok {
		t.Error("zero AuthAt must not read as a mark")
	}
}

func TestClearAuthNoWriteWhenUnmarked(t *testing.T) {
	useConfigDir(t)
	if err := router.ClearAuth("openai·chatgpt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(router.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("ClearAuth on an empty state created quota.json (stat err %v)", err)
	}
	if err := router.ClearProviderAuth("dbx"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(router.StatePath()); !os.IsNotExist(err) {
		t.Fatal("ClearProviderAuth on an empty state created quota.json")
	}
	// An existing file with an entry but no mark stays byte-identical, mtime untouched.
	if err := router.Record("openai·chatgpt", 10, "5h", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(router.StatePath())
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(router.StatePath(), old, old); err != nil {
		t.Fatal(err)
	}
	_ = router.ClearAuth("openai·chatgpt")
	_ = router.ClearAuth("never-seen")
	_ = router.ClearProviderAuth("chatgpt")
	after, _ := os.ReadFile(router.StatePath())
	fi, _ := os.Stat(router.StatePath())
	if string(before) != string(after) || fi.ModTime().After(old.Add(time.Second)) {
		t.Fatalf("an unmarked clear rewrote quota.json (mtime %v)", fi.ModTime())
	}
}

func TestClearProviderAuthClearsRouterAndLegMarks(t *testing.T) {
	useConfigDir(t)
	for _, a := range []string{router.ProviderAccount("dbx"), "anthropic·dbx", "openai·dbx", "anthropic·other", router.ProviderAccount("dbx2")} {
		if err := router.MarkAuth(a, "r"); err != nil {
			t.Fatal(err)
		}
	}
	if err := router.ClearProviderAuth("dbx"); err != nil {
		t.Fatal(err)
	}
	s, now := router.LoadState(), time.Now()
	for _, a := range []string{router.ProviderAccount("dbx"), "anthropic·dbx", "openai·dbx"} {
		if _, ok := s.AuthMarked(a, now); ok {
			t.Errorf("%s still marked", a)
		}
	}
	for _, a := range []string{"anthropic·other", router.ProviderAccount("dbx2")} {
		if _, ok := s.AuthMarked(a, now); !ok {
			t.Errorf("%s was cleared but belongs to another provider", a)
		}
	}
}

func TestMarkAuthKeepsQuotaFields(t *testing.T) {
	useConfigDir(t)
	now := time.Now()
	if err := router.Record("openai·chatgpt", 40, "5h", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	_ = router.MarkAuth("openai·chatgpt", "r")
	_ = router.ClearAuth("openai·chatgpt")
	if e := router.LoadState()["openai·chatgpt"]; e.UsedPct != 40 {
		t.Fatalf("mark/clear dropped quota fields: %+v", e)
	}
}

func TestPickSkipsAuthMarkedAccountWithReason(t *testing.T) {
	tb := testTable(t)
	marked := router.State{"openai·chatgpt": {AuthReason: "auth needs_login: run `claude-code databricks login --profile chatgpt`", AuthAt: now0.Add(-time.Hour)}}
	r := pick(tb, "sonnet", "codex", marked, FallbackAgents)
	if strings.Contains(runners(r), "codex") {
		t.Fatalf("auth-marked codex picked: %s", runners(r))
	}
	var skip *Skip
	for i := range r.Skipped {
		if r.Skipped[i].Gate == router.GateAuth {
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
	for name, st := range map[string]router.State{
		"expired": {"openai·chatgpt": {AuthReason: "r", AuthAt: now0.Add(-router.AuthMarkTTL - time.Minute)}},
		"cleared": {"openai·chatgpt": {UsedPct: 5, Seen: now0}},
	} {
		if !strings.Contains(runners(pick(tb, "sonnet", "codex", st, FallbackAgents)), "codex") {
			t.Errorf("%s: codex wrongly gated", name)
		}
	}
}

func TestPickAuthGateComesBeforeExhausted(t *testing.T) {
	tb := testTable(t)
	st := router.State{"openai·chatgpt": {AuthReason: "auth rejected", AuthAt: now0, ExhaustedUntil: now0.Add(time.Hour), Reason: "402"}}
	r := pick(tb, "sonnet", "codex", st, FallbackAgents)
	for _, s := range r.Skipped {
		if s.Agent == "codex" && s.Gate != router.GateAuth {
			t.Fatalf("codex skipped at %q, want the auth gate first: %+v", s.Gate, s)
		}
	}
}

func TestPickAuthGateOnOpencodeSubCandidate(t *testing.T) {
	tb := testTable(t)
	st := router.State{"opencode·go": {AuthReason: "auth rejected", AuthAt: now0}}
	r := pickRepo(tb, "sonnet", st, true)
	found := false
	for _, s := range r.Skipped {
		if s.Gate == router.GateAuth {
			found = true
		}
	}
	if !found {
		t.Fatalf("an auth-marked opencode account must show an auth skip: %+v", r.Skipped)
	}
}

func TestAccountAuthMarkedFallsBackToTheProviderMark(t *testing.T) {
	at := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	s := router.State{
		"databricks·dev": {AuthReason: "router mark", AuthAt: at},
		"anthropic·qa":   {AuthReason: "own mark", AuthAt: at},
		"anthropic·old":  {AuthReason: "own", AuthAt: at.Add(-router.AuthMarkTTL - time.Hour)},
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
	if _, ok := s.AccountAuthMarked("anthropic·dev", at.Add(router.AuthMarkTTL)); ok {
		t.Error("an expired provider mark still gates the leg account")
	}
}

// A router mark on databricks·<p> gates a pick whose account is "<provider>·<p>".
func TestPickGatesLegAccountOnRouterProviderMark(t *testing.T) {
	tb := testTable(t)
	st := router.State{router.ProviderAccount("chatgpt"): {AuthReason: "auth rejected: run `claude-code databricks login --profile chatgpt`", AuthAt: now0}}
	r := pick(tb, "sonnet", "codex", st, FallbackAgents)
	if strings.Contains(runners(r), "codex") {
		t.Fatalf("openai·chatgpt picked despite databricks·chatgpt mark: %s", runners(r))
	}
	got := false
	for _, s := range r.Skipped {
		got = got || (s.Agent == "codex" && s.Gate == router.GateAuth)
	}
	if !got {
		t.Fatalf("skips = %+v, want a codex auth skip", r.Skipped)
	}
}
