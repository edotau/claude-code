package menu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a routed agent, when check/dry-run reports it, then neither router boundary is called.
func TestRunAgentsReadOnlyNeverConfiguresRouter(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "claude", "profile", "openai")
	t.Setenv("OPENAI_API_KEY", "synthetic-openai-key")
	origEnsure, origSecret := agent.EnsureRouter, agent.RouterSecret
	t.Cleanup(func() { agent.EnsureRouter, agent.RouterSecret = origEnsure, origSecret })
	calls := 0
	agent.EnsureRouter = func(context.Context) (string, error) {
		calls++
		return "", errors.New("read-only attempted router startup")
	}
	agent.RouterSecret = func() (string, error) {
		calls++
		return "", errors.New("read-only attempted secret access")
	}
	for _, o := range []Options{{Check: true, Agent: "codex"}, {DryRun: true, Agent: "codex"}} {
		d, out := newDialog("")
		d.reg, d.o = reg, o
		if !runAgents(d) || !strings.Contains(out.String(), "routes to openai") {
			t.Fatalf("read-only route failed: %s", out)
		}
	}
	if calls != 0 {
		t.Fatalf("read-only invoked router boundaries %d times", calls)
	}
}

// Given stored A and newly staged B, when Codex signs in, then its stdin receives B, not A.
func TestCodexUseKeyPrefersStagedPaste(t *testing.T) {
	menuTestEnv(t)
	t.Setenv("OPENAI_API_KEY", "")
	vaulttest.Secret(t, "OPENAI_API_KEY", "synthetic-old-key")
	d, _ := newDialog("")
	d.pending = map[string]stagedKey{}
	if !d.stage("OPENAI_API_KEY", "synthetic-new-key") {
		t.Fatal("stage failed")
	}
	t.Cleanup(func() { discardKeys(d) })
	orig := codexKeyLogin
	t.Cleanup(func() { codexKeyLogin = orig })
	got := ""
	codexKeyLogin = func(key string) error { got = key; return nil }
	if !codexUseKey(d) || got != "synthetic-new-key" {
		t.Fatal("Codex login did not receive the newly staged key")
	}
}

// Given a failed vault delete, when the page submits and the menu exits, then retry state and failure survive.
func TestFailedDeleteRetainedAndReportedByMenu(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.Secret(t, "MENU_DELETE_RETRY", "synthetic-delete-value")
	good := os.Getenv("CLAUDE_CONFIG_DIR")
	brokenVault(t)
	d, out := newDialog("")
	d.reg = reg
	origPick, origConfirm := pickMenu, saveConfirmed
	t.Cleanup(func() { pickMenu, saveConfirmed = origPick, origConfirm })
	saveConfirmed = func(string) bool { return true }
	pickMenu = flatPick(func(string, [][2]string, int) (int, error) {
		d.stageDelete("MENU_DELETE_RETRY")
		confirmPageKeys(d, nil)
		return -1, nil
	})
	if rc := runMenu(d); rc != 1 {
		t.Fatalf("runMenu = %d, want failed deletion exit 1: %s", rc, out)
	}
	if !d.pendingDeletes["MENU_DELETE_RETRY"] {
		t.Fatal("failed deletion lost its retry state")
	}
	assertContainsAll(t, out.String(), []string{"MENU_DELETE_RETRY: not deleted:", "deletion(s) not removed from the vault"})
	t.Setenv("CLAUDE_CONFIG_DIR", good)
	confirmPageKeys(d, nil)
	if len(d.pendingDeletes) != 0 {
		t.Fatal("successful retry left pending deletions")
	}
	assertNotInVault(t, "MENU_DELETE_RETRY")
}

// brokenVault installs an unreadable vault path only in this test's isolated config directory.
func brokenVault(t *testing.T) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "state", "vault.db"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// Given persisted profiles and an unwritable cache target, when saving the seed, then regeneration is required.
func TestSeedCacheFailureStillMarksChanged(t *testing.T) {
	d, out := seedDialog(t, "")
	if err := os.MkdirAll(filepath.Join(paths.StateDir(), "last-profile.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if saveSeed(d, map[string]string{"codex": "chatgpt"}) {
		t.Fatal("cache cleanup failure reported success")
	}
	if vault.GetAgentRecord("codex").Profile != "chatgpt" || !d.changed {
		t.Fatalf("persisted profile must trigger regeneration despite cache failure: changed=%v, out=%s", d.changed, out)
	}
	if !strings.Contains(out.String(), "seed:") {
		t.Fatal("missing cache failure diagnostic")
	}
}

// Given an SP client id without a stored/pasted secret, when identity is submitted, then no identity is saved.
func TestDbxIdentityRejectsClientWithoutSecret(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	answers := []string{"synthetic-client", ""}
	dbxSecret = func(string, string, func(string) string) (string, error) {
		s := answers[0]
		answers = answers[1:]
		return s, nil
	}
	dbxIdentityPrompt(d, "dev", providers.IdentitySP, d.reg.Providers["dev"], "")
	if d.changed || len(f.calls) != 0 {
		t.Fatalf("incomplete SP pair performed work: changed=%v calls=%v", d.changed, f.calls)
	}
	if !strings.Contains(dbxOut(d), "has no secret") {
		t.Fatalf("missing incomplete-pair diagnostic: %s", dbxOut(d))
	}
	d.reload()
	if d.reg.Providers["dev"].Auth.SPClientID != "" {
		t.Fatal("incomplete SP client id was persisted")
	}
}

// renderFailureDialog uses a default-shaped but hermetic home so the real renderer cannot skip config loading.
func renderFailureDialog(t *testing.T) *dialog {
	t.Helper()
	menuTestEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if err := os.MkdirAll(filepath.Join(home, ".claude", "state", "vault.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	d, _ := newDialog("")
	d.changed = true
	return d
}

// Given a changed menu and render failure, when leaving normally, then the deferred render makes exit nonzero.
func TestRunMenuRenderFailureExitsOne(t *testing.T) {
	reg := menuTestEnv(t)
	d := renderFailureDialog(t)
	d.reg = reg
	orig := pickMenu
	t.Cleanup(func() { pickMenu = orig })
	pickMenu = flatPick(func(string, [][2]string, int) (int, error) { return -1, nil })
	if rc := runMenu(d); rc != 1 {
		t.Fatalf("runMenu = %d, want 1 after config render failure", rc)
	}
}

// Given a failed renderer, when sections finish, then both the render error and nonzero status propagate.
func TestFinishSectionsPropagatesRenderFailure(t *testing.T) {
	d := renderFailureDialog(t)
	if err := rerenderConfigs(d); err == nil || !strings.Contains(err.Error(), "agent configs:") {
		t.Fatalf("rerenderConfigs error = %v, want contextual render failure", err)
	}
	if rc := finishSections(d, true); rc != 1 {
		t.Fatalf("finishSections = %d, want 1 after config render failure", rc)
	}
}

// Given a working old key and rejected replacement, when input ends at Keep old?, then the old key remains.
func TestKeepOldKeyOnEOF(t *testing.T) {
	menuTestEnv(t)
	const name = "MENU_KEEP_OLD_EOF"
	t.Setenv(name, "")
	vaulttest.Secret(t, name, "working-credential")
	p := fakeKeyedProvider(t, name, "working-credential")
	d, out := newDialog("")
	kt := configure.KeyTarget{EnvVar: name, Provider: p}
	if !keepOldKey(d, kt, "rejected-replacement") {
		t.Fatalf("EOF discarded the working credential: %s", out)
	}
	if got, _, ok := vault.Get(name); !ok || got != "working-credential" {
		t.Fatal("working credential changed")
	}
	assertContainsAll(t, out.String(), []string{"rejected", "input ended: kept the old key"})
}

// transientModelStore fails storage after a model pick and recovers when its error is reported.
// The real picker, vault reads/writes and pin cleanup all run; only terminal I/O schedules the outage.
type transientModelStore struct {
	t      *testing.T
	good   string
	bad    string
	input  *strings.Reader
	out    bytes.Buffer
	failed bool
}

func (s *transientModelStore) Read(p []byte) (int, error) {
	s.t.Setenv("CLAUDE_CONFIG_DIR", s.bad)
	return s.input.Read(p)
}

func (s *transientModelStore) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "model: ") {
		s.failed = true
		s.t.Setenv("CLAUDE_CONFIG_DIR", s.good)
	}
	return s.out.Write(p)
}

// Given a transient model-save failure, when storage recovers, then effort/context pins still belong to the old model.
func TestCopilotFailedModelSaveKeepsPins(t *testing.T) {
	reg := menuTestEnv(t)
	vaulttest.SetRecord(t, "copilot", "model", "large-model")
	vaulttest.SetRecord(t, "copilot", "effort", "high")
	vaulttest.SetRecord(t, "copilot", "context", "1m")
	good := os.Getenv("CLAUDE_CONFIG_DIR")
	brokenVault(t)
	s := &transientModelStore{t: t, good: good, bad: os.Getenv("CLAUDE_CONFIG_DIR"), input: strings.NewReader("3\n")}
	t.Setenv("CLAUDE_CONFIG_DIR", good)
	d := &dialog{reg: reg, br: bufio.NewReader(s), out: s, copilotCatalogLoaded: true,
		copilotCatalog: []copilot.SDKModel{
			{ID: "plain-model", Enabled: true, ContextTokens: 100_000},
			{ID: "large-model", Enabled: true, ReasoningEffort: true, Efforts: []string{"high"}, ContextTokens: 2_000_000},
		}}
	pickCopilotModel(d)
	if !s.failed {
		t.Fatalf("did not exercise model-save failure: %s", &s.out)
	}
	rec := vault.GetAgentRecord("copilot")
	if rec.Model != "large-model" || rec.Effort != "high" || rec.Context != "1m" {
		t.Fatalf("failed model save changed old model/pins: %+v", rec)
	}
}

// Given saved routing and ended input, when its text row opens, then no reset is staged or confirmed.
func TestRoutingEOFDoesNotResetSavedOverride(t *testing.T) {
	menuTestEnv(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"routing":{"weights":{"price":0.73,"headroom":0.27}}}`)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := providers.RoutingOverlay()
	if err != nil {
		t.Fatal(err)
	}
	d, _ := newDialog("")
	origPick, origConfirm := pickMenu, saveConfirmed
	t.Cleanup(func() { pickMenu, saveConfirmed = origPick, origConfirm })
	picks, confirms := 0, 0
	pickMenu = func(string, []tui.Section, [2]string, int) (int, error) {
		picks++
		if picks == 1 {
			return 0, nil // weights.price is a number/text prompt
		}
		return -1, nil
	}
	saveConfirmed = func(string) bool { confirms++; return true }
	runRoutingMenu(d, reg.Routing, overlay)
	after, err := providers.RoutingOverlay()
	if err != nil || after.Weights["price"] != 0.73 || confirms != 0 || d.changed {
		t.Fatalf("EOF reset/confirmed routing: weights=%v confirms=%d changed=%v err=%v", after.Weights, confirms, d.changed, err)
	}
}

// Given one registry profile lacking a credential, when provider runs in check mode, then sections exit 1.
func TestProviderCheckFailsForUnsetCredential(t *testing.T) {
	menuTestEnv(t)
	const env = "MENU_MISSING_PROVIDER_KEY"
	t.Setenv(env, "")
	p := &providers.Provider{Name: "missing", Auth: providers.Auth{Type: providers.AuthKey, Env: env}}
	d, out := newDialog("")
	d.o.Check = true
	d.reg = &providers.Registry{Providers: map[string]*providers.Provider{"missing": p}}
	if rc := finishSections(d, runProvider(d)); rc != 1 {
		t.Fatalf("provider --check = %d, want 1 for unset credential: %s", rc, out)
	}
}

// Given unreadable vault metadata, when rendering web search, then status is unknown, not an absent key.
func TestWebSearchVaultFailureIsUnknown(t *testing.T) {
	menuTestEnv(t)
	t.Setenv(webSearchKey, "")
	brokenVault(t)
	if got := webSearchRow().State; got != "status unknown" {
		t.Fatalf("web search state = %q, want status unknown on vault failure", got)
	}
}

// Given the Databricks actions, when rendered, then PAT profiles are not promised freshly minted tokens.
func TestDbxTokenActionsDescribeStoredPAT(t *testing.T) {
	d := dbxEnv(t)
	f := &dbxFake{}
	installDbxFake(t, f)
	runDatabricksEnv(d, "dev")
	if len(f.rows) == 0 {
		t.Fatal("no Databricks action menu rendered")
	}
	for _, label := range []string{"Token", "Copy token"} {
		found := false
		for _, row := range f.rows[0] {
			if row[0] == label {
				found = strings.Contains(row[1], "stored PAT")
			}
		}
		if !found {
			t.Fatalf("%s must describe the stored PAT exception: %v", label, f.rows[0])
		}
	}
}

func shellOn(script string) (*shell, *bytes.Buffer) {
	var out bytes.Buffer
	return &shell{Out: &out, In: bufio.NewReader(strings.NewReader(script))}, &out
}

// Given a page with a counting row and Done, When 1 is entered twice then 2, Then the row ran twice and Run reports done.
func TestRunKeepsPageUntilRowDone(t *testing.T) {
	s, out := shellOn("1\n1\n2\n")
	n := 0
	done := s.Run("T", func() []shellRow {
		return []shellRow{{Name: "a", Detail: "x", Run: func() bool { n++; return false }}, {Name: "Done", Run: func() bool { return true }}}
	})
	if !done || n != 2 || !strings.Contains(out.String(), "1) a") {
		t.Fatalf("done=%v n=%d out=%q", done, n, out.String())
	}
}

// Given a blank or out-of-range answer, Then Run returns false and no row runs.
func TestRunDismissed(t *testing.T) {
	for _, in := range []string{"\n", "9\n", ""} {
		s, _ := shellOn(in)
		if s.Run("T", func() []shellRow { return []shellRow{{Name: "a", Run: func() bool { t.Fatal("ran"); return true }}} }) {
			t.Fatalf("%q reported done", in)
		}
	}
}

func TestPaintAndMark(t *testing.T) {
	s, out := shellOn("\n")
	s.Paint = strings.ToUpper
	s.Pick("hi", [][2]string{{"a", "b"}})
	if !strings.Contains(out.String(), "HI\n") || mark(true) != " (current)" || mark(false) != "" {
		t.Fatalf("%q", out.String())
	}
}

// Given two lines typed ahead on the terminal, When Line runs twice, Then the second call sees the first call's look-ahead.
func TestLineTTYKeepsLookAhead(t *testing.T) {
	orig, r := lineReader, bufio.NewReader(strings.NewReader("one\ntwo\n"))
	lineReader = func() *bufio.Reader { return r }
	t.Cleanup(func() { lineReader = orig })
	var out bytes.Buffer
	s := &shell{Out: &out, TTY: true}
	if a, b := s.Line("> "), s.Line("> "); a != "one" || b != "two" {
		t.Fatalf("got %q then %q", a, b)
	}
}
