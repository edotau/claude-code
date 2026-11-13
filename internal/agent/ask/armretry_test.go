package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
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

// Parallel legs that move workspaces together each report only their own workspaces (run with -race).
func TestParallelLegsKeepTheirOwnWorkspaces(t *testing.T) {
	armRateFixture(t, []string{"dev"}, nil)
	cl := newClient()
	hops, answered := make([][]string, 2), make([]string, 2)
	var wg sync.WaitGroup
	for i, start := range []string{"dev", "qa"} {
		wg.Go(func() {
			a := &armRetry{agentName: "codex", fallback: router.FallbackNone, tried: []string{start}}
			o := agent.RunOptions{Agent: "codex", Prompt: "go", Fallback: router.FallbackNone, AutoArm: true}
			env := a.streamLeg(t.Context(), cl, o, make(chan wire.Event, 256))
			answered[i] = env.Provider
			if env.Error != nil {
				t.Logf("leg %d error: %s", i, env.Error.Message)
			}
			for _, h := range env.Hops {
				hops[i] = append(hops[i], h.Profile)
			}
		})
	}
	wg.Wait()
	if got := strings.Join(hops[0], ","); got != "dev,qa" {
		t.Errorf("leg starting on dev hops %q, want dev,qa", got)
	}
	if slices.Contains(hops[1], "dev") || !strings.Contains(answered[1], "qa") {
		t.Errorf("leg starting on qa: hops %v answered by %q, want qa and never dev", hops[1], answered[1])
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

// A 402 on a probe attempt re-runs the same workspace under the fallback policy the probe skipped, never another one.
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
		t.Errorf("launches %v, want codex on dev twice: the probe, then the fallback policy", bins)
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

// holdArm runs an attempt on arm that stays inside the gate until the returned release (also run at cleanup).
func holdArm(t *testing.T, arm string) (release func()) {
	t.Helper()
	started, done := make(chan struct{}), make(chan struct{})
	go (&armRetry{tried: []string{arm}}).exec(t.Context(), nil, func(context.Context, *agent.Prepared) (wire.Result, error) {
		close(started)
		<-done
		return wire.Result{}, nil
	})
	<-started
	var once sync.Once
	release = func() { once.Do(func() { close(done) }) }
	t.Cleanup(release)
	return release
}

// A leg waiting at the gate for another workspace leaves with ctx's error instead of waiting for the holder.
func TestArmGateCancelledWaiterLeaves(t *testing.T) {
	armRateFixture(t, nil, nil)
	holdArm(t, "dev")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var ran bool // written before the res send, read after the receive
	res := make(chan error, 1)
	go func() {
		_, err := (&armRetry{tried: []string{"qa"}}).exec(ctx, nil, func(context.Context, *agent.Prepared) (wire.Result, error) {
			ran = true
			return wire.Result{}, nil
		})
		res <- err
	}()
	select {
	case err := <-res:
		if !errors.Is(err, context.DeadlineExceeded) || ran {
			t.Errorf("waiter err %v ran %v, want the deadline error and no run", err, ran)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter still blocked at the gate")
	}
}

// The gate's edges: no legs, one leg, a leg re-entering its own workspace, and a context cancelled before it joins.
func TestArmGateEdges(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{`+
		`"dev":{"preset":"databricks","host":"https://dev.cloud.databricks.com","auth":{"type":"key","env":"DEV_WS_TOKEN"}}}}}`)
	t.Setenv(agentconfig.OverlayVar, "")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	live, stop := context.WithTimeout(t.Context(), 2*time.Second) // a deadlocked entry fails here instead of hanging
	defer stop()
	for _, c := range []struct {
		name    string
		ctx     context.Context
		arms    []string
		wantN   int
		wantArm string
		wantErr bool
	}{
		{"no legs", live, nil, 0, "", false},
		{"one leg", live, []string{"dev"}, 1, "dev", false},
		{"re-entering its own workspace", live, []string{"dev", "dev"}, 2, "dev", false},
		{"cancelled before joining", cancelled, []string{"dev"}, 0, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(providers.EnvSessionProfile, "")
			var err error
			for _, arm := range c.arms {
				if err = enterArm(c.ctx, arm); err != nil {
					break
				}
			}
			n, queued := gateState()
			arm := os.Getenv(providers.EnvSessionProfile)
			for range n {
				leaveArm()
			}
			if got := fmt.Sprint(n, queued, arm, err != nil); got != fmt.Sprint(c.wantN, 0, c.wantArm, c.wantErr) {
				t.Errorf("entered/queued/arm/failed = %s, want %d 0 %q %v (err %v)", got, c.wantN, c.wantArm, c.wantErr, err)
			}
		})
	}
	if n, queued := gateState(); n != 0 || queued != 0 {
		t.Errorf("gate left at n=%d queue=%d, want idle", n, queued)
	}
}

// gateState is how many legs the arm gate holds and how many wait at it.
func gateState() (n, queued int) {
	armGate.cond.L.Lock()
	defer armGate.cond.L.Unlock()
	return armGate.n, len(armGate.queue)
}

// Once another workspace waits, a new same-workspace leg queues behind it instead of joining the holders.
func TestArmGateWaitingWorkspaceIsNotStarved(t *testing.T) {
	armRateFixture(t, nil, nil)
	release := holdArm(t, "dev")
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	enter := func(arm string) {
		wg.Go(func() {
			_, _ = (&armRetry{tried: []string{arm}}).exec(t.Context(), nil, func(context.Context, *agent.Prepared) (wire.Result, error) {
				mu.Lock()
				order = append(order, arm)
				mu.Unlock()
				return wire.Result{}, nil
			})
		})
		time.Sleep(50 * time.Millisecond) // let it reach the gate before the next one starts
	}
	enter("qa")
	enter("dev")
	mu.Lock()
	early := slices.Clone(order)
	mu.Unlock()
	release()
	wg.Wait()
	if len(early) != 0 || !slices.Equal(order, []string{"qa", "dev"}) {
		t.Errorf("ran %v before release, %v overall; want none, then qa before dev", early, order)
	}
}
