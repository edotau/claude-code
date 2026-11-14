package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// armRateFixture is armChildEnvFixture with a codex fake that answers 429 for each workspace in limited, 402 for
// each in broke, and "hello" elsewhere; it returns the dump dir of the launches (one file per launch).
func armRateFixture(t *testing.T, limited, broke []string) string {
	t.Helper()
	dump := armChildEnvFixture(t)
	var cond strings.Builder
	for _, w := range limited {
		fmt.Fprintf(&cond, `*"/p/%s/"*) echo '429 Too Many Requests: rate limit exceeded' >&2; exit 1;; `, w)
	}
	for _, w := range broke {
		fmt.Fprintf(&cond, `*"/p/%s/"*) echo '402 Payment Required: monthly quota exceeded' >&2; exit 1;; `, w)
	}
	pathstest.FakeVendor(t, "codex", `case "$1 $2" in login*) echo "Logged in using ChatGPT"; exit 0;; `+
		`--version*|*--help*|debug*) echo 1.0.0; exit 0;; esac`+"\n"+`n=$(ls `+dump+` | wc -l | tr -d ' ')`+"\n"+
		`all="$* $(env)"; echo "$all" > `+dump+`/$n-codex`+"\n"+
		`cat > /dev/null; case "$all" in `+cond.String()+`*) echo hello;; esac`)
	origRetry := [3]time.Duration{retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor}
	retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = time.Millisecond, 2*time.Millisecond, time.Millisecond
	retry.ResetCooldowns()
	origFindings := armFindings
	armFindings = noCredential(nil)
	t.Cleanup(func() {
		retry.DefaultBaseDelay, retry.DefaultMaxDelay, retry.RateLimitFloor = origRetry[0], origRetry[1], origRetry[2]
		armFindings = origFindings
		retry.ResetCooldowns()
	})
	t.Setenv(providers.EnvSessionProfile, "")
	t.Setenv(agentconfig.OverlayVar, "")
	return dump
}

// launchedOn is the workspace of each codex launch in the dump dir, in launch order.
func launchedOn(t *testing.T, dump string) []string {
	t.Helper()
	_, envs := armDumps(t, dump)
	var out []string
	for _, e := range envs {
		all := strings.Join(e, "\n")
		switch {
		case strings.Contains(all, "/p/dev/"):
			out = append(out, "dev")
		case strings.Contains(all, "/p/qa/"):
			out = append(out, "qa")
		default:
			out = append(out, "?")
		}
	}
	return out
}

func askAuto(t *testing.T, profile, fallback string) (int, map[string]any, string) {
	t.Helper()
	ov := `{"provider":"databricks","profile":"` + profile + `","fallback":"` + fallback + `"}`
	code, out, stderr := runCLI(t, "ask", "--agent", "codex", "--overlay", ov, "--format", "json", "go")
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &env); err != nil {
		t.Fatalf("exit %d: stdout is not one envelope: %v\n%s\n%s", code, err, out, stderr)
	}
	return code, env, stderr
}

func hopProfiles(env map[string]any) []string {
	var out []string
	hops, _ := env["hops"].([]any)
	for _, h := range hops {
		out = append(out, h.(map[string]any)["profile"].(string))
	}
	return out
}

// A 429 on the auto-picked workspace cools it and re-runs the same agent on the next one; both are listed, in order.
func TestAutoRateLimitMovesToNextWorkspace(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, stderr := askAuto(t, "auto", "none")
	if code != 0 || env["status"] != "ok" || env["agent"] != "codex" {
		t.Fatalf("exit %d status %v agent %v, want an ok codex answer\n%s", code, env["status"], env["agent"], stderr)
	}
	if got := launchedOn(t, dump); got[0] != "dev" || got[len(got)-1] != "qa" {
		t.Errorf("launches on %v, want dev first and qa last", got)
	}
	checkMovedReport(t, env)
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	if until := router.LoadState().CoolingUntil(router.AccountOf(reg.Providers["dev"])); !until.After(time.Now()) {
		t.Errorf("dev cooling until %v, want a hold after the 429", until)
	}
}

// checkMovedReport asserts the envelope lists the hops dev then qa and names the move in ran_on.
func checkMovedReport(t *testing.T, env map[string]any) {
	t.Helper()
	if got := strings.Join(hopProfiles(env), ","); !strings.HasPrefix(got, "dev") || !strings.HasSuffix(got, "qa") {
		t.Errorf("hops %q, want every workspace tried in order: dev then qa", got)
	}
	if ran, _ := env["ran_on"].(string); !strings.Contains(ran, "tried dev → qa") {
		t.Errorf("ran_on %q, want it to name dev → qa", ran)
	}
}

// Every workspace limited: --fallback none still fails, naming each workspace tried and keeping the rate_limit kind.
func TestAutoRateLimitEveryWorkspaceFails(t *testing.T) {
	dump := armRateFixture(t, []string{"dev", "qa"}, nil)
	code, env, stderr := askAuto(t, "auto", "none")
	msg, _ := env["error"].(map[string]any)["message"].(string)
	kind, _ := env["error"].(map[string]any)["kind"].(string)
	if code == 0 || kind != "rate_limit" || !strings.Contains(msg, "tried dev, qa") {
		t.Fatalf("exit %d kind %q message %q, want a rate_limit failure naming dev, qa\n%s", code, kind, msg, stderr)
	}
	if got := strings.Join(launchedOn(t, dump), ","); !strings.Contains(got, "dev") || !strings.Contains(got, "qa") {
		t.Errorf("launches %q, want both workspaces asked", got)
	}
}

// An explicitly named workspace never moves: its 429 is the failure.
func TestExplicitProfileRateLimitStays(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, _ := askAuto(t, "dev", "none")
	if code == 0 || env["error"] == nil {
		t.Fatalf("exit %d, want the rate limit to fail the ask", code)
	}
	for _, w := range launchedOn(t, dump) {
		if w != "dev" {
			t.Errorf("launches %v, want only dev", launchedOn(t, dump))
		}
	}
}

// A failure that is not a rate limit stays on its workspace: only the fallback policy applies.
func TestAutoNonRateLimitFailureDoesNotMove(t *testing.T) {
	dump := armRateFixture(t, nil, []string{"dev"})
	if code, _, _ := askAuto(t, "auto", "none"); code == 0 {
		t.Fatal("a 402 on the picked workspace answered, want the failure")
	}
	for _, w := range launchedOn(t, dump) {
		if w != "dev" {
			t.Errorf("launches %v, want only dev: a 402 is not a rate limit", launchedOn(t, dump))
		}
	}
}

// With the default fallback the 429 still moves workspaces first: codex answers on qa, no other agent runs.
func TestAutoRateLimitMovesBeforeAgentFallback(t *testing.T) {
	dump := armRateFixture(t, []string{"dev"}, nil)
	code, env, stderr := askAuto(t, "auto", "all")
	bins, _ := armDumps(t, dump)
	if code != 0 || env["agent"] != "codex" || env["fallback"] != nil {
		t.Fatalf("exit %d agent %v fallback %v, want codex answering on qa with no fallback\n%s", code, env["agent"], env["fallback"], stderr)
	}
	for _, b := range bins {
		if b != "codex" {
			t.Errorf("launches %v, want only codex before any agent fallback", bins)
		}
	}
}

// A --job child on an auto pick retries like a foreground ask; --session, --live and --host never move.
func TestArmRunServesJobAndKeepsExclusions(t *testing.T) {
	run := func(mut func(*askRun)) *armRetry {
		r := &askRun{f: &askFlags{}, auto: "dev", jobRec: &agent.Record{Request: agent.RunOptions{AutoArm: true}}}
		mut(r)
		return r.armRun()
	}
	if a := run(func(*askRun) {}); a == nil || !a.job {
		t.Fatalf("armRun = %+v, want a job retry", a)
	}
	for name, mut := range map[string]func(*askRun){
		"session": func(r *askRun) { r.f.session = "s" }, "live": func(r *askRun) { r.f.live = true },
		"host": func(r *askRun) { r.f.host = "h" }, "not auto": func(r *askRun) { r.auto = "" },
	} {
		if a := run(mut); a != nil {
			t.Errorf("%s: armRun = %+v, want nil", name, a)
		}
	}
}

// A 402 uses the requested fallback chain once, without replaying the lead or moving workspaces.
func TestAutoProbeFailureAppliesFallbackPolicyOnSameWorkspace(t *testing.T) {
	dump := armRateFixture(t, nil, []string{"dev"})
	askAuto(t, "auto", "all")
	bins, _ := armDumps(t, dump)
	codex := 0
	for _, b := range bins {
		if b == "codex" {
			codex++
		}
	}
	if codex != 2 {
		t.Errorf("launches %v, want the two codex dispatches in the requested fallback chain", bins)
	}
	for _, w := range launchedOn(t, dump) {
		if w == "qa" {
			t.Errorf("launches %v: a 402 moved to qa", launchedOn(t, dump))
		}
	}
}

// With stream JSON a moved ask emits ONE result event, the final success, never a failed attempt's.
func TestAutoRateLimitStreamJSONEmitsOneResult(t *testing.T) {
	armRateFixture(t, []string{"dev"}, nil)
	ov := `{"provider":"databricks","profile":"auto","fallback":"none"}`
	_, out, stderr := runCLI(t, "ask", "--agent", "codex", "--overlay", ov, "--format", "stream-json", "go")
	var results []wire.Event
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var ev wire.Event
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == wire.EventResult {
			results = append(results, ev)
		}
	}
	if len(results) != 1 || results[0].Status != wire.StatusOK || !strings.Contains(results[0].Envelope.RanOn, "tried dev → qa") {
		t.Errorf("result events %+v, want one ok result naming dev → qa\n%s\n%s", results, out, stderr)
	}
}

// A --job run that moves workspaces keeps ONE record: the published id ends ok with every attempt as a hop.
func TestJobRetryKeepsOneRecord(t *testing.T) {
	armRateFixture(t, []string{"dev"}, nil)
	cl := newClient()
	a := &armRetry{agentName: "codex", fallback: router.FallbackNone, format: helptext.Text, out: &bytes.Buffer{}, job: true,
		tried: []string{"dev"}}
	req := agent.RunOptions{Agent: "codex", Prompt: "go", Fallback: router.FallbackNone, AutoArm: true}
	p, err := a.prepare(t.Context(), cl, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Record.Save(); err != nil {
		t.Fatal(err)
	}
	if code := a.run(t.Context(), cl, p, req); code != 0 {
		t.Fatalf("run exit %d, want 0", code)
	}
	rec, err := agent.LoadRecord(p.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	env := rec.ResultEnvelope(&bytes.Buffer{})
	var hops []string
	for _, h := range env.Hops {
		hops = append(hops, h.Profile)
	}
	if rec.Status != "ok" || env.Status != wire.StatusOK || strings.Join(hops, ",") != "dev,qa" {
		t.Errorf("published record %s status %q, result %q, hops %v; want ok, ok, dev,qa", rec.ID, rec.Status, env.Status, hops)
	}
	if n := len(agent.RecentRecords(agent.AllRecords)); n != 1 {
		t.Errorf("%d dispatch records, want the one published id", n)
	}
}

// Two attempts on different workspaces both reach Prepare before either finishes, each on its own run values.
func TestArmAttemptsOnDifferentWorkspacesRunTogether(t *testing.T) {
	armRateFixture(t, nil, nil)
	entered, release := make(chan string, 2), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open) // a failed run must not leave an attempt parked
	old := armPrepare
	armPrepare = func(_ *agent.Client, _ context.Context, req agent.RunOptions, _ *agent.Record) (*agent.Prepared, error) {
		entered <- req.Values[providers.EnvSessionProfile]
		<-release
		return nil, errors.New("stub: not prepared")
	}
	t.Cleanup(func() { armPrepare = old })
	var wg sync.WaitGroup
	for _, arm := range []string{"dev", "qa"} {
		wg.Go(func() {
			_, _ = (&armRetry{tried: []string{arm}}).prepare(t.Context(), newClient(), agent.RunOptions{}, nil)
		})
	}
	var got []string
	for range 2 {
		select {
		case arm := <-entered:
			got = append(got, arm)
		case <-time.After(5 * time.Second): // reached only when an attempt is held back by the other
			t.Fatalf("attempts entered %v, want both workspaces in Prepare before either finishes", got)
		}
	}
	open()
	wg.Wait()
	if slices.Sort(got); !slices.Equal(got, []string{"dev", "qa"}) {
		t.Errorf("attempts ran on %v, want dev and qa each on its own", got)
	}
}

// retryStub keeps retry regressions off vendor binaries and the network.
type retryStub struct {
	name string
	run  func(agent.RunOptions, wire.Sink) (wire.Result, error)
}

func (s retryStub) Name() string     { return s.name }
func (s retryStub) Caps() agent.Caps { return agent.Caps{} }
func (s retryStub) Available() error { return nil }
func (s retryStub) Run(_ context.Context, req agent.RunOptions, sink wire.Sink) (wire.Result, error) {
	return s.run(req, sink)
}

func retryUnitFixture(t *testing.T) {
	t.Helper()
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+
		`"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"none"}},`+
		`"qa":{"preset":"databricks","host":"https://qa.cloud.databricks.com","auth":{"type":"none"}}}}}`)
	t.Setenv(agentconfig.OverlayVar, "")
	t.Setenv(providers.EnvSessionProfile, "")
	oldFind, oldLive, oldCool := armFindings, armLive, armCooling
	armFindings = noCredential(nil)
	armLive = func([]*providers.Provider) map[string]int { return nil }
	armCooling = func(*providers.Provider, time.Time) bool { return false }
	t.Cleanup(func() { armFindings, armLive, armCooling = oldFind, oldLive, oldCool })
}

func retryUnit(t *testing.T, run func(*testing.T)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		retryUnitFixture(t)
		run(t)
	})
}

// A progressed probe keeps the requested chain and reconciles through its successor without workspace replay.
func TestArmProbePreservesRequestedFallbackAfterProgress(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.example.com"},"qa":{"preset":"databricks","host":"https://qa.example.com"}}}}`)
	orig := armFindings
	armFindings = noCredential(nil)
	t.Cleanup(func() { armFindings = orig })
	if !armsLeft("lead-stub", "", []string{"dev"}) {
		t.Fatal("fixture has no spare workspace")
	}
	a := &armRetry{agentName: "lead-stub", fallback: "all", tried: []string{"dev"}}
	req := a.probe(agent.RunOptions{Prompt: "task", Fallback: "all", Tools: true})
	lead := &armFallbackStub{name: "lead-stub", err: &retry.Error{
		Kind: retry.KindRateLimit, Progressed: true, Err: errors.New("429")}}
	next := &armFallbackStub{name: "fallback-stub"}
	chain := agent.Chain{Want: lead.name, Dispatches: []agent.Dispatch{{Agent: lead}}}
	if req.Fallback == "all" {
		chain.Dispatches = append(chain.Dispatches, agent.Dispatch{Agent: next})
	}
	p := &agent.Prepared{Opts: req, Chain: chain}
	res, err := new(agent.Client).Exec(t.Context(), p, wire.Discard, agent.RunRecordedOpts{Hold: true})
	if err != nil || len(lead.seen) != 1 || len(next.seen) != 1 {
		t.Fatalf("answer=%q error=%v lead calls=%d fallback calls=%d policy=%q",
			res.Answer, err, len(lead.seen), len(next.seen), req.Fallback)
	}
	if !strings.Contains(next.seen[0].Prompt, "reconcile") || !strings.Contains(next.seen[0].Prompt, "partial") {
		t.Fatalf("fallback prompt lacks reconciliation: %q", next.seen[0].Prompt)
	}
	if _, again := a.next(t.Context(), res, err, req); again {
		t.Fatal("successful reconciliation replayed the task on another workspace")
	}
}

type armFallbackStub struct {
	name string
	err  error
	seen []agent.RunOptions
}

func (s *armFallbackStub) Name() string     { return s.name }
func (s *armFallbackStub) Caps() agent.Caps { return agent.Caps{} }
func (s *armFallbackStub) Available() error { return nil }
func (s *armFallbackStub) Run(_ context.Context, req agent.RunOptions, _ wire.Sink) (wire.Result, error) {
	s.seen = append(s.seen, req)
	return wire.Result{Answer: "partial"}, s.err
}

func TestArmProbeYieldsOnlyRetryableWorkspaceFailures(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"qa":{"preset":"databricks","host":"https://qa.example.com"}}}}`)
	orig := armFindings
	armFindings = noCredential(nil)
	t.Cleanup(func() { armFindings = orig })
	for _, tc := range []struct {
		name       string
		kind       retry.Kind
		progressed bool
		answer     string
		tried      []string
		want       bool
	}{
		{"pre-progress 429", retry.KindRateLimit, false, "", []string{"dev"}, true},
		{"tool progress", retry.KindRateLimit, true, "", []string{"dev"}, false},
		{"text progress", retry.KindRateLimit, false, "partial", []string{"dev"}, false},
		{"quota", retry.KindQuota, false, "", []string{"dev"}, false},
		{"last workspace", retry.KindRateLimit, false, "", []string{"dev", "qa"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &armRetry{agentName: "codex", fallback: "all", tried: tc.tried}
			req := a.probe(agent.RunOptions{Fallback: "all"})
			err := &retry.Error{Kind: tc.kind, Progressed: tc.progressed, Err: errors.New("stub")}
			if got := req.YieldFallback(wire.Result{Answer: tc.answer}, err); got != tc.want {
				t.Fatalf("yield=%v, want %v", got, tc.want)
			}
		})
	}
}
