package memory_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// permissions is the conversion ask.memoryRunOptions applies to a Scope: plan mode, bank writes, no redirects.
func permissions(s memory.Scope) agent.Permissions {
	p := agent.Permissions{Mode: "plan", Deny: []agent.Rule{{Tool: "Bash", Arg: agent.RedirectFileArg}}}
	for _, tool := range []string{"Edit", "Write"} {
		for _, d := range s.NoWrite {
			p.Deny = append(p.Deny, agent.Rule{Tool: tool, Arg: d})
		}
		for _, w := range s.Write {
			p.Allow = append(p.Allow, agent.Rule{Tool: tool, Arg: w})
		}
	}
	for _, d := range s.CdDirs {
		p.Allow = append(p.Allow, agent.CdRule(d))
	}
	for _, b := range s.Bash {
		p.Allow = append(p.Allow, agent.Rule{Tool: "Bash", Arg: b})
	}
	for _, b := range s.NoBash {
		p.Deny = append(p.Deny, agent.Rule{Tool: "Bash", Arg: b})
	}
	return p
}

func TestJobScopeBankAndDreamWrites(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	store := memory.StoreDir()
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, ".layout"), []byte("flat 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", work, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	home := memory.StoreDir()
	claudeMD := filepath.Join(paths.ConfigDir(), "CLAUDE.md")
	other := filepath.Join(store, "other-repo", "progress.md")
	tests := []struct {
		kind, tool, arg string
		want            agent.Decision
	}{
		{memory.JobUpdate, "Write", memory.Path("progress.md"), agent.Allow},
		{memory.JobUpdate, "Edit", filepath.Join(home, "conventions.md"), agent.Allow},
		{memory.JobUpdate, "Write", other, agent.Deny},
		{memory.JobUpdate, "Write", claudeMD, agent.Deny},
		{memory.JobUpdate, "Write", filepath.Join(store, ".git", "config"), agent.Deny},
		{memory.JobUpdate, "Bash", "claude-code memory promote x", agent.Deny},
		{memory.JobUpdate, "Bash", "cd " + store, agent.Allow},
		{memory.JobUpdate, "Bash", "cd " + t.TempDir(), agent.Deny},
		{memory.JobDream, "Write", other, agent.Allow},
		{memory.JobDream, "Edit", claudeMD, agent.Allow},
		{memory.JobDream, "Write", filepath.Join(store, ".git", "config"), agent.Deny},
		{memory.JobDream, "Write", filepath.Join(t.TempDir(), "x.md"), agent.Deny},
		{memory.JobDream, "Bash", "claude-code memory promote --to claude-md x", agent.Allow},
		{memory.JobDream, "Bash", "claude-code memory review --timeline --n 3", agent.Allow},
		{memory.JobDream, "Bash", "claude-code memory review --brief", agent.Allow},
		{memory.JobDream, "Bash", "rm -rf " + store, agent.Deny},
		{memory.JobDream, "Bash", "echo x > " + other, agent.Deny},
	}
	for _, tc := range tests {
		p := permissions(memory.JobScope(tc.kind, work))
		if got := p.Decide(tc.tool, tc.arg); got != tc.want {
			t.Errorf("%s: Decide(%s, %q) = %v, want %v", tc.kind, tc.tool, strings.TrimPrefix(tc.arg, store), got, tc.want)
		}
	}
}

func TestJobScopeUnknownHoldsNoTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, kind := range []string{"digest", "nonsense", ""} {
		s := memory.JobScope(kind, t.TempDir())
		if s.Tools || len(s.Write)+len(s.Bash)+len(s.CdDirs) != 0 {
			t.Errorf("JobScope(%q) = %+v, want no tools and no grants", kind, s)
		}
	}
	if s := memory.JobScope(memory.JobUpdate, t.TempDir()); !s.Tools {
		t.Errorf("update scope reports no tools: %+v", s)
	}
}

// planRow is one plan call: the gate's verdict and, where the gate allows a hostile --plan, the verb's own refusal.
type planRow struct {
	kind, tool, arg string
	want            agent.Decision
	plan            string // the --plan the verb would act on; "" when the gate alone decides
}

func planRows(home, plan string) []planRow {
	task := "claude-code plan task 'Phase 1.1' "
	done := task + "--done --sha abc1234 --plan "
	escape := home + "/../../../etc/2026-11-18-x.md"
	return []planRow{
		{memory.JobDream, "Bash", done + plan, agent.Allow, plan},
		{memory.JobDream, "Bash", task + "--strike 'gone — evidence: decision:ab12cd34' --plan " + plan, agent.Allow, plan},
		{memory.JobDream, "Bash", task + "--reopen --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", task + "-reopen --done --sha abc1234 --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", "FOO=bar " + task + "--done --sha abc1234 --reopen --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", "bash -c \"" + task + "--reopen --plan " + plan + "\"", agent.Deny, ""},
		{memory.JobDream, "Bash", "claude-code plan tick --audit", agent.Deny, ""},
		{memory.JobDream, "Bash", "claude-code plan tick --range a..b --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", "claude-code plan claim x --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", task + "--done --plan " + plan, agent.Deny, ""},
		{memory.JobDream, "Bash", done + "2026-11-18-a.md", agent.Deny, ""},
		{memory.JobDream, "Bash", done + home + "-evil/2026-11-18-a.md", agent.Deny, ""},
		{memory.JobDream, "Bash", done + plan + " > /tmp/x", agent.Deny, ""},
		{memory.JobDream, "Bash", done + plan + "; rm -rf " + home, agent.Deny, ""},
		{memory.JobDream, "Bash", done + escape, agent.Allow, escape},                                                // `*` spans `..`: the verb refuses
		{memory.JobDream, "Bash", done + plan + " --plan /etc/2026-11-18-x.md", agent.Allow, "/etc/2026-11-18-x.md"}, // the last --plan wins
		{memory.JobDream, "Write", plan, agent.Deny, ""},
		{memory.JobUpdate, "Bash", done + plan, agent.Deny, ""},
		{memory.JobUpdate, "Edit", plan, agent.Deny, ""},
	}
}

// Dream may tick or strike a plan task only through the verb, in this checkout's plans home; update may not at all.
func TestJobScopePlanRules(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	work := filepath.Join(t.TempDir(), "proj")
	if out, err := exec.Command("git", "init", "-q", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	home := plans.Home(work)
	plan := filepath.Join(home, "2026-11-18-a.md")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, []byte("- [ ] Phase 1.1 x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range planRows(home, plan) {
		if got := permissions(memory.JobScope(r.kind, work)).Decide(r.tool, r.arg); got != r.want {
			t.Errorf("%s: Decide(%s, %q) = %v, want %v", r.kind, r.tool, r.arg, got, r.want)
		}
		if _, err := plans.ResolvePlan(work, r.plan); r.plan != "" && (err == nil) != (r.plan == plan) {
			t.Errorf("ResolvePlan(%q) err = %v: only the home's dated plan resolves", r.plan, err)
		}
	}
	wide := memory.JobScope(memory.JobDream, work) // the denies survive a later, broader grant
	wide.Bash, wide.Write = append(wide.Bash, "claude-code plan *"), append(wide.Write, home+"/**")
	for _, call := range [][2]string{{"Bash", "claude-code plan task x --reopen --plan " + plan}, {"Bash", "claude-code plan tick --audit"}, {"Write", plan}} {
		if got := permissions(wide).Decide(call[0], call[1]); got != agent.Deny {
			t.Errorf("widened Decide(%s, %q) = %v, want deny", call[0], call[1], got)
		}
	}
}

// planCall is one plan command a fake dream issues and what the verb would do once the gate allows it.
type planCall struct {
	cmd string
	do  func() error
}

// planFixture is a repo whose main has a commit touching a.go, a side-branch commit, a note and a plan of four tasks.
func planFixture(t *testing.T) (work, plan, good, side string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	work = hosttest.RepoAt(t, filepath.Join(t.TempDir(), "proj"))
	hosttest.WriteRel(t, work, "a.go", "package a\n")
	hosttest.In(t, work, "add", "a.go")
	hosttest.In(t, work, "commit", "-q", "-m", "feat: a")
	good = strings.TrimSpace(hosttest.In(t, work, "rev-parse", "--short=7", "HEAD"))
	hosttest.In(t, work, "switch", "-q", "-c", "side")
	hosttest.WriteRel(t, work, "b.go", "package a\n")
	hosttest.In(t, work, "add", "b.go")
	hosttest.In(t, work, "commit", "-q", "-m", "feat: b")
	side = strings.TrimSpace(hosttest.In(t, work, "rev-parse", "--short=7", "HEAD"))
	hosttest.In(t, work, "switch", "-q", "main")
	hosttest.WriteRel(t, memory.StoreDir(), "feedback/x.md", "x\n")
	plan = filepath.Join(plans.Home(work), time.Now().Format(time.DateOnly)+"-e2e.md")
	hosttest.WriteRel(t, filepath.Dir(plan), filepath.Base(plan), "# P\n\n- [ ] Phase 1.1 One — owns: a.go\n- [ ] Phase 1.2 Two\n"+
		"- [ ] Phase 1.3 Three — owns: b.go\n- [x] Phase 1.4 Four ("+good+")\n")
	return work, plan, good, side
}

// A fake dream's plan calls pass the gate or not; Go keeps the proven ones, reopens the bad sha and logs each.
func TestDreamPlanPhaseKeepsProvenChangesOnly(t *testing.T) {
	work, plan, good, side := planFixture(t)
	task := "claude-code plan task "
	calls := []planCall{
		{task + "'Phase 1.1' --done --sha " + good + " --plan " + plan, func() error { return plans.MarkDone(plan, "Phase 1.1", good) }},
		{task + "'Phase 1.2' --strike 'moot — evidence: note:feedback/x.md' --plan " + plan,
			func() error { return plans.Strike(plan, "Phase 1.2", "moot — evidence: note:feedback/x.md") }},
		{task + "'Phase 1.3' --done --sha " + side + " --plan " + plan, func() error { return plans.MarkDone(plan, "Phase 1.3", side) }},
		{task + "'Phase 1.4' --reopen --plan " + plan, func() error { return plans.Reopen(plan, "Phase 1.4") }},
	}
	denied, prompt := 0, ""
	run := func(ctx context.Context, p, cwd string) (string, error) {
		prompt = p
		gate := permissions(memory.JobScope(memory.JobKind(ctx), cwd))
		for _, c := range calls {
			if gate.Decide("Bash", c.cmd) != agent.Allow {
				denied++
			} else if err := c.do(); err != nil {
				t.Error(err)
			}
		}
		return "fake", nil
	}
	var out strings.Builder
	if err := memory.RunDream(memory.DreamRunOptions{CWD: work, Force: true, Out: &out, Run: run}); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(plan)
	if !strings.Contains(prompt, `"id":"Phase 1.3"`) || denied != 1 || !strings.Contains(out.String(), "plans: kept 2, reverted 1") {
		t.Fatalf("prompt lists open tasks %v, denied %d, out:\n%s", strings.Contains(prompt, "Open plan tasks"), denied, out.String())
	}
	for _, want := range []string{"[x] Phase 1.1 One — owns: a.go (" + good + ")", "~~Phase 1.2 Two~~ — superseded: moot",
		"[ ] Phase 1.3 Three — owns: b.go\n", "[x] Phase 1.4 Four"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plan lacks %q:\n%s", want, body)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(paths.StateDir(), "memory", "plans-audit.jsonl")); strings.Count(string(log), `"verdict":"kept"`) != 2 ||
		!strings.Contains(string(log), `"verdict":"reverted: sha is not on the trunk branch"`) {
		t.Errorf("log:\n%s", log)
	}
}
