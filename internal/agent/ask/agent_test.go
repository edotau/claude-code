package ask

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// attachFlag.Set resolves a real file to an absolute path and errors on anything else: missing,
// directory, or a path flag.Abs itself cannot resolve.
func TestAttachFlagSet(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFixtureFile(t, f, "hi")

	var a attachFlag
	if err := a.Set(f); err != nil {
		t.Fatalf("Set(%q): %v", f, err)
	}
	if len(a) != 1 || a[0] != f {
		t.Errorf("a = %q, want [%q]", a, f)
	}

	if err := a.Set(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("Set on a missing path: want an error, got nil")
	}
	if err := a.Set(dir); err == nil {
		t.Error("Set on a directory: want an error, got nil")
	}
}

// keepEnvFlag.Set comma-splits each occurrence and accumulates across repeats.
func TestKeepEnvFlagSet(t *testing.T) {
	var k keepEnvFlag
	if err := k.Set("DATABRICKS_HOST,DATABRICKS_TOKEN"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := k.Set("HOME"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !slices.Equal([]string(k), []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Errorf("k = %q, want accumulated comma-split names", k)
	}
}

// attachFlag.Set rejects binary files up front (NUL, invalid UTF-8) and accepts multibyte UTF-8 text.
func TestAttachFlagSetRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	var a attachFlag
	for name, data := range map[string]string{"nul.bin": "ab\x00cd", "bad.dat": "\xff\xfe\x80"} {
		f := filepath.Join(dir, name)
		writeFixtureFile(t, f, data)
		err := a.Set(f)
		if err == nil || !strings.Contains(err.Error(), "binary file") || !strings.Contains(err.Error(), name) {
			t.Errorf("Set(%s): err = %v, want a binary-file error naming the path", name, err)
		}
	}
	if len(a) != 0 {
		t.Errorf("a = %q, want nothing recorded for rejected files", a)
	}
	f := filepath.Join(dir, "utf8.txt")
	writeFixtureFile(t, f, "héllo 世界")
	if err := a.Set(f); err != nil || len(a) != 1 {
		t.Errorf("Set(multibyte text): err = %v, a = %q, want accepted", err, a)
	}
}

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID) plus, when active is set, a "work" set.
func profileFixture(t *testing.T, active string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if active != "" {
		doc = `{` + acmeProvider + `,"profiles":{"active":"` + active + `","sets":{"work":{
		  "codex":{"account":"acme","model":"acme-large","effort":"high"}}}}}`
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	client := NewClient()
	for _, profile := range []string{"acme", "chatgpt"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatalf("Prepare(%s) = %v", profile, err)
		}
		res, err := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: want one dispatch, got %+v, err %v", profile, res.Hops, err)
		}
		if profile == "acme" && (err != nil || res.Answer != "ok") {
			t.Fatalf("native profile did not run: %+v, %v", res, err)
		}
		if profile == "chatgpt" && (err == nil || !strings.Contains(err.Error(), "opencode has no openai login")) {
			t.Fatalf("hub profile did not check its login: %+v, %v", res, err)
		}
	}
}

// A cancelled batch prepares nothing and never refreshes quota.
func TestClientPrepareHonorsCancellation(t *testing.T) {
	refreshes := 0
	prev := RouteRefresh
	RouteRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { RouteRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: router.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's alias, as the pinned route does.
func TestClientPrepareProfileTakesTierSlot(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := NewClient()
	for _, profile := range []string{"chatgpt", "acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: hops = %+v, want one unavailable dispatch", profile, res.Hops)
		}
		if want := (router.RuleClassifier{}).Alias("pong"); res.Hops[0].Model != want {
			t.Errorf("%s: dispatch model = %q, want alias %q", profile, res.Hops[0].Model, want)
		}
	}
}

// An explicit --profile is one dispatch for every agent even with --fallback all: nothing can silently take over.
func TestClientPrepareProfileIsOneDispatchUnderFallbackAll(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := NewClient()
	for _, name := range []string{"codex", "claude", "opencode", "copilot", "gemini"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: name, Provider: "acme", Prompt: "p", Fallback: router.FallbackAll}, nil)
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed dispatch without fallback", name, len(res.Hops), err)
		}
	}
}

// writeFixtureFile creates path (with parents) holding content.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A background start whose job-id line cannot be written reports the write error instead of exiting 0 silently.
func TestReportBackgroundStartReturnsWriteError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // a reader-less pipe fails every write with EPIPE
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig; w.Close() })
	for _, format := range []string{helptext.Text, helptext.JSON} {
		if err := ReportBackgroundStart(format, job.Record{ID: "j1"}); err == nil {
			t.Errorf("%s: write to a closed pipe returned nil", format)
		}
	}
}

func TestRoster(t *testing.T) {
	if got := agent.Names(); !slices.Equal(got, []string{"codex", "copilot", "gemini", "opencode"}) {
		t.Errorf("external roster = %v, want [codex copilot gemini opencode]", got)
	}
}

// TestLookupKnownVendors: every external vendor is a registered, external schema with an engine of its own;
// resume, catalog and read-permission facts are pinned by TestRosterFacts.
func TestLookupKnownVendors(t *testing.T) {
	var engines []string
	for _, e := range agent.Engines() {
		engines = append(engines, e.Name())
	}
	for _, name := range []string{"codex", "gemini", "copilot", "opencode"} {
		s, ok := agent.LookupSchema(name)
		if !ok {
			t.Errorf("LookupSchema(%q) not found", name)
			continue
		}
		if s.Name != name || !s.External() {
			t.Errorf("LookupSchema(%q) = %q, external %v", name, s.Name, s.External())
		}
		if !slices.Contains(engines, name) {
			t.Errorf("schema %q has no engine", name)
		}
	}
}

// TestRosterPromptTransport: every uncapped row names its transport (the zero value is the capped argv),
// and gemini's verified ceiling sits below its stdin transport.
func TestRosterPromptTransport(t *testing.T) {
	want := map[string]agent.PromptTransport{"claude": agent.PromptStdin, "codex": agent.PromptStdin,
		"copilot": agent.PromptMessage, "opencode": agent.PromptArgv, "gemini": agent.PromptStdin}
	for name, via := range want {
		s, ok := agent.LookupSchema(name)
		if !ok || s.CLI.PromptVia != via {
			t.Errorf("%s PromptVia = %v (found %v), want %v", name, s.CLI.PromptVia, ok, via)
		}
	}
	if agent.PromptArgv.MaxBytes() != agent.MaxPromptBytes || agent.PromptStdin.MaxBytes() != 0 {
		t.Errorf("MaxBytes: argv %d stdin %d", agent.PromptArgv.MaxBytes(), agent.PromptStdin.MaxBytes())
	}
	if g, _ := agent.LookupSchema("gemini"); g.CLI.PromptMaxBytes != 100*1024 {
		t.Errorf("gemini PromptMaxBytes = %d, want %d", g.CLI.PromptMaxBytes, 100*1024)
	}
}

func TestRosterMatchesSpecs(t *testing.T) {
	var names, runners []string
	for _, s := range agent.Roster() {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"claude", "codex", "copilot", "gemini", "opencode"}) {
		t.Fatalf("roster order = %v", names)
	}
	for _, r := range agent.Engines() {
		runners = append(runners, r.Name())
	}
	for label, other := range map[string][]string{
		"Engines": runners, "RouteAgents": slices.Clone(providers.RouteAgents),
	} {
		slices.Sort(other)
		want := slices.Clone(names)
		slices.Sort(want)
		if !slices.Equal(other, want) {
			t.Errorf("%s names = %v, roster names = %v", label, other, want)
		}
	}
}

func TestRosterFacts(t *testing.T) {
	catalogs := map[string]agent.CLI{
		"codex":    {ResumeFormat: "codex resume session", ModelsProvider: "openai"},
		"gemini":   {ResumeFormat: "agy --conversation session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"opencode": {ResumeFormat: "opencode run --session session", ModelsArgv: []string{"models"}, ReadNeedsTools: true},
		"copilot":  {ResumeFormat: "copilot --resume session", ModelsProvider: "github", ReadNeedsTools: true},
	}
	for _, s := range agent.Roster() {
		t.Run(s.Name, func(t *testing.T) {
			got, ok := agent.LookupSchema(s.Name)
			if !ok || !reflect.DeepEqual(got, s) {
				t.Fatalf("LookupSchema(%q) = %+v, %v; want %+v", s.Name, got, ok, s)
			}
			if s.External() != (s.Name != "claude") {
				t.Errorf("External() = %v", s.External())
			}
			assertRosterWireFacts(t, s, catalogs[s.Name])
		})
	}
	if s, ok := agent.LookupSchema("unknown"); ok || !reflect.DeepEqual(s, agent.Schema{}) {
		t.Errorf("unknown schema = %+v, %v", s, ok)
	}
	if (agent.Schema{}).External() {
		t.Error("zero schema is external")
	}
}

func assertRosterWireFacts(t *testing.T, s agent.Schema, want agent.CLI) {
	t.Helper()
	if s.Name == "claude" {
		if s.CLI.Binary != "" || s.CLI.Dialects != nil || s.CLI.RouterSuffix != nil {
			t.Errorf("claude wire facts = %+v", s.CLI)
		}
		return
	}
	if s.ResumeHint("session") != want.ResumeFormat {
		t.Error("resume command changed")
	}
	if !slices.Equal(s.CLI.ModelsArgv, want.ModelsArgv) || s.CLI.ModelsProvider != want.ModelsProvider ||
		s.CLI.ReadNeedsTools != want.ReadNeedsTools || !slices.Equal(s.CLI.ExtraUsage, want.ExtraUsage) {
		t.Error("catalog, tools or help facts changed")
	}
}

// attach per engine: codex folds the file inline (stdin has no live-read channel). gemini (agy)
// inlines it too when tools are off (agy's no-tools Preamble bars it from reading @path live), but
// appends a live @path reference once tools are on. Every other vendor (opencode included) leaves the
// prompt untouched since it carries the attachment in its own argv instead (opencodeRun's -f).
func TestVendorEngineAttachPrompt(t *testing.T) {
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("file-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	inline := "task\n\n<file path=" + `"` + f + `"` + ">\nfile-content\n</file>"
	cases := []struct {
		name   string
		vendor string
		tools  bool
		want   string
	}{
		{"codex/tools", "codex", true, inline},
		{"codex/no-tools", "codex", false, inline},
		{"gemini/tools", "gemini", true, "task\n@" + f},
		{"gemini/no-tools", "gemini", false, inline},
		{"opencode/tools", "opencode", true, "task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := agent.SchemaFor(c.vendor).Eng().Attach("task", []string{f}, c.tools)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("attach(%s, tools=%v) = %q, want %q", c.vendor, c.tools, got, c.want)
			}
		})
	}
}

// A missing attachment is a fatal error for the inline path, not a silently dropped reference,
// regardless of Tools (gemini without tools now inlines too, so it must fail the same way).
func TestVendorEngineAttachPromptMissingPath(t *testing.T) {
	for _, c := range []struct {
		vendor string
		tools  bool
	}{
		{"codex", true}, {"gemini", false},
	} {
		if _, err := agent.SchemaFor(c.vendor).Eng().Attach("task", []string{"/does/not/exist"}, c.tools); err == nil {
			t.Fatalf("%s tools=%v: want an error for a missing attachment, got nil", c.vendor, c.tools)
		}
	}
}

const agentHdr = "X-Claude-Code-Agent"

// withHeaders returns tg with the provider's own headers set.
func withHeaders(tg providers.Target, h map[string]string) providers.Target {
	p := *tg.Provider
	p.Headers = h
	tg.Provider = &p
	return tg
}

// loadTarget is provider's built-in row with every provider key set, under a fresh config dir.
func loadTarget(t *testing.T, provider, model string) providers.Target {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"OPENAI_API_KEY", "GEMINI_API_KEY", "GATEWAY_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "sk-SECRET-123")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Get(provider)
	if err != nil {
		t.Fatal(err)
	}
	return providers.Target{Provider: p, Model: model}
}

// A direct dispatch never passes routerRoute, so it carries the provider's own headers and no agent tag.
func TestDirectDispatchesHaveNoAgentTag(t *testing.T) {
	vaulttest.Use(t)
	ctx := agent.WithDirect(context.Background())
	own := map[string]string{"X-Tenant": "t1"}

	_, argv, err := agent.SchemaFor("codex").Configure(ctx, withHeaders(loadTarget(t, "openai", "gpt-5"), own), "")
	if err != nil {
		t.Fatal(err)
	}
	if j := strings.Join(argv, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
		t.Errorf("direct codex headers wrong: %q", argv)
	}
	for _, v := range []string{"opencode", "copilot"} {
		env, _, err := agent.SchemaFor(v).Configure(ctx, withHeaders(loadTarget(t, "openai", "gpt-5"), own), "")
		if err != nil {
			t.Fatal(err)
		}
		if j := strings.Join(env, "\x00"); strings.Contains(j, agentHdr) || !strings.Contains(j, "X-Tenant") {
			t.Errorf("direct %s headers wrong: %q", v, env)
		}
	}
}

// Ties agentid.All to the registered roster: five agents, claude included, sorted by name.
func TestRosterNamesAreTheFiveAgents(t *testing.T) {
	if got := agent.RosterNames(); !slices.Equal(got, []string{"claude", "codex", "copilot", "gemini", "opencode"}) {
		t.Errorf("RosterNames() = %v, want [claude codex copilot gemini opencode]", got)
	}
}
