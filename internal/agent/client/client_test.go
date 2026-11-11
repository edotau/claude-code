package client

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent"

	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// TestMain runs in the hermetic host (hosttest) with the vendor roster registered and the proxy stubbed.
func TestMain(m *testing.M) {
	agent.NoticeOut = io.Discard
	agentconfig.SetCatalogWarn(io.Discard)
	agent.StubProxyForTest("http://127.0.0.1:9999", "test-router-secret")
	os.Exit(hosttest.Main(m))
}

// Explicit Model/Provider/Effort beat the definition's.
func TestResolveOverlayAndAgentsPrecedence(t *testing.T) {
	c := &Client{}
	defs := map[string]*agent.Schema{"sec": {Name: "sec", Prompt: "client", Model: "cm", Profile: "cp", Effort: "low"}}
	o := agent.RunOptions{Agent: "sec", Agents: defs, Model: "mine", Provider: "myprov", Effort: "ultra"}
	if err := c.resolve(&o); err != nil || o.Model != "mine" || o.Provider != "myprov" || o.Effort != "ultra" || o.Agent != "claude" {
		t.Fatalf("explicit options lost: %+v, %v", o, err)
	}
	t.Run("definition defaults", func(t *testing.T) { assertDefinitionOverlay(t, c, defs) })
	if o = (agent.RunOptions{Agent: "ghost"}); c.resolve(&o) == nil {
		t.Error("an unknown ref must fail resolve")
	}
}

func assertDefinitionOverlay(t *testing.T, c *Client, defs map[string]*agent.Schema) {
	t.Helper()
	o := agent.RunOptions{Agent: "sec", Agents: defs}
	if err := c.resolve(&o); err != nil || o.Model != "cm" || o.Provider != "cp" || o.Effort != "low" || o.Persona.Prompt != "client" {
		t.Fatalf("definition not overlaid: %+v, %v", o, err)
	}
}

// A definition with `model: inherit` carries no model of its own, so nothing reaches routing.
func TestOverlayInheritModelIsNone(t *testing.T) {
	c := &Client{}
	o := agent.RunOptions{Agent: "plan", Agents: map[string]*agent.Schema{"plan": {Name: "plan", Prompt: "p", Model: "inherit"}}}
	if err := c.resolve(&o); err != nil || o.Model != "" || o.Persona.Model != "inherit" {
		t.Fatalf("inherit leaked into the request model: %+v, %v", o, err)
	}
}

// Record resolves the selector to its agent but, unlike Prepare, never routes or applies the rubric.
func TestClientRecordSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	defs, err := agent.ParseAgents(`{"sec":{"prompt":"p","agent":"codex"}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(nil, nil, nil)
	o := agent.RunOptions{Agent: "sec", Agents: defs, Prompt: "x", Provider: "no-such-provider", Rubric: "/nonexistent/rubric.md"}
	if _, err := c.Prepare(t.Context(), o, nil); err == nil {
		t.Fatal("control: Prepare should fail on the unknown provider")
	}
	rec, err := c.Record(t.Context(), o, nil)
	if err != nil || rec.Agent != "codex" {
		t.Fatalf("Record = agent %q, err %v; want codex and no error", rec.Agent, err)
	}
}

// scratchRepo makes a throwaway git repo (no commits needed — router.IsWorkRepo only reads the origin
// remote) with origin set to url, so a test controls exactly what "the repo this dispatch runs in" looks like.
func scratchRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", origin)
	return dir
}

// setupWorkdirFixture isolates config state and makes opencode "configured" without a real login, so the
// sub-pick's gates turn on the work-repo rule alone, not on missing credentials.
func setupWorkdirFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	t.Setenv("OPENCODE_API_KEY", "test-key")
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := vault.SetAgentRecord("opencode", vault.AgentRecord{Profile: "zen-free"}); err != nil { // no profile = off
		t.Fatal(err)
	}
}

// A work --workdir must skip free Zen even when the process cwd is a personal repo.
func TestClientPrepareWorkRepoFromWorkdirNotCwd(t *testing.T) {
	setupWorkdirFixture(t)
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Opencode.WorkRepoMarkers = &[]string{"git.corp.example"}
		return nil
	}); err != nil {
		t.Fatalf("UpdateRouting() = %v", err)
	}
	personal := scratchRepo(t, "https://github.com/tester/claude-code.git")
	work := scratchRepo(t, "https://git.corp.example/scm/x/y.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, work)
	if res.Hops[0].Account != "opencode·go" {
		t.Errorf("dispatch.Account = %q, want opencode·go (free blocked on the work repo)", res.Hops[0].Account)
	}
}

// Without --workdir, a personal process cwd still gets the free Zen dispatch.
func TestClientPrepareWorkRepoDefaultsToCwdWhenWorkdirEmpty(t *testing.T) {
	setupWorkdirFixture(t)
	personal := scratchRepo(t, "https://github.com/tester/claude-code.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, "")
	if !strings.Contains(res.Hops[0].Account, "zen-free") {
		t.Errorf("dispatch = %+v, want free Zen on a personal cwd with no --workdir override", res.Hops[0])
	}
}

func runPreparedWorkdir(t *testing.T, workdir string) wire.Result {
	t.Helper()
	client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	p, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "test prompt",
		Fallback: router.FallbackAgents, WorkDir: workdir}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	res, err := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
	if err != nil || len(res.Hops) != 1 || res.Answer != "ok" {
		t.Fatalf("Exec() = %+v, %v; want one successful dispatch", res, err)
	}
	if res.DispatchID != p.Record.ID || res.DispatchID == "" {
		t.Fatalf("dispatch id = %q, want %q", res.DispatchID, p.Record.ID)
	}
	return res
}

// Run keeps going when the dispatch record can't be saved, but says so through Notify instead of dropping it.
func TestClientRunNotifiesUnsavedRecord(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("PATH", t.TempDir()) // no agent binary: the dispatch itself fails fast
	state := filepath.Join(cfg, "state", "harness")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "dispatch"), nil, 0o600); err != nil { // a file where the dir goes
		t.Fatal(err)
	}
	var notes []string
	c := NewClient(nil, nil, func(s string) { notes = append(notes, s) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = c.Run(ctx, agent.RunOptions{Agent: "claude", Provider: "ultra", Prompt: "x"}, nil)
	if !slices.ContainsFunc(notes, func(s string) bool { return strings.Contains(s, "record not saved before the run") }) {
		t.Fatalf("notes = %q, want the pre-run 'record not saved' notice", notes)
	}
}

// burstRunner emits n text events, then blocks on ctx when hold is set.
type burstRunner struct {
	n    int
	hold bool
	ctx  chan context.Context
}

func (*burstRunner) Name() string     { return "fake" }
func (*burstRunner) Caps() agent.Caps { return agent.Caps{} }
func (*burstRunner) Available() error { return nil }
func (b *burstRunner) Run(ctx context.Context, _ agent.RunOptions, w wire.Sink) (wire.Result, error) {
	for range b.n {
		_ = w.Emit(wire.Event{Type: wire.EventText, Text: "x"})
	}
	if b.ctx != nil {
		b.ctx <- ctx
	}
	if b.hold {
		<-ctx.Done()
		return wire.Result{}, context.Cause(ctx)
	}
	return wire.Result{Answer: "a"}, nil
}

func streamPrepared(t *testing.T, r agent.Engine) *Prepared {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return &Prepared{Opts: agent.RunOptions{Agent: "fake", Prompt: "p"}, Record: job.ClaimRecord("fake", agent.RunOptions{Prompt: "p"}, time.Now()), Chain: agent.Single(r)}
}

// streamPreparedRun streams p through c.Exec, as Stream does for a RunOptions.
func streamPreparedRun(c *Client, p *Prepared) <-chan wire.Event {
	return c.streamWith(context.Background(), cmp.Or(p.Opts.Agent, "claude"), func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		return c.Exec(ctx, p, sink, job.RunRecordedOpts{})
	})
}

func drainEvents(ch <-chan wire.Event) (results int, last wire.Event) {
	for ev := range ch {
		if ev.Type == wire.EventResult {
			results++
		}
		last = ev
	}
	return results, last
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() <= base {
			return
		}
	}
	t.Fatalf("goroutines = %d, want <= %d: the run leaked", runtime.NumGoroutine(), base)
}

// Given a consumer that reads nothing, When the run emits 3x the buffer, Then the run still finishes and nothing leaks.
func TestStreamConsumerReadsNothingStillFinishes(t *testing.T) {
	p := streamPrepared(t, &burstRunner{n: 3 * streamBuffer})
	base := runtime.NumGoroutine()
	ch := streamPreparedRun(&Client{}, p)
	// Under suite load other tests' goroutines can drop the count early, so wait for the full buffer too.
	for end := time.Now().Add(5 * time.Second); len(ch) < streamBuffer && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	waitGoroutines(t, base)
	if len(ch) != streamBuffer {
		t.Errorf("buffered = %d, want a full %d (oldest dropped)", len(ch), streamBuffer)
	}
}

// Given a full buffer with drops, Then the stream still ends in exactly one result, as its last event.
func TestStreamFullBufferDropsStillEndsInOneResult(t *testing.T) {
	notes := make(chan string, 2)
	c := &Client{Notify: func(s string) { notes <- s }}
	ch := streamPreparedRun(c, streamPrepared(t, &burstRunner{n: 2 * streamBuffer}))
	select { // the note fires as the run ends with nobody reading, so drops are certain
	case <-notes:
	case <-time.After(5 * time.Second):
		t.Fatal("no dropped-events note within 5s")
	}
	results, last := drainEvents(ch)
	if results != 1 || last.Type != wire.EventResult {
		t.Errorf("results = %d, last = %q, want one result last", results, last.Type)
	}
	if len(notes) != 0 {
		t.Errorf("extra notes = %d, want exactly one dropped-events note", len(notes)+1)
	}
}

// Given a consumer stalled past streamStall, Then the run's ctx is cancelled with errStreamAbandoned.
func TestStreamStalledConsumerCancelsRun(t *testing.T) {
	orig := streamStall
	streamStall = 20 * time.Millisecond
	t.Cleanup(func() { streamStall = orig })
	r := &burstRunner{n: streamBuffer, ctx: make(chan context.Context, 1)}
	ch := (&Client{}).streamWith(context.Background(), "fake", func(ctx context.Context, w wire.Sink) (wire.Result, error) {
		res, err := r.Run(ctx, agent.RunOptions{}, w)
		for ctx.Err() == nil {
			_ = w.Emit(wire.Event{Type: wire.EventText})
			time.Sleep(5 * time.Millisecond)
		}
		return res, err
	})
	ctx := <-r.ctx
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("ctx not cancelled by a stalled consumer")
	}
	if !errors.Is(context.Cause(ctx), errStreamAbandoned) {
		t.Errorf("cause = %v, want errStreamAbandoned", context.Cause(ctx))
	}
	drainEvents(ch)
}

// Given Prepare fails, Then the stream carries one failure result and closes.
func TestStreamPrepareFailureYieldsOneResult(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ch := (&Client{}).Stream(context.Background(), agent.RunOptions{Agent: "no-such-agent", Prompt: "p"})
	results, last := drainEvents(ch)
	if results != 1 || last.Type != wire.EventResult || last.Status != wire.StatusError {
		t.Errorf("results = %d, last = %+v, want one error result", results, last)
	}
}

// The chanSink queues no progress mark for a Client stream.
func TestChanSinkDropsProgressMark(t *testing.T) {
	cs := newChanSink(func(error) {})
	_ = cs.Emit(wire.Event{Type: wire.EventProgress})
	if len(cs.ch) != 0 {
		t.Errorf("stream queued %d events; want none", len(cs.ch))
	}
}

// RunOptions.Lead must reach the router pick: a lead naming no registry row is skipped by name.
func TestClientRouteCarriesLeadToThePick(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	c := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	chain, err := c.route(t.Context(), nil, agent.RunOptions{Agent: "codex", Prompt: "x", Lead: "no-such-row", Fallback: router.FallbackAll})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(chain.Skipped, "\n"), "no-such-row") {
		t.Fatalf("skipped = %q, want a skip naming the lead row no-such-row", chain.Skipped)
	}
}

// Given a pack file that cannot be written, Then Exec fails and the claimed record leaves running.
func TestExecPackWriteFailureFinishesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := job.ClaimRecord("claude", agent.RunOptions{}, time.Now())
	p := &Prepared{Record: rec, pack: &packFile{path: filepath.Join(t.TempDir(), "missing", "pack.md"), body: "x"}}
	res, err := (&Client{}).Exec(context.Background(), p, wire.Discard, job.RunRecordedOpts{})
	if err == nil || res.Status != wire.StatusError {
		t.Fatalf("Exec = %v/%v, want the write error", res.Status, err)
	}
	got, lerr := job.LoadRecord(rec.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if got.Status != "error" {
		t.Errorf("record status = %q, want error", got.Status)
	}
}

// Given a tracked file and a tracked .env, Prepare packs the file only and records the task, not the bytes.
func TestPreparePacksAndKeepsRecordCompact(t *testing.T) {
	setupWorkdirFixture(t)
	dir := gitRepoWith(t, map[string]string{"main.go": "package main // NEEDLE-1", ".env": "TOKEN=hunter2"})
	var notes []string
	client := NewClient(router.RuleClassifier{}.Alias, nil, func(s string) { notes = append(notes, s) })
	p, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "explain", WorkDir: dir,
		Fallback: router.FallbackNone, Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	if !strings.Contains(p.Opts.Prompt, "NEEDLE-1") || strings.Contains(p.Opts.Prompt, "hunter2") {
		t.Errorf("prompt must hold main.go and not .env:\n%s", p.Opts.Prompt)
	}
	if p.Record.Request.Prompt != "explain" || p.Record.Request.Pack == nil {
		t.Errorf("record = %q pack=%v, want the bare task and the pack spec", p.Record.Request.Prompt, p.Record.Request.Pack)
	}
}

func TestPrepareRefusesPackWithHost(t *testing.T) {
	setupWorkdirFixture(t)
	client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	_, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "p", Host: "box",
		Pack: &pack.Spec{Dirs: []string{"."}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("Prepare() = %v, want a --host refusal", err)
	}
}

func gitRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	names := []string{}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	for _, args := range [][]string{{"init", "-q"}, append([]string{"add"}, names...)} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// Given a lead with a 1M window and a stub on a 200K model, an n between their budgets drops only the stub.
func TestFitChainSkipsSmallWindows(t *testing.T) {
	lead := agent.Dispatch{Agent: stubEngine("claude"), Model: "claude-sonnet-5-5[1m]"}
	small := agent.Dispatch{Agent: stubEngine("stub"), Model: "mystery-model"}
	chain := agent.Chain{Dispatches: []agent.Dispatch{lead, small}}
	lb, _ := packBudget(lead, agent.RunOptions{}, false)
	sb, _ := packBudget(small, agent.RunOptions{}, false)
	if sb >= lb {
		t.Fatalf("fixture: small budget %d must be under lead %d", sb, lb)
	}
	got := fitChain(chain, (lb+sb)/2, agent.RunOptions{})
	if len(got.Dispatches) != 1 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0], "stub") {
		t.Fatalf("chain = %+v, want only the lead and a skip naming stub", got)
	}
	if kept := fitChain(chain, sb-1, agent.RunOptions{}); len(kept.Dispatches) != 2 {
		t.Errorf("a prompt under both budgets must keep both, got %+v", kept)
	}
}

func TestPackBudgetArgvCap(t *testing.T) {
	d := agent.Dispatch{Agent: stubEngine("opencode"), Model: "claude-sonnet-5-5[1m]"}
	if got, note := packBudget(d, agent.RunOptions{}, false); got != agent.MaxPromptBytes || !strings.Contains(note, "argv prompt cap") {
		t.Errorf("budget = %d (%q), want MaxPromptBytes with a cap note", got, note)
	}
}

// A PackFile lead keeps the prompt to the instruction, turns tools on, and its file never re-enters a Collect.
func TestPackPromptFileTransport(t *testing.T) {
	setupWorkdirFixture(t)
	body := strings.Repeat("x", 300<<10)
	dir := gitRepoWith(t, map[string]string{"big.txt": body, ".env": "TOKEN=hunter2"})
	lead := agent.Dispatch{Agent: stubEngine("gemini"), Model: "claude-sonnet-5-5[1m]"}
	p := &Prepared{Opts: agent.RunOptions{Prompt: "find it", WorkDir: dir, Pack: &pack.Spec{Dirs: []string{"."}, MaxFileBytes: 400 << 10}},
		Chain: agent.Chain{Dispatches: []agent.Dispatch{lead}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(p.Opts.Prompt) > 1024 || !strings.Contains(p.Opts.Prompt, p.pack.path) || !p.Opts.Tools || !p.Record.Request.Tools {
		t.Fatalf("prompt = %q tools=%v, want a short instruction naming %s with tools on", p.Opts.Prompt, p.Opts.Tools, p.pack.path)
	}
	if len(p.pack.body) < 300<<10 || strings.Contains(p.pack.body, "hunter2") {
		t.Fatalf("pack body = %d bytes, want the 300 KB file and no .env", len(p.pack.body))
	}
	if err := p.pack.write(); err != nil {
		t.Fatal(err)
	}
	got, err := pack.Collect(context.Background(), dir, []string{"."}, nil, 100, 0, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got.Included {
		if strings.HasPrefix(f.Path, ".claude-code-pack-") {
			t.Errorf("Collect picked up the pack file %s", f.Path)
		}
	}
	p.pack.remove()
	if _, err := os.Stat(p.pack.path); !os.IsNotExist(err) {
		t.Errorf("pack file still present after remove: %v", err)
	}
}

// Given a gemini lead (pack read from a file with tools), --read-only keeps the run: agy enforces it with deny rules.
func TestPackPromptKeepsReadOnlyOnFileRoute(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	o := agent.RunOptions{Prompt: "p", WorkDir: dir, ReadOnly: true, Pack: &pack.Spec{Dirs: []string{"."}}}
	p := &Prepared{Opts: o, Chain: agent.Chain{Dispatches: []agent.Dispatch{{Agent: stubEngine("gemini")}}}}
	if err := NewClient(router.RuleClassifier{}.Alias, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatalf("packPrompt() = %v, want the read-only file route accepted", err)
	}
	defer p.pack.remove()
	if !p.Opts.Tools || !p.Opts.ReadOnly {
		t.Fatalf("opts tools=%v read-only=%v, want both on", p.Opts.Tools, p.Opts.ReadOnly)
	}
}

// Given a relative --workdir, the file-route pack path in the prompt is absolute (the child starts inside the workdir).
func TestPackPromptFilePathAbsoluteForRelativeWorkdir(t *testing.T) {
	dir := gitRepoWith(t, map[string]string{"a.go": "package a"})
	t.Chdir(dir)
	if err := os.Mkdir("sub", 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{Opts: agent.RunOptions{Prompt: "p", WorkDir: "sub", Pack: &pack.Spec{Dirs: []string{"."}}},
		Chain: agent.Chain{Dispatches: []agent.Dispatch{{Agent: stubEngine("gemini")}}}}
	p.Record.ID = "id1"
	if err := NewClient(nil, nil, nil).packPrompt(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs("sub")
	if want := filepath.Join(abs, pack.FilePrefix+"id1.md"); p.pack.path != want || !strings.Contains(p.Opts.Prompt, want) {
		t.Fatalf("pack path = %q, prompt = %q, want absolute %q", p.pack.path, p.Opts.Prompt, want)
	}
}

// stubEngine is an agent.Engine that only has a name; the pack tests never run it.
type stubEngine string

func (s stubEngine) Name() string     { return string(s) }
func (s stubEngine) Caps() agent.Caps { return agent.Caps{} }
func (s stubEngine) Available() error { return nil }
func (s stubEngine) Run(context.Context, agent.RunOptions, wire.Sink) (wire.Result, error) {
	return wire.Result{}, nil
}

const parityRows = `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com",` +
	`"auth":{"type":"none"},"models":{"opus":"databricks-claude-opus-5-5"}}},` +
	`"anthropic":{"alt":{"base_url":"https://alt.example.com","protocol":"anthropic","auth":{"type":"none"},"models":{"opus":"claude-opus-5-5"}}}}}`

var codexModelArg = regexp.MustCompile(`^model="(.*)"$`)

func launchModel(t *testing.T, name string, o agent.LaunchOptions) string {
	t.Helper()
	o.PrintEnv = true
	plan, err := agent.LaunchPlan(context.Background(), name, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := plan.Set["COPILOT_MODEL"]; m != "" {
		return m
	}
	for i, a := range plan.Argv {
		if m := codexModelArg.FindStringSubmatch(a); m != nil && i > 0 && plan.Argv[i-1] == "-c" {
			return m[1]
		}
		if a == "--model" && i+1 < len(plan.Argv) {
			return plan.Argv[i+1]
		}
	}
	t.Fatalf("no model in %v", plan.Argv)
	return ""
}

func TestLaunchAndAgentAgreeOnModel(t *testing.T) {
	cases := []struct{ agent, profile, flag string }{
		{"codex", "dev", ""},
		{"codex", "dev", "gpt-5.5"},
		{"copilot", "alt", ""},
		{"copilot", "alt", "claude-sonnet-5-5"},
	}
	for _, c := range cases {
		t.Run(c.agent+"/"+c.profile+"/"+c.flag, func(t *testing.T) {
			vaulttest.Use(t)
			vaulttest.Put(t, vault.Overlay, "providers", parityRows)
			pathstest.FakeVendor(t, c.agent, "")
			pin := "databricks-gpt-6-1-sol"
			if c.agent == "copilot" {
				pin = "claude-haiku-5-5"
			}
			vaulttest.SetRecord(t, c.agent, "model", pin)
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			tg, _, err := agent.SchemaFor(c.agent).Target(agent.RunOptions{Provider: c.profile, Model: c.flag})
			if err != nil {
				t.Fatal(err)
			}
			got := launchModel(t, c.agent, agent.LaunchOptions{Provider: c.profile, Model: c.flag})
			if got != tg.Model {
				t.Errorf("launch %q vs agent %q", got, tg.Model)
			}
			if c.flag == "" && !strings.Contains(got, pin) {
				t.Errorf("model %q ignores the pin %q", got, pin)
			}
		})
	}
}

func TestVendorSignInSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "ultra") // neither codex nor gemini can use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	offers := 0
	agent.OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { agent.OfferConfigure = nil })
	for _, c := range []struct {
		vendor, bin string
		args        []string
	}{
		{"codex", "codex", []string{"login", "--device-auth"}},
		{"codex", "codex", []string{"logout"}},
		{"gemini", "agy", []string{"auth", "login"}},
	} {
		fake := pathstest.FakeVendor(t, c.bin, "")
		plan, err := agent.LaunchPlan(context.Background(), c.vendor, agent.LaunchOptions{}, c.args)
		if err != nil {
			t.Fatalf("%s %v: err = %v, want a plain sign-in launch", c.vendor, c.args, err)
		}
		if plan.Binary != fake || len(plan.Set) != 0 || !slices.Equal(plan.Argv[len(plan.Argv)-len(c.args):], c.args) {
			t.Errorf("%s %v: plan = %+v, want the plain CLI with the args and no harness env", c.vendor, c.args, plan)
		}
	}
	if offers != 0 {
		t.Errorf("configure offered %d times for sign-in commands, want 0", offers)
	}
}
