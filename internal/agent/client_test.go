package agent

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Explicit Model/Provider/Effort beat the definition's.
func TestResolveOverlayAndAgentsPrecedence(t *testing.T) {
	c := &Client{}
	defs := map[string]*Schema{"sec": {Name: "sec", Prompt: "client", Model: "cm", Profile: "cp", Effort: "low"}}
	o := RunOptions{Agent: "sec", Agents: defs, Model: "mine", Provider: "myprov", Effort: "max"}
	if err := c.resolve(&o); err != nil || o.Model != "mine" || o.Provider != "myprov" || o.Effort != "max" || o.Agent != "claude" {
		t.Fatalf("explicit options lost: %+v, %v", o, err)
	}
	t.Run("definition defaults", func(t *testing.T) { assertDefinitionOverlay(t, c, defs) })
	if o = (RunOptions{Agent: "ghost"}); c.resolve(&o) == nil {
		t.Error("an unknown ref must fail resolve")
	}
}

func assertDefinitionOverlay(t *testing.T, c *Client, defs map[string]*Schema) {
	t.Helper()
	o := RunOptions{Agent: "sec", Agents: defs}
	if err := c.resolve(&o); err != nil || o.Model != "cm" || o.Provider != "cp" || o.Effort != "low" || o.Persona.Prompt != "client" {
		t.Fatalf("definition not overlaid: %+v, %v", o, err)
	}
}

// A definition with `model: inherit` carries no model of its own, so nothing reaches routing.
func TestOverlayInheritModelIsNone(t *testing.T) {
	c := &Client{}
	o := RunOptions{Agent: "plan", Agents: map[string]*Schema{"plan": {Name: "plan", Prompt: "p", Model: "inherit"}}}
	if err := c.resolve(&o); err != nil || o.Model != "" || o.Persona.Model != "inherit" {
		t.Fatalf("inherit leaked into the request model: %+v, %v", o, err)
	}
}

// Record resolves the selector to its agent but, unlike Prepare, never routes or applies the rubric.
func TestClientRecordSkipsRouting(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	defs, err := ParseAgents(`{"sec":{"prompt":"p","agent":"codex"}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(nil, nil, nil)
	o := RunOptions{Agent: "sec", Agents: defs, Prompt: "x", Provider: "no-such-provider", Rubric: "/nonexistent/rubric.md"}
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
	personal := scratchRepo(t, "https://github.com/edotau/claude-code.git")
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
	personal := scratchRepo(t, "https://github.com/edotau/claude-code.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, "")
	if !strings.Contains(res.Hops[0].Account, "zen-free") {
		t.Errorf("dispatch = %+v, want free Zen on a personal cwd with no --workdir override", res.Hops[0])
	}
}

func runPreparedWorkdir(t *testing.T, workdir string) wire.Result {
	t.Helper()
	client := NewClient(router.RuleClassifier{}.Alias, nil, nil)
	p, err := client.Prepare(context.Background(), RunOptions{Agent: "opencode", Prompt: "test prompt",
		Fallback: router.FallbackAgents, WorkDir: workdir}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	res, err := client.Exec(context.Background(), p, nil, RunRecordedOpts{})
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
	_, _ = c.Run(ctx, RunOptions{Agent: "claude", Provider: "max", Prompt: "x"}, nil)
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
func (*burstRunner) Caps() Caps       { return Caps{} }
func (*burstRunner) Available() error { return nil }
func (b *burstRunner) Run(ctx context.Context, _ RunOptions, w wire.Sink) (wire.Result, error) {
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

func streamPrepared(t *testing.T, r Engine) *Prepared {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	return &Prepared{Opts: RunOptions{Agent: "fake", Prompt: "p"}, Record: ClaimRecord("fake", RunOptions{Prompt: "p"}, time.Now()), chain: Single(r)}
}

// streamPreparedRun streams p through c.Exec, as Stream does for a RunOptions.
func streamPreparedRun(c *Client, p *Prepared) <-chan wire.Event {
	return c.streamWith(context.Background(), cmp.Or(p.Opts.Agent, "claude"), func(ctx context.Context, sink wire.Sink) (wire.Result, error) {
		return c.Exec(ctx, p, sink, RunRecordedOpts{})
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

// Given a consumer stalled past streamStall, Then the run's ctx is cancelled with ErrStreamAbandoned.
func TestStreamStalledConsumerCancelsRun(t *testing.T) {
	orig := streamStall
	streamStall = 20 * time.Millisecond
	t.Cleanup(func() { streamStall = orig })
	r := &burstRunner{n: streamBuffer, ctx: make(chan context.Context, 1)}
	ch := (&Client{}).streamWith(context.Background(), "fake", func(ctx context.Context, w wire.Sink) (wire.Result, error) {
		res, err := r.Run(ctx, RunOptions{}, w)
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
	if !errors.Is(context.Cause(ctx), ErrStreamAbandoned) {
		t.Errorf("cause = %v, want ErrStreamAbandoned", context.Cause(ctx))
	}
	drainEvents(ch)
}

// Given Prepare fails, Then the stream carries one failure result and closes.
func TestStreamPrepareFailureYieldsOneResult(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ch := (&Client{}).Stream(context.Background(), RunOptions{Agent: "no-such-agent", Prompt: "p"})
	results, last := drainEvents(ch)
	if results != 1 || last.Type != wire.EventResult || last.Status != wire.StatusError {
		t.Errorf("results = %d, last = %+v, want one error result", results, last)
	}
}
