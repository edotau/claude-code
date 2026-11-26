package ask

import (
	"cmp"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/route"
)

// Two batches started in the same millisecond still get distinct batch ids.
func TestNewIDDistinctAtOneInstant(t *testing.T) {
	old := batchNow
	batchNow = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { batchNow = old })
	a, b := newID(), newID()
	if a == b || !strings.HasPrefix(a, "batch-20261029T120000.000") {
		t.Errorf("ids %q and %q: want distinct ids that keep the batch-<stamp> prefix", a, b)
	}
}

// plan_task is validated per line: a bad one fails only its task, with the line number; a heading with a space passes.
func TestParseTaskPlanTask(t *testing.T) {
	in := `{"id":"a","prompt":"p","plan_task":"Phase 1.1"}` + "\n" + `{"id":"b","prompt":"p","plan_task":"bad/id"}` + "\n"
	lines, err := readTaskLines(strings.NewReader(in))
	if err != nil || len(lines) != 2 {
		t.Fatalf("readTaskLines = %d lines, %v", len(lines), err)
	}
	seen := map[string]bool{}
	good, err := parseTask(lines[0], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err != nil || good.Plan != "Phase 1.1" {
		t.Fatalf("Phase 1.1: plan %q, err %v", good.Plan, err)
	}
	bad, err := parseTask(lines[1], "codex", agent.RunOptions{}, false, t.TempDir(), seen)
	if err == nil || !strings.Contains(err.Error(), "line 2") || !strings.Contains(err.Error(), "plan_task") {
		t.Fatalf("bad/id error = %v, want line 2 plan_task error", err)
	}
	if bad.ID != "b" || bad.Plan != "" {
		t.Errorf("rejected task = id %q plan %q, want id kept and no plan", bad.ID, bad.Plan)
	}
}

// A line with trailing junk after its object is that line's error; Decoder.More alone accepts a stray '}'.
func TestReadTaskLinesRejectsTrailingJunk(t *testing.T) {
	in := "{\"prompt\":\"x\"}}\n{\"prompt\":\"a\"} {\"prompt\":\"b\"}\n{\"prompt\":\"ok\"}\n"
	got, err := readTaskLines(strings.NewReader(in))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
	if got[0].Err == nil || got[1].Err == nil || got[2].Err != nil {
		t.Errorf("errs = %v / %v / %v, want error, error, nil", got[0].Err, got[1].Err, got[2].Err)
	}
}

func TestBatchLineKeyHint(t *testing.T) {
	for line, want := range map[string]string{
		`{"task":"x"}`:                  `did you mean "prompt"`,
		`{"promt":"x"}`:                 `did you mean "prompt"`,
		`{"noTools":true,"prompt":"x"}`: `put --no-tools on the --batch command`,
		`{"prompt":"x"}`:                "",
	} {
		lines, err := readTaskLines(strings.NewReader(line + "\n"))
		if err != nil || len(lines) != 1 {
			t.Fatalf("readTaskLines(%s) = %d, %v", line, len(lines), err)
		}
		got := lines[0].Err
		if (want == "") != (got == nil) || got != nil && !strings.Contains(got.Error(), want) {
			t.Errorf("%s: error %v, want %q", line, got, want)
		}
	}
}

func TestBatchLineOverlayRoute(t *testing.T) {
	base := agent.RunOptions{Provider: "ultra", Lead: "ultra"}
	lines, err := readTaskLines(strings.NewReader(
		`{"prompt":"x","overlay":{"provider":"databricks","profile":"dev"}}` + "\n" +
			`{"prompt":"x","profile":"qa","overlay":{"provider":"databricks","profile":"dev"}}` + "\n" +
			`{"prompt":"x","overlay":{"profile":"dev"}}` + "\n"))
	if err != nil || len(lines) != 3 {
		t.Fatalf("readTaskLines = %d, %v", len(lines), err)
	}
	req, _, err := lineRequest(lines[0], "codex", base)
	if err != nil || req.Route != (route.Route{Provider: "databricks", Profile: "dev"}) || req.Provider != "dev" || req.Lead != "" {
		t.Errorf("request %+v, error %v", req, err)
	}
	for i, want := range []string{"--profile qa conflicts with overlay profile dev", "provider: required"} {
		_, _, err := lineRequest(lines[i+1], "codex", base)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("line %d: error %v, want %q", i+2, err, want)
		}
	}
}

// Replaying a failed batch leg retains its session and task and leaves siblings alone.
func TestBatchLegReplay(t *testing.T) {
	failed := failBatchLeg(t)
	code, out, stderr := runCLI(t, "ask", "--retry", failed.ID, "--format", "json")
	if code != 0 {
		t.Fatalf("replay: %d, %s, %s", code, out, stderr)
	}
	replayed := agent.RecentRecords(10)
	if len(replayed) != 3 {
		t.Fatalf("replay ran siblings: records = %d", len(replayed))
	}
	assertReplayedLeg(t, failed, replayed[0])
}

// failBatchLeg runs a two-leg batch and saves its newest leg as a failure.
func failBatchLeg(t *testing.T) agent.Record {
	t.Helper()
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, cliTask(t, "a", "first"), cliTask(t, "b", "second"))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--agent", "claude", "--fallback", "none")
	if code != 0 {
		t.Fatalf("batch: %d, %s, %s", code, out, stderr)
	}
	recs := agent.RecentRecords(10)
	if len(recs) != 2 {
		t.Fatalf("batch records = %d", len(recs))
	}
	failed := recs[0]
	failed.Status, failed.Error = "error", "leg failed"
	if failed.SessionID == "" {
		t.Fatal("batch leg did not retain its returned session")
	}
	if err := failed.Save(); err != nil {
		t.Fatal(err)
	}
	return failed
}

// assertReplayedLeg checks a replay kept the failed leg's task and session and ran on the route the leg answered on.
func assertReplayedLeg(t *testing.T, failed, replayed agent.Record) {
	t.Helper()
	got := replayed.Request
	if failed.RanRoute.IsZero() || replayed.RanRoute != failed.RanRoute {
		t.Fatalf("replay ran on %+v, want the recorded %+v", replayed.RanRoute, failed.RanRoute)
	}
	if got.SessionID != failed.SessionID || got.Prompt != failed.Request.Prompt ||
		got.Provider != cmp.Or(failed.Request.Provider, failed.RanRoute.Profile) || got.Lead != failed.Request.Lead {
		t.Fatalf("replay lost task/session/route: %+v", got)
	}
}

func TestBatchFlagErrors(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"jobs", []string{"--jobs", "2", "hi"}, "--jobs needs --batch"},
		{"fail-fast", []string{"--fail-fast", "hi"}, "--fail-fast needs --batch"},
		{"text format", []string{"--batch", "-", "--format", "text"}, "--batch always prints JSON"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cliEnv(t)
			code, out, stderr := runCLI(t, append([]string{"ask"}, c.argv...)...)
			if code != 2 || !strings.Contains(out+stderr, c.want) {
				t.Errorf("exit %d, output %q, want exit 2 naming %q", code, out+stderr, c.want)
			}
		})
	}
}

// --batch validates --provider like a single ask does: an unknown provider exits 2 before any task runs.
func TestBatchRejectsUnknownProvider(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	file := batchFile(t, taskLine(t, map[string]any{"id": "a", "agent": "claude", "prompt": "one", "workdir": t.TempDir(), "tools": true}))
	code, out, stderr := runCLI(t, "ask", "--batch", file, "--provider", "nonexistent")
	if code != 2 || !strings.Contains(stderr, "nonexistent") || strings.TrimSpace(out) != "" {
		t.Fatalf("exit %d, want 2 naming the provider with no envelopes\nstdout: %s\nstderr: %s", code, out, stderr)
	}
}

// A line's runner joins an inherited overlay document and keeps its other keys.
func TestLineRunnerKeepsInheritedKeys(t *testing.T) {
	lines, err := readTaskLines(strings.NewReader(`{"prompt":"x","overlay":{"runner":"pi"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for doc, want := range map[string]string{`{"effort":"high"}`: `"effort":"high"`, "": `"runner":"pi"`} {
		req, _, err := lineRequest(lines[0], "codex", agent.RunOptions{Values: proc.Values{route.OverlayVar: doc}})
		if got := req.Values[route.OverlayVar]; err != nil || !strings.Contains(got, want) || !strings.Contains(got, `"runner":"pi"`) {
			t.Fatalf("doc %q: values %q, err %v; want runner pi and %s", doc, got, err, want)
		}
	}
}

// A line overlay's model is typed, so a fallback never swaps it; auto and no model stay untyped.
func TestBatchLineOverlayModelIsTyped(t *testing.T) {
	for overlay, want := range map[string]bool{`{"model":"sonnet"}`: true, `{"model":"auto"}`: false, `{"effort":"high"}`: false} {
		lines, err := readTaskLines(strings.NewReader(`{"prompt":"x","overlay":` + overlay + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		req, _, err := lineRequest(lines[0], "codex", agent.RunOptions{})
		if err != nil || req.ModelTyped != want {
			t.Errorf("overlay %s: ModelTyped %v (%v), want %v", overlay, req.ModelTyped, err, want)
		}
		if overlay == `{"model":"auto"}` { // an auto line overlay replaces a typed base model
			req, _, err = lineRequest(lines[0], "codex", agent.RunOptions{Model: "sonnet", ModelTyped: true})
			if err != nil || req.ModelTyped {
				t.Errorf("auto over a typed base: ModelTyped %v (%v), want false", req.ModelTyped, err)
			}
		}
	}
}

func TestBatchLineOverlayKeepsRunner(t *testing.T) {
	base := agent.RunOptions{Values: proc.Values{route.OverlayVar: `{"provider":"databricks","profile":"dev","budgetUSD":2}`, "X": "1"}}
	lines, err := readTaskLines(strings.NewReader(`{"prompt":"x","overlay":{"runner":"cli"}}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := lineRequest(lines[0], "codex", base)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := agentconfig.LoadOverlayContext(proc.WithValues(context.Background(), req.Values), "", nil)
	if err != nil || ov.Runner != route.RunnerCLI || ov.Provider != "databricks" || ov.BudgetUSD == nil || req.Values["X"] != "1" {
		t.Errorf("overlay %+v, values %v, error %v", ov, req.Values, err)
	}
	if strings.Contains(base.Values[route.OverlayVar], "runner") {
		t.Errorf("the batch base values were mutated: %v", base.Values)
	}
}

func TestBatchLineKeepsFallbackPolicy(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: ""})
	base := agent.RunOptions{FallbackDefaulted: true, Values: proc.Values{route.OverlayVar: `{"maxChildren":2}`}}
	for _, policy := range []string{"none", "agents"} {
		line := decodeTaskLine([]byte(`{"prompt":"x","overlay":{"fallback":"`+policy+`"}}`), 1)
		task, err := buildTask(ctx, line, batchOpts{Agent: "claude", Fallback: "all"}, base, t.TempDir(),
			map[string]bool{}, func() *agent.Client { return &agent.Client{} })
		if err != nil {
			t.Fatal(err)
		}
		ov, err := agentconfig.LoadOverlayContext(proc.WithValues(ctx, task.Req.Values), "", nil)
		if err != nil || task.Req.Fallback != policy || task.Req.FallbackDefaulted || ov.Fallback != policy || ov.MaxChildren == nil {
			t.Fatalf("%s: request fallback %q defaulted %v, overlay %+v, err %v", policy,
				task.Req.Fallback, task.Req.FallbackDefaulted, ov, err)
		}
	}
	if base.Values[route.OverlayVar] != `{"maxChildren":2}` {
		t.Fatalf("base changed: %v", base.Values)
	}
}

func TestBatchOverlayRunKeys(t *testing.T) {
	line := decodeTaskLine([]byte(`{"prompt":"x","overlay":{"agent":"codex","tools":false,"timeout":"30s","noPersist":true,"tier":"deep"}}`), 1)
	task, err := parseTask(line, "claude", agent.RunOptions{Tools: true}, false, t.TempDir(), map[string]bool{})
	if err != nil || task.Agent != "codex" || task.Req.Tools || !task.Req.NoPersist ||
		task.Req.Timeout != 30*time.Second || !task.Req.TimeoutTyped || !strings.Contains(task.Req.Prompt, "Tier: deep") {
		t.Fatalf("task %+v, err %v", task, err)
	}
}

func TestBatchOverlayRouteSkipsPool(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: ""})
	line := decodeTaskLine([]byte(`{"prompt":"x","overlay":{"provider":"databricks","profile":"qa"}}`), 1)
	task, err := buildTask(ctx, line, batchOpts{Agent: "codex", Fallback: "none", Auto: "dev"}, agent.RunOptions{},
		t.TempDir(), map[string]bool{}, func() *agent.Client { return &agent.Client{} })
	if err != nil || task.arm != nil || task.Req.AutoArm || task.Req.Route.Profile != "qa" || task.Req.Provider != "qa" {
		t.Fatalf("task %+v, err %v, want qa without an auto arm", task, err)
	}
}

func TestBatchFallbackControlsModelPin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := proc.WithValues(t.Context(), proc.Values{route.OverlayVar: ""})
	line := decodeTaskLine([]byte(`{"prompt":"x","model":"claude-sonnet-5-5","overlay":{"fallback":"none"}}`), 1)
	task, err := buildTask(ctx, line, batchOpts{Agent: "claude", Fallback: "all", Swap: true, Auto: "dev"}, agent.RunOptions{},
		t.TempDir(), map[string]bool{}, func() *agent.Client { return &agent.Client{} })
	if err != nil || task.arm == nil || !task.arm.strict || task.arm.fallback != "none" {
		t.Fatalf("arm %+v, err %v, want the line's strict model and fallback none", task.arm, err)
	}
}

func TestBatchOverlayBriefUsesRunKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	line := decodeTaskLine([]byte(`{"prompt":"x","own":["a.go"],"report":"r.md","overlay":{"agent":"pi","tools":true,"codemode":"only"}}`), 1)
	req, name, err := lineRequest(line, "claude", agent.RunOptions{})
	if err != nil || name != "pi" || req.Codemode != "only" || !req.Tools || strings.Contains(req.Prompt, "goes through rtk") {
		t.Fatalf("name %s, request %+v, err %v, want the pi brief in code mode", name, req, err)
	}
}
