package ask

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/platform/hosttest"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/agentconfig"
	"github.com/edotau/claude-code/internal/routing/route"
)

func TestSpecContractDryRunAndBadKind(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	ov := `{"agent":"claude","fallback":"none"}`
	brief := `{"own":["a.go"],"contract":{"unit":"func x","kind":"function"},"prompt":"p"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--overlay", ov, "--brief", brief, "--dry-run"}) })
	if code != 0 || !regexp.MustCompile(`(?m)^terms:.*gate`).MatchString(out) {
		t.Fatalf("exit = %d, want 0 with a terms line holding gate\nout=%s\nstderr=%s", code, out, stderr)
	}
	bad := strings.Replace(brief, `"function"`, `"bogus"`, 1)
	code, _, stderr = hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--overlay", ov, "--brief", bad, "--dry-run"}) })
	if code != 2 || !strings.Contains(stderr, "function") || !strings.Contains(stderr, "test-repair") {
		t.Fatalf("exit = %d, want 2 naming the kinds\nstderr=%s", code, stderr)
	}
}

func TestSpecDryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	ov := `{"agent":"claude","fallback":"none"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--overlay", ov, "--brief", `{"prompt":"say ok"}`, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "agent:") || !strings.Contains(out, "argv:") || !strings.Contains(out, "say ok") {
		t.Fatalf("exit = %d, want 0 with the route block\nout=%s\nstderr=%s", code, out, stderr)
	}
	if n := len(agent.RecentRecords(2)); n != 0 {
		t.Fatalf("%d dispatch records saved, want none", n)
	}
}

func TestBriefDryRunTypedProfileOverlayReRun(t *testing.T) {
	dir := t.TempDir()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.example.com","auth":{"type":"none"}},"qa":{"preset":"databricks","host":"https://qa.example.com","auth":{"type":"none"}}}}}`)
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--overlay", `{"agent":"claude"}`, "--brief", `{"prompt":"x"}`, "--profile", "dev", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nout=%s\nstderr=%s", code, out, stderr)
	}
	argv := dryRunArgv(t, out)
	if !strings.Contains(argv, "--profile dev") {
		t.Fatalf("argv = %s, want typed profile dev", argv)
	}
	args := appendDryRun(strings.Fields(strings.TrimPrefix(argv, "claude-code agents ask ")))
	code, out, stderr = hosttest.CaptureOutput(t, func() int { return Cmd(args) })
	if code != 0 || !strings.Contains(out, "databricks·dev") {
		t.Fatalf("re-run exit = %d, want route dev\nout=%s\nstderr=%s", code, out, stderr)
	}
}

func dryRunArgv(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if argv, ok := strings.CutPrefix(line, "argv:"); ok {
			return strings.TrimSpace(argv)
		}
	}
	t.Fatalf("dry-run output lacks argv:\n%s", out)
	return ""
}

func appendDryRun(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			out := append([]string{}, args[:i]...)
			out = append(out, "--dry-run")
			return append(out, args[i:]...)
		}
	}
	return append([]string{"--dry-run"}, args...)
}

func TestSpecArgsParsedNotScanned(t *testing.T) {
	_, _, err := briefParts(`{"prompt":"x"}}`, nil)
	if err == nil {
		t.Fatal("trailing data accepted")
	}
	if got := dropBrief([]string{"--system-prompt", "--brief", "--brief", "{}"}); len(got) != 2 || got[1] != "--brief" {
		t.Fatalf("dropBrief = %q, want the system-prompt value kept", got)
	}
	args, err := withBrief(&askFlags{brief: `{"prompt":"--tools"}`}, []string{"--system-prompt", "--"}, nil)
	if err != nil || len(args) < 4 || args[len(args)-2] != "--" || args[len(args)-1] != "--tools" {
		t.Fatalf("withBrief = %q err %v, want a terminator before the prompt", args, err)
	}
}

func TestSpecFileRefusals(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "s.json")
	if err := os.WriteFile(real, []byte(`{"prompt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.json")
	if err := os.Symlink(real, link); err == nil {
		if _, _, err := briefParts(link, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink err = %v", err)
		}
	}
	big := filepath.Join(dir, "b.json")
	if err := os.WriteFile(big, []byte(`{"prompt":"`+strings.Repeat("a", 70<<10)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := briefParts(big, nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("big err = %v", err)
	}
}

func TestBriefNearestKey(t *testing.T) {
	prompt, _, err := parseBrief([]byte(`{"prompt":"hi"}`))
	if err != nil || prompt != "hi" {
		t.Fatalf("prompt = %q err %v, want hi", prompt, err)
	}
	if _, _, err := parseBrief([]byte(`{"ownn":["a.go"]}`)); err == nil || !strings.Contains(err.Error(), `"own"`) {
		t.Fatalf("unknown key err = %v, want one naming own", err)
	}
	if _, _, err := parseBrief([]byte(`{"fallback":"bogus"}`)); err == nil || !strings.Contains(err.Error(), `"fallback"`) {
		t.Fatalf("config key err = %v, want one refusing fallback", err)
	}
}

func TestBriefErrorsNameBrief(t *testing.T) {
	for _, doc := range []string{`null`, `{"prompt":null}`, `{"prompt":42}`, `{} {}`, `{"own":null}`,
		`{"ownn":[]}`, `{"action":"rm"}`, `{"contract":{"role":"fleet"}}`} {
		if _, _, err := briefParts("-", strings.NewReader(doc)); err == nil || !strings.Contains(err.Error(), "brief") || strings.Contains(err.Error(), "spec") {
			t.Errorf("%s: error %v, want brief validation error", doc, err)
		}
	}
	if _, _, err := parseBrief([]byte(`{"prompt":null}`)); err == nil {
		t.Fatal("null prompt became an absent task")
	}
}

func TestRetryKeepsTightestOverlayLimits(t *testing.T) {
	for _, current := range []int{1, 10} {
		fs, f := askFlagSet()
		r := &askRun{fs: fs, f: f, ov: agentconfig.Overlay{MaxChildren: new(current), MaxDepth: new(current),
			MaxTreeTokens: new(current), BudgetUSD: new(float64(current)), BudgetCredits: new(current * 30)}}
		rec := agent.Record{Overlay: &agent.StoredOverlay{MaxChildren: new(2), MaxDepth: new(2), MaxTreeTokens: new(2),
			BudgetUSD: new(2.0), BudgetCredits: new(60)}}
		if err := r.mergeRecordOverlay(rec); err != nil {
			t.Fatal(err)
		}
		want := min(current, 2)
		if *r.ov.MaxChildren != want || *r.ov.MaxDepth != want || *r.ov.MaxTreeTokens != want ||
			*r.ov.BudgetUSD != float64(want) || *r.ov.BudgetCredits != want*30 {
			t.Fatalf("current %d: merged limits %+v, want %d", current, r.ov, want)
		}
		if *rec.Overlay.MaxChildren != 2 || *rec.Overlay.BudgetUSD != 2 {
			t.Fatal("record limits mutated")
		}
	}
}

func TestBriefContractKey(t *testing.T) {
	_, task, err := parseBrief([]byte(`{"own":["a.go"],"contract":{"unit":"func x","kind":"function"}}`))
	if err != nil || task.Contract == nil || task.Contract.Unit != "func x" || !task.Set() {
		t.Fatalf("task = %+v err %v", task, err)
	}
	if _, _, err := parseBrief([]byte(`{"contract":{"unit":"x","knd":"f"}}`)); err == nil {
		t.Fatal("unknown contract field accepted")
	}
}

func TestBriefTaskKeys(t *testing.T) {
	_, task, err := parseBrief([]byte(`{"own":["a.go"],"report":"r.md","verify":"go vet ./x"}`))
	if err != nil || !slices.Equal(task.Own, []string{"a.go"}) || task.Report != "r.md" || task.Verify != "go vet ./x" {
		t.Fatalf("task = %+v err %v", task, err)
	}
	if _, _, err := parseBrief([]byte(`{"own":"a.go"}`)); err == nil || !strings.Contains(err.Error(), "own") {
		t.Fatalf("wrong type err = %v, want one naming own", err)
	}
	if _, _, err := parseBrief([]byte(`{"model":"m"}`)); err == nil {
		t.Fatal("config key accepted in a brief")
	}
}

func TestBriefActionKey(t *testing.T) {
	_, task, err := parseBrief([]byte(`{"action":"diff","own":["a.go"]}`))
	if err != nil || task.Action != "diff" || len(task.Own) != 1 {
		t.Fatalf("task = %+v err %v", task, err)
	}
	if _, _, err := parseBrief([]byte(`{"action":"rm"}`)); err == nil || !strings.Contains(err.Error(), "gate|diff") {
		t.Fatalf("unknown action err = %v, want one listing the actions", err)
	}
}

func TestBriefContractRoleParent(t *testing.T) {
	_, task, err := parseBrief([]byte(`{"contract":{"unit":"x","kind":"function","role":"fleet","parent":"d-1"}}`))
	if err != nil || task.Contract.Role != "fleet" || task.Contract.Parent != "d-1" {
		t.Fatalf("task = %+v err %v", task.Contract, err)
	}
	for _, bad := range []string{`{"role":"fleet"}`, `{"parent":"d-1"}`, `{"role":"boss","parent":"d-1"}`} {
		doc := `{"contract":{"unit":"x","kind":"function",` + bad[1:] + `}`
		if _, _, err := parseBrief([]byte(doc)); err == nil {
			t.Errorf("contract %s accepted", bad)
		}
	}
}

// Given keys, fieldsOf lists only those flags, each by its key with the flag beside it; --brief never appears.
func TestFromFlagSetKeys(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.Bool("no-tools", false, "")
	fs.String("model", "", "")
	fs.String("workdir", "", "")
	fs.String("brief", "", "")
	s := fieldsOf(fs, map[string]string{"no-tools": "noTools", "model": "model", "brief": "brief"}, nil)
	if got := fieldNames(s); !slices.Equal(got, []string{"model", "noTools"}) || s.Fields[0].FlagName() != "model" ||
		s.Fields[1].FlagName() != "no-tools" || s.Fields[1].Kind != "bool" {
		t.Fatalf("fields = %+v, want model and noTools (typed as --no-tools)", s.Fields)
	}
	if all := fieldsOf(fs, nil, nil); !slices.Equal(fieldNames(all), []string{"model", "no-tools", "workdir"}) {
		t.Fatalf("names = %v, want every flag but --brief", fieldNames(all))
	}
}

func fieldNames(s Schema) []string {
	out := make([]string, len(s.Fields))
	for i, f := range s.Fields {
		out[i] = f.Name
	}
	return out
}

func TestSpecTaskKeysRenderBrief(t *testing.T) {
	s := `{"own":["a.go"],"verify":"go vet ./x","report":"r.md","prompt":"do it"}`
	prompt, task, err := briefParts(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	brief, _, err := specBrief(prompt, nil, task)
	if err != nil || !strings.Contains(brief, `Owned: ["a.go"]`) || !strings.Contains(brief, "Verify: go vet ./x") {
		t.Fatalf("brief = %q err %v", brief, err)
	}
	for _, bad := range []string{`{"own":["a.go"],"verify":"rm -rf x","prompt":"p"}`, `{"verify":"go vet ./x","prompt":"p"}`} {
		p, task, err := briefParts(bad, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := specBrief(p, nil, task); err == nil {
			t.Errorf("specBrief accepted %s", bad)
		}
	}
}

func TestSpecTaskKeysNeedInlinePrompt(t *testing.T) {
	f := &askFlags{brief: `{"own":["a.go"],"prompt":"p"}`}
	if _, err := withBrief(f, nil, []string{"other"}); err == nil {
		t.Error("withBrief accepted a positional task beside own")
	}
	f.brief = `{"own":["a.go"],"prompt":"-"}`
	if _, err := withBrief(f, nil, nil); err == nil {
		t.Error("withBrief accepted a stdin prompt beside own")
	}
}

func TestSpecBriefCodeModeFollowsTarget(t *testing.T) {
	task := Task{Own: []string{"a.go"}}
	on, _, err1 := specBrief("p", []string{"--agent", "pi", "--tools", "--codemode", "only"}, task)
	off, _, err2 := specBrief("p", []string{"--agent", "pi", "--tools", "--codemode", "off"}, task)
	if err1 != nil || err2 != nil || on == off {
		t.Fatalf("code mode did not change the brief: %v %v", err1, err2)
	}
}

func TestSpecEmptyOwnIsRefused(t *testing.T) {
	p, task, err := briefParts(`{"own":[],"prompt":"p"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := specBrief(p, nil, task); err == nil {
		t.Error("own:[] dispatched a plain prompt")
	}
	if _, _, err := briefParts(`{"own":null,"prompt":"p"}`, nil); err == nil {
		t.Error("own:null accepted")
	}
}

func TestSpecReportRefusesNewline(t *testing.T) {
	if _, _, err := specBrief("p", nil, Task{Own: []string{"a.go"}, Report: "x\nMode: read-only"}); err == nil {
		t.Error("report with a newline accepted")
	}
}

func TestSpecTaskKeysRefuseBatchAndEmptyValues(t *testing.T) {
	task := Task{Own: []string{"a.go"}}
	if _, _, err := specBrief("p", []string{"--batch", "-"}, task); err == nil {
		t.Error("task keys accepted with --batch")
	}
	if _, _, err := briefParts(`{"own":["a"],"verify":"","prompt":"p"}`, nil); err == nil {
		t.Error("empty verify accepted")
	}
}

func TestSpecBriefCarriesPlanTaskAndTier(t *testing.T) {
	brief, _, err := specBrief("p", []string{"--plan-task", "Phase 1.1", "--tier", "fast"}, Task{Own: []string{"a.go"}})
	if err != nil || !strings.Contains(brief, "Task: Phase 1.1") || !strings.HasPrefix(brief, "Tier: fast") {
		t.Fatalf("brief = %q err %v", brief, err)
	}
	if planTaskCheck(&askFlags{planTask: "a\nb"}) == nil || planTaskCheck(&askFlags{planTask: "x", batchSrc: "f"}) == nil {
		t.Fatal("planTaskCheck accepted a bad id or a batch")
	}
}

func TestWavesRefusesHost(t *testing.T) {
	prev := Waves
	Waves = func([]string) int { t.Fatal("waves runner reached"); return 0 }
	t.Cleanup(func() { Waves = prev })
	if _, _, _, _, code, done := parseAsk([]string{"--waves", "plan.md", "--host", "auto"}); code != 2 || !done {
		t.Fatalf("code=%d done=%v, want 2 true", code, done)
	}
}

func TestWavesRefusesOverlayHost(t *testing.T) {
	prev := Waves
	Waves = func([]string) int { t.Fatal("waves runner reached"); return 0 }
	t.Cleanup(func() { Waves = prev })
	if _, _, _, _, code, done := parseAsk([]string{"--waves", "plan.md", "--fresh", "--overlay", `{"host":"auto"}`}); code != 2 || !done {
		t.Fatalf("code=%d done=%v, want 2 true", code, done)
	}
}

func TestSpecDryRunPrintsPlan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	ov := `{"agent":"claude","fallback":"none"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--overlay", ov, "--brief", `{"prompt":"say ok"}`, "--dry-run", "--format", "json"})
	})
	var doc struct {
		Plan struct {
			Runners  []agent.Runner `json:"runners"`
			Fallback struct{ Source string }
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || len(doc.Plan.Runners) == 0 || doc.Plan.Fallback.Source != "typed" {
		t.Fatalf("exit = %d, want plan.runners and plan.fallback.source (err %v)\nout=%s\nstderr=%s", code, err, out, stderr)
	}
}

func TestCompletionFlagsOfferSpecChoices(t *testing.T) {
	want := map[string][]string{}
	for _, f := range allFlagSchema().Fields {
		if len(f.Choices) > 0 {
			want[f.Name] = f.Choices
		}
	}
	if len(want) != 7 {
		t.Fatalf("choice flags = %d, want 7", len(want))
	}
	for _, f := range CompletionFlags() {
		var got []string
		if f.Values != nil {
			got = f.Values()
		}
		if w, ok := want[f.Name]; ok && !slices.Equal(got, w) {
			t.Errorf("--%s completes %v, want %v", f.Name, got, w)
		}
	}
}

// Given the same settings as flags and as --overlay + --brief, each dry run prints the same route and argv.
func TestSpecOverlayAndFlagLinesEqual(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	run := func(args ...string) string {
		code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd(append([]string{"--dry-run"}, args...)) })
		if code != 0 {
			t.Fatalf("args %q exit %d\nstderr=%s", args, code, stderr)
		}
		return out
	}
	flags := run("--agent", "claude", "--fallback", "none", "--model", "sonnet", "--timeout", "5m", "--tools=false", "--", "say ok")
	for name, out := range map[string]string{
		"overlay+brief": run("--overlay", `{"agent":"claude","fallback":"none","model":"sonnet","timeout":"5m","tools":false}`,
			"--brief", `{"prompt":"say ok"}`),
		"overlay": run("--overlay", `{"agent":"claude","fallback":"none","model":"sonnet","timeout":"5m","tools":false}`,
			"--", "say ok"),
	} {
		if out != flags {
			t.Errorf("%s != flags\n%s=%s\nflags=%s", name, name, out, flags)
		}
	}
}

// Every ask flag is an overlay key, a task-content flag, a document flag or a listed stay flag, and only one of them.
func TestFlagIsKey(t *testing.T) {
	stay := []string{"workdir", "session", "attach", "format", "retry", "recover", "live", "background", "carry", "save",
		"job", "batch", "jobs", "fail-fast", "loop", "loop-rounds", "loop-planner", "loop-budget-tokens", "loop-resume",
		"overlay", "dry-run", "stdin-framed", "waves", "wave", "status", "fresh", "gate", "profiles"}
	task := []string{"plan-task", "dirs", "files", "diff", "index", "max-files", "max-file-bytes", "rubric"}
	keys := map[string]bool{}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[agentconfig.Overlay]()) {
		keys[strings.Split(f.Tag.Get("json"), ",")[0]] = true
	}
	fs, _ := askFlagSet()
	fs.VisitAll(func(f *flag.Flag) {
		key, isKey := askKeys[f.Name]
		homes := btoi(isKey) + btoi(slices.Contains(stay, f.Name)) + btoi(slices.Contains(task, f.Name)) +
			btoi(f.Name == briefFlagName)
		if homes != 1 {
			t.Errorf("--%s has %d homes, want exactly one (overlay key, stay, task or document)", f.Name, homes)
		}
		if isKey && !keys[key] {
			t.Errorf("--%s spells overlay key %q, which agentconfig.Overlay lacks", f.Name, key)
		}
	})
	for _, n := range slices.Concat(stay, task, slices.Collect(maps.Keys(askKeys))) {
		if fs.Lookup(n) == nil {
			t.Errorf("listed flag --%s is not an ask flag", n)
		}
	}
	if len(stay) != 28 || len(askKeys) != 28 {
		t.Errorf("stay flags %d, config keys %d; want 28 and 28", len(stay), len(askKeys))
	}
}

// askSchema is the overlay key list: one field per config flag, named by its key, the flag kept beside it.
func TestAskSchemaIsOverlayKeys(t *testing.T) {
	s := askSchema()
	if len(s.Fields) != len(askKeys) {
		t.Fatalf("askSchema has %d fields, want %d", len(s.Fields), len(askKeys))
	}
	for _, f := range s.Fields {
		if askKeys[f.FlagName()] != f.Name {
			t.Errorf("field %q types --%s, want the key askKeys gives it", f.Name, f.FlagName())
		}
	}
	if f, ok := s.Lookup("noTools"); !ok || f.Flag != "no-tools" || f.Kind != "bool" {
		t.Errorf("noTools = %+v %v, want the --no-tools bool", f, ok)
	}
	if e, _ := s.Lookup("endpoint"); e.Kind != "string" || e.Flag != "direct" {
		t.Errorf("endpoint = %+v, want a string key typed by --direct", e)
	}
}

// A flag and its --overlay key holding the same value are one setting; different values are refused.
func TestFlagAndOverlayKeyDedupOrConflict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"--overlay", `{"model":"sonnet"}`, "--model", "opus"}, 2},
		{[]string{"--overlay", `{"model":"opus"}`, "--model", "opus"}, 0},
		{[]string{"--overlay", `{"effort":"low"}`, "--effort", "high"}, 2},
		{[]string{"--overlay", `{"model":"sonnet"}`}, 0},
		{[]string{"--overlay", `{"model":"sonnet"}`, "--model="}, 0}, // an empty flag is unset, not a value
		{[]string{"--overlay", `{"timeout":"5m"}`, "--timeout", "5m"}, 0},
		{[]string{"--overlay", `{"timeout":"5m"}`, "--timeout", "300s"}, 0},
		{[]string{"--overlay", `{"timeout":"5m"}`, "--timeout", "3m"}, 2},
	} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int {
			return Cmd(append(append([]string{"--dry-run", "--agent", "claude"}, c.args...), "--", "x"))
		})
		if (code == 0) != (c.code == 0) || (c.code != 0 && !strings.Contains(stderr, "conflicts with")) {
			t.Errorf("%v: exit %d stderr %q, want exit %d", c.args, code, stderr, c.code)
		}
	}
	fs, flags := askFlagSet()
	if err := fs.Parse([]string{"--host", "east"}); err != nil {
		t.Fatal(err)
	}
	typed := agentconfig.Overlay{Host: "west"}
	typeOverlay(fs, typed, nil, true)
	origins := agentconfig.Origins([]string{"host"}, typed, agentconfig.Overlay{}, flagsOverlay(fs))
	if flags.host != "east" || origins.Of("host") != agentconfig.OriginFlag {
		t.Fatalf("host %q, origin %v, want the typed flag's east over overlay west", flags.host, origins.Of("host"))
	}
}

// Given a typed --overlay, its model, fallback and timeout count as typed exactly like the flags; a typed flag wins.
func TestTypedOverlayKeysAreTyped(t *testing.T) {
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--tier", "deep"}); err != nil {
		t.Fatal(err)
	}
	ov := agentconfig.Overlay{Model: "sonnet", Fallback: "none"}
	ov.Tier, ov.Timeout, ov.Endpoint, ov.Tools = "fast", "5m", endpointDirect, new(false)
	set := typeOverlay(fs, ov, nil, true)
	markTyped(fs, f)
	type typedness struct {
		model, timeout, fallback, direct, tools bool
		tier                                    string
		d                                       time.Duration
	}
	got := typedness{f.modelTyped, f.timeoutTyped, helptext.Explicit(fs, "fallback"), f.direct, f.tools, f.tier, f.timeout}
	if want := (typedness{true, true, true, true, false, "deep", 5 * time.Minute}); got != want {
		t.Errorf("typed = %+v, want %+v (the typed --tier deep wins)", got, want)
	}
	f.model = "record"
	typeOverlay(fs, ov, set, true)
	back := flagsOverlay(fs)
	if f.model != "sonnet" || back.Model != "sonnet" || back.Tier != "deep" || back.Timeout != "5m0s" {
		t.Errorf("retype left model %q; flagsOverlay = %+v, want the typed flags as keys", f.model, back)
	}
}

// The derive hop: a typed --overlay is a HopAppend onto the inherited one, so its fence and caps hold and bind it.
func TestTypedOverlayKeepsInheritedFence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	inherited := `{"provider":"anthropic","profile":"anthropic","maxChildren":2,"fence":{"profiles":["anthropic"]}}`
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: inherited})
	merged, typed, _, err := LoadOverlays(ctx, `{"model":"sonnet"}`, "")
	if err != nil || merged.Model != "sonnet" || merged.Fence == nil || merged.MaxChildren == nil || *merged.MaxChildren != 2 ||
		typed.Fence != nil || typed.Provider != "" {
		t.Fatalf("merged %+v typed %+v err %v, want the inherited fence and cap kept, the typed doc alone apart", merged, typed, err)
	}
	if _, _, _, err := LoadOverlays(ctx, `{"provider":"anthropic","profile":"ultra"}`, ""); err == nil || !strings.Contains(err.Error(), "fence") {
		t.Fatalf("a typed profile outside the inherited fence: err %v, want a fence refusal", err)
	}
}

// Given --direct or endpoint direct under a confinement, the unit reaches ctx whole; another provider's is left alone.
func TestWithEndpointReachesCtx(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"provider":"anthropic","profile":"anthropic"}`})
	got, err := withEndpoint(ctx, route.Route{Endpoint: endpointDirect, Provider: "anthropic", Profile: "anthropic"})
	if err != nil || !strings.Contains(proc.Value(got, route.OverlayVar), `"endpoint":"direct"`) {
		t.Fatalf("ctx doc %q err %v, want the endpoint on it", proc.Value(got, route.OverlayVar), err)
	}
	got, err = withEndpoint(ctx, route.Route{Endpoint: endpointDirect, Provider: "openai", Profile: "openai"})
	if err != nil || strings.Contains(proc.Value(got, route.OverlayVar), "endpoint") {
		t.Fatalf("another provider's endpoint reached ctx: %q err %v", proc.Value(got, route.OverlayVar), err)
	}
}

// Typed route flags beside a config-only --overlay replace the stored unit, as they do with no --overlay.
func TestTypedConfigOverlayLetsFlagRouteReplaceStoredUnit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--provider", "anthropic", "--profile", "anthropic", "--overlay", `{"model":"sonnet"}`}); err != nil {
		t.Fatal(err)
	}
	var stored agentconfig.Overlay
	stored.Provider, stored.Profile, stored.Model = "databricks", "dev", "sonnet"
	r := &askRun{ctx: context.Background(), f: f, fs: fs}
	if err := takeConfig(r, stored, agentconfig.Overlay{Model: "sonnet"}); err != nil || r.ov.Provider != "" || !r.ovTyped {
		t.Fatalf("err %v provider %q typed %v, want the stored unit dropped and the overlay typed", err, r.ov.Provider, r.ovTyped)
	}
}

// A typed --overlay beside --brief sets tier on the rendered brief.
func TestBriefUsesTypedOverlayTier(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	args, err := withBrief(&askFlags{overlay: `{"tier":"deep"}`, brief: `{"own":["a.go"],"prompt":"p"}`},
		[]string{"--overlay", `{"tier":"deep"}`}, nil)
	if err != nil || len(args) == 0 || !strings.HasPrefix(args[len(args)-1], "Tier: deep") {
		t.Fatalf("args %q err %v, want a brief headed with Tier: deep", args, err)
	}
}

// --provider/--profile become the run overlay's unit (owner, 2026-11-26) and the flags clear; --as stays a flag.
func TestFlagRouteIsTheOverlayUnit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	for args, folded := range map[string]bool{
		"--provider|anthropic|--profile|anthropic":                    true,
		"--provider|anthropic|--profile|anthropic|--host|box":         false,
		"--provider|anthropic|--profile|anthropic|--retry|20261126T0": false,
	} {
		fs, f := askFlagSet()
		if err := fs.Parse(strings.Split(args, "|")); err != nil {
			t.Fatal(err)
		}
		r := &askRun{ctx: context.Background(), f: f, fs: fs}
		if code, done := r.mergeFlags(); done {
			t.Fatalf("%s: mergeFlags exit %d", args, code)
		}
		got := r.ov.Provider == "anthropic" && r.ov.Profile == "anthropic" && f.profile == "" && f.provider == ""
		if got != folded {
			t.Errorf("%s: unit %+v flags %q/%q, want folded %v", args, r.ov.Route(), f.provider, f.profile, folded)
		}
	}
}

// A folded route is typed again for a carried child's inner line, and still counts for the code-mode check.
func TestFoldedRouteRetypedAndSeenByCodemode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	fs, f := askFlagSet()
	args := []string{"--agent", "claude", "--provider", "anthropic", "--profile", "anthropic", "--codemode", "on"}
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	r := &askRun{ctx: context.Background(), f: f, fs: fs, args: args}
	if code, done := r.mergeFlags(); done {
		t.Fatalf("mergeFlags exit %d", code)
	}
	f.tools = true
	if err := resolveCodemode(f, r.ov.Route()); err != nil || f.codemode != "on" {
		t.Errorf("anthropic route folded: code mode %q, %v, want on", f.codemode, err)
	}
	r.retypeRoute()
	if f.profile != "anthropic" || f.provider != "anthropic" || !helptext.Explicit(fs, "profile") {
		t.Errorf("route flags %q/%q after retype, want anthropic/anthropic typed", f.provider, f.profile)
	}
}

// An inherited overlay's run keys (tools, noPersist, timeout) apply when no flag is typed.
func TestInheritedOverlayRunKeysApply(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"tools":false,"noPersist":true,"timeout":"30s"}`})
	fs, f := askFlagSet()
	r := &askRun{ctx: ctx, f: f, fs: fs}
	if code, done := r.config(); done {
		t.Fatalf("config exit %d", code)
	}
	if f.tools || !f.noPersist || f.timeout != 30*time.Second || f.timeoutTyped {
		t.Errorf("tools %v noPersist %v timeout %v typed %v, want false true 30s false", f.tools, f.noPersist, f.timeout, f.timeoutTyped)
	}
}

// A --retry record's tools, noPersist and timeout never replace the inherited fence's keys.
func TestRetryKeepsInheritedRunKeys(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"tools":false,"noPersist":true,"timeout":"30s"}`})
	fs, f := askFlagSet()
	r := &askRun{ctx: ctx, f: f, fs: fs}
	if code, done := r.config(); done {
		t.Fatalf("config exit %d", code)
	}
	f.tools, f.noPersist, f.timeout = true, false, 5*time.Minute // what Apply(record) wrote
	r.retypeOverlay()
	if f.tools || !f.noPersist || f.timeout != 30*time.Second || f.timeoutTyped {
		t.Errorf("tools %v noPersist %v timeout %v typed %v after retry, want false true 30s false", f.tools, f.noPersist, f.timeout, f.timeoutTyped)
	}
}

// A typed --timeout 0 (outside the key's domain) also drops the overlay's own timeout.
func TestTimeoutFlagOutsideDomainClearsOverlayKey(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	for _, d := range []string{"0"} {
		fs, f := askFlagSet()
		if err := fs.Parse([]string{"--timeout", d}); err != nil {
			t.Fatal(err)
		}
		f.cli = typedFlags(fs)
		r := &askRun{ctx: context.Background(), f: f, fs: fs}
		r.ov.Timeout = "30s"
		if code, done := r.mergeFlags(); done || r.ov.Timeout != "" {
			t.Errorf("--timeout %s: exit %d overlay timeout %q, want empty", d, code, r.ov.Timeout)
		}
	}
}

// An explicit --direct=false clears the overlay's endpoint direct; --timeout takes 48h (no maximum).
func TestDirectFalseAndTimeoutFlagDomain(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--direct=false", "--timeout", "48h"}); err != nil {
		t.Fatal(err)
	}
	f.cli = typedFlags(fs)
	r := &askRun{ctx: context.Background(), f: f, fs: fs}
	r.ov.Provider, r.ov.Profile, r.ov.Endpoint = "anthropic", "anthropic", endpointDirect
	if code, done := r.mergeFlags(); done {
		t.Fatalf("mergeFlags exit %d", code)
	}
	if r.ov.Endpoint != "" || f.timeout != 48*time.Hour {
		t.Errorf("endpoint %q timeout %v, want empty and 48h", r.ov.Endpoint, f.timeout)
	}
	fs, f = askFlagSet()
	if err := fs.Parse([]string{"--timeout", "0"}); err != nil {
		t.Fatal(err)
	}
	f.cli = typedFlags(fs)
	r = &askRun{ctx: context.Background(), f: f, fs: fs}
	if code, done := r.mergeFlags(); done {
		t.Fatalf("--timeout 0: mergeFlags exit %d", code)
	}
}

// A --provider that does not hold the typed --profile is refused, never replaced by the profile's own provider.
func TestFlagRouteMismatchRefused(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--provider", "openai", "--profile", "anthropic"}); err != nil {
		t.Fatal(err)
	}
	r := &askRun{ctx: context.Background(), f: f, fs: fs}
	if _, err := r.flagUnit(agentconfig.Overlay{}); err == nil || f.provider != "openai" {
		t.Fatalf("err %v provider %q, want a refusal that leaves the flags", err, f.provider)
	}
}

// A task document cannot pair with --overlay -: the brief names its agent before stdin could be read.
func TestBriefRefusesStdinOverlay(t *testing.T) {
	if _, err := withBrief(&askFlags{overlay: "-", brief: `{"own":["a.go"],"prompt":"p"}`}, nil, nil); err == nil {
		t.Fatal("--overlay - with own accepted")
	}
}

// A stored half of the tools pair, which ask never applied, does not refuse a typed --tools.
func TestStoredNoToolsBesideTypedTools(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	t.Setenv(route.OverlayVar, `{"noTools":true}`)
	for _, args := range [][]string{{"--tools"}, {"--overlay", `{"tools":true}`}} {
		code, _, stderr := hosttest.CaptureOutput(t, func() int {
			return Cmd(append(append([]string{"--dry-run", "--agent", "claude"}, args...), "--", "x"))
		})
		if code != 0 {
			t.Errorf("%q under a stored noTools: exit %d stderr %s, want 0", args, code, stderr)
		}
	}
}

// --brief takes only the task keys and the prompt; a config key is refused by name, and --spec is removed.
func TestBriefTaskKeysOnly(t *testing.T) {
	prompt, task, err := briefParts(`{"own":["a.go"],"prompt":"do x"}`, nil)
	if err != nil || prompt != "do x" || !slices.Equal(task.Own, []string{"a.go"}) {
		t.Fatalf("prompt %q task %+v err %v", prompt, task, err)
	}
	if _, _, err := briefParts(`{"model":"opus","prompt":"p"}`, nil); err == nil || !strings.Contains(err.Error(), `"model"`) {
		t.Fatalf("config key in a brief: err %v, want one naming model", err)
	}
	if _, _, _, _, code, done := parseAsk([]string{"--spec", `{"prompt":"p"}`}); !done || code != 2 {
		t.Fatalf("--spec accepted: code=%d done=%v, want 2 true", code, done)
	}
	if got := dropBrief([]string{"--brief", "{}", "--model", "m"}); !slices.Equal(got, []string{"--model", "m"}) {
		t.Fatalf("dropBrief = %q, want the brief dropped", got)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A route typed as a flag fences the last-resort row like a typed overlay unit: the request says so.
func TestFoldedFlagRouteIsFenceTyped(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	for _, args := range [][]string{{"--provider", "anthropic", "--profile", "anthropic"}, {"--tier", "fast"}} {
		fs, f := askFlagSet()
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		f.cli = typedFlags(fs)
		r := &askRun{ctx: context.Background(), f: f, fs: fs}
		if code, done := r.mergeFlags(); done {
			t.Fatalf("%v: mergeFlags exit %d", args, code)
		}
		req, err := r.batchBase()
		if want := args[0] == "--provider"; err != nil || req.FenceTyped != want {
			t.Errorf("%v: FenceTyped %v err %v, want %v", args, req.FenceTyped, err, want)
		}
	}
}

// The tools default never replaces an inherited overlay timeout; without one a fresh tools run still gets it.
func TestToolsDefaultKeepsInheritedTimeout(t *testing.T) {
	fs, _ := askFlagSet()
	inherited := agentconfig.Overlay{Timeout: "30s"}
	if got := askTimeout(fs, inherited, 30*time.Second, "", "", true); got != 30*time.Second {
		t.Errorf("inherited 30s became %v under the tools default", got)
	}
	if got := askTimeout(fs, agentconfig.Overlay{}, 0, "", "", true); got != agent.DefaultToolsTimeout {
		t.Errorf("no overlay timeout gave %v, want the tools default", got)
	}
}

// A --retry whose inherited deadline replaced the record's keeps no TimeoutTyped from the record.
func TestRetryInheritedTimeoutDropsRecordTyped(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"timeout":"30s"}`})
	fs, f := askFlagSet()
	r := &askRun{ctx: ctx, f: f, fs: fs}
	if code, done := r.config(); done {
		t.Fatalf("config exit %d", code)
	}
	f.timeout, f.timeoutTyped = 5*time.Minute, true // what Apply(record) wrote
	r.retypeOverlay()
	if f.timeout != 30*time.Second || f.timeoutTyped {
		t.Errorf("timeout %v typed %v after retry, want 30s untyped", f.timeout, f.timeoutTyped)
	}
}

// A stored COWORKER_OVERLAY (a batch line's base document, a review's env) loads as stored, not as a typed document.
func TestStoredOverlayNotLoadedAsTyped(t *testing.T) {
	vaulttest.Use(t)
	doc := `{"runner":"pi","origins":{"unit":"flag"}}` // a typed load refuses the unknown key
	t.Setenv(route.OverlayVar, doc)
	if rc, done := reviewLane("codex", ReviewOptions{}); done || rc != 0 {
		t.Errorf("reviewLane on a stored env doc: rc %d done %v, want it loaded as stored", rc, done)
	}
	req := agent.RunOptions{Values: proc.Values{route.OverlayVar: doc}}
	fs, f := askFlagSet()
	if err := applyLineOverlay(&req, fs, f, route.Route{}, agentconfig.Overlay{Runner: "cli"}); err != nil {
		t.Errorf("applyLineOverlay over a stored base document: %v", err)
	}
}

// A typed --timeout 0 that empties the overlay still replaces the old document the children would read.
func TestClearedTimeoutReachesChildDoc(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "")
	old := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: `{"timeout":"30s"}`})
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--timeout", "0"}); err != nil {
		t.Fatal(err)
	}
	f.cli = typedFlags(fs)
	r := &askRun{ctx: old, f: f, fs: fs}
	r.ov.Timeout = "30s"
	if code, done := r.mergeFlags(); done {
		t.Fatalf("mergeFlags exit %d", code)
	}
	ctx, err := carryOverlay(old, carried(r.ov))
	if err != nil {
		t.Fatal(err)
	}
	got, err := agentconfig.LoadOverlayContext(ctx, "", nil)
	if err != nil || got.Timeout != "" {
		t.Errorf("child document timeout %q err %v, want none", got.Timeout, err)
	}
}

// A value projected from a document keeps that document's origin; only a flag typed on this line mints Flag.
func TestProjectedValueKeepsItsOrigin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(route.OverlayVar, "")
	unit := agentconfig.Overlay{Provider: "anthropic", Profile: "anthropic"}
	direct := agentconfig.Overlay{Provider: "anthropic", Profile: "anthropic", Endpoint: endpointDirect}
	for _, tc := range []struct {
		name    string
		args    []string
		typed   agentconfig.Overlay
		wantKey string
		want    agentconfig.Origin
	}{
		{"overlay model projected", []string{"--overlay", `{"model":"sonnet"}`}, agentconfig.Overlay{Model: "sonnet"}, "model", agentconfig.OriginOverlay},
		{"model typed here", []string{"--model", "sonnet"}, agentconfig.Overlay{}, "model", agentconfig.OriginFlag},
		{"overlay unit projected", []string{"--overlay", "{}"}, unit, "unit", agentconfig.OriginOverlay},
		{"profile typed here", []string{"--provider", "anthropic", "--profile", "anthropic"}, agentconfig.Overlay{}, "unit", agentconfig.OriginFlag},
		{"a flag agreeing with the overlay unit", []string{"--overlay", "{}", "--provider", "anthropic", "--profile", "anthropic"}, unit, "unit", agentconfig.OriginOverlay},
		{"endpoint direct projected", []string{"--overlay", "{}"}, direct, "unit", agentconfig.OriginOverlay},
		{"--direct typed here", []string{"--overlay", "{}", "--direct"}, unit, "unit", agentconfig.OriginFlag},
	} {
		fs, f := askFlagSet()
		if err := fs.Parse(tc.args); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		f.cli = typedFlags(fs) // before takeConfig projects the overlay's keys onto the flag set
		r := &askRun{ctx: context.Background(), f: f, fs: fs}
		if err := takeConfig(r, tc.typed, tc.typed); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if code, done := r.mergeFlags(); done {
			t.Fatalf("%s: mergeFlags exit %d", tc.name, code)
		}
		if got := r.origins().Of(tc.wantKey); got != tc.want {
			t.Errorf("%s: %s origin %v, want %v", tc.name, tc.wantKey, got, tc.want)
		}
	}
}

// --brief beside --overlay preserves Overlay origin for overlay keys and Flag origin for typed flags.
func TestBriefBesideOverlayOrigins(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv(route.OverlayVar, "")
	fs, f, _, _, code, done := parseAsk([]string{"--agent", "opencode", "--native",
		"--overlay", `{"provider":"anthropic","profile":"anthropic","model":"sonnet"}`,
		"--brief", `{"prompt":"go"}`})
	if done || code != 0 {
		t.Fatalf("parseAsk: code=%d done=%v", code, done)
	}
	r := &askRun{ctx: context.Background(), f: f, fs: fs}
	_, cDone := r.config()
	_, mDone := r.mergeFlags()
	want := agentconfig.Overlay{Schema: r.ov.Schema, Provider: "anthropic", Profile: "anthropic", Model: "sonnet"}
	want.Agent, want.Native = "opencode", new(true)
	if cDone || mDone || !reflect.DeepEqual(r.ov, want) {
		t.Fatalf("overlay %+v, want %+v", r.ov, want)
	}
	if o := r.origins(); o.Of("unit") != agentconfig.OriginOverlay || o.Of("model") != agentconfig.OriginOverlay ||
		o.Of("native") != agentconfig.OriginFlag {
		t.Errorf("origins %v, want unit/model OriginOverlay and native OriginFlag", o)
	}
}

// An inherited file-sourced fence reaches --native as its resolved document, not as a path the router cannot read.
func TestInheritedFenceIsTheResolvedDocument(t *testing.T) {
	vaulttest.Use(t)
	file := filepath.Join(t.TempDir(), "parent.json")
	if err := os.WriteFile(file, []byte(`{"provider":"openai","profile":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(route.OverlayVar, "")
	ctx := proc.WithValues(context.Background(), proc.Values{route.OverlayVar: file})
	fs, f := askFlagSet()
	r := &askRun{ctx: ctx, f: f, fs: fs}
	if code, done := r.config(); done {
		t.Fatalf("config exit %d", code)
	}
	if err := checkNative(r.ctx, true); err != nil {
		t.Fatalf("the raw path was read as a fence (%v): the test needs it unreadable", err)
	}
	fence := withValues(r.ctx, proc.Values{route.OverlayVar: r.inherited.Doc()})
	if err := checkNative(fence, true); err == nil || !strings.Contains(err.Error(), "outside overlay openai") {
		t.Errorf("--native against the resolved inherited fence: %v, want a refusal", err)
	}
}

// relaunchPhases runs a --job relaunch's real phases up to the route check, as the detached child would.
func relaunchPhases(t *testing.T, carried string, req agent.RunOptions) (code int, stderr string) {
	t.Helper()
	armChildEnvFixture(t)
	t.Setenv(route.OverlayVar, "")
	req.Prompt, req.Native = "go", true
	rec := agent.NewRecord("opencode", req, time.Now())
	rec.Status = "running"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--job", rec.ID, "--native", "--fallback", "none"}); err != nil {
		t.Fatal(err)
	}
	r := &askRun{ctx: proc.WithValues(t.Context(), proc.Values{route.OverlayVar: carried}), f: f, fs: fs, ag: "opencode"}
	phases := []func() (int, bool){r.config, r.applyRecord, r.checkArm}
	code, _, stderr = hosttest.CaptureOutput(t, func() int {
		for _, phase := range phases {
			if c, done := phase(); done {
				return c
			}
		}
		return 0
	})
	return code, stderr
}

// A --job relaunch recomputes its origins from the record: --native meets the recorded fence, never its carried route.
func TestRelaunchOriginsFromRecord(t *testing.T) {
	dev := `{"provider":"databricks","profile":"dev"}`
	qa := `{"provider":"databricks","profile":"qa"}`
	for _, tc := range []struct {
		name, carried string
		req           agent.RunOptions
		refused       string // "" = --native passes the route check
	}{
		{"a flag unit meets the recorded fence", qa, agent.RunOptions{TypedFlags: []string{"profile"}, InheritedOverlay: dev}, "outside overlay databricks·dev"},
		{"a flag unit with no fence", dev, agent.RunOptions{TypedFlags: []string{"profile"}}, ""},
		{"an inherited unit meets its carried route", dev, agent.RunOptions{}, "outside overlay databricks·dev"},
	} {
		code, stderr := relaunchPhases(t, tc.carried, tc.req)
		refused := strings.Contains(stderr, "--native: outside overlay")
		if want := tc.refused != ""; refused != want || want && (code != 2 || !strings.Contains(stderr, tc.refused)) {
			t.Errorf("%s: exit %d stderr %q, want a native refusal naming %q (none when empty)", tc.name, code, stderr, tc.refused)
		}
	}
}

// The pool options and planner ask hold the overlay's timeout; a tools line keeps it, else gets the default.
func TestBatchToolsDefaultKeepsOverlayTimeout(t *testing.T) {
	profileFixture(t)
	fs, f := askFlagSet()
	for _, tc := range []struct {
		name string
		ov   agentconfig.Overlay
		want time.Duration
	}{{"an overlay timeout", agentconfig.Overlay{Timeout: "30s"}, 30 * time.Second}, {"none held", agentconfig.Overlay{}, agent.DefaultToolsTimeout}} {
		r := &askRun{ctx: t.Context(), f: f, fs: fs, ov: tc.ov, ref: "opencode"}
		o := r.batchOpts()
		o.Agent, o.Fallback = "opencode", "all"
		line := parsedLine{N: 1, Task: batchLine{ID: "a", Prompt: "go"}}
		bt, err := buildTask(t.Context(), line, o, agent.RunOptions{Tools: true, Timeout: 30 * time.Second}, t.TempDir(), map[string]bool{}, newClient)
		if err != nil || bt.Req.Timeout != tc.want {
			t.Errorf("%s: line timeout %v (err %v), want %v", tc.name, bt.Req.Timeout, err, tc.want)
		}
		var plannerArgs []string
		orig := loopAsk
		loopAsk = func(_ context.Context, args []string, out io.Writer) int {
			plannerArgs = append([]string{}, args...)
			_, _ = out.Write([]byte(`{"status":"ok","structured":{"done":true,"summary":"s","parts":[]}}`))
			return 0
		}
		f.timeout = 30 * time.Second
		if tc.ov.Timeout == "" {
			f.timeout = 0
		}
		_, _, perr := r.loopPlanFn()(t.Context(), "claude", "plan")
		loopAsk = orig
		if perr != nil || !strings.Contains(strings.Join(plannerArgs, " "), "--timeout "+tc.want.String()) {
			t.Errorf("%s: planner args %v (err %v), want --timeout %v", tc.name, plannerArgs, perr, tc.want)
		}
	}
}
