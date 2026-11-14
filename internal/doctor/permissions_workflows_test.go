package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/waves"
)

const scratchScript = `const S = {
  find: stepOf(a.steps, 'find', { model: 'sonnet', effort: 'low' }, OPT),
  fix: stepOf(a.steps, 'fix', { model: 'sonnet', tools: 'edit' }, OPT),
  scope: stepOf(a.steps, 'scope', { model: 'haiku', tools: 'run' }, OPT),
}
const g = await stageGate(CTX, { scope: SCOPE, check: S.scope, fix: S.fix })
`

func scratchWorkflows(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestParseStagesDerivesNeedsFromTheScript(t *testing.T) {
	defs := parseStages("x", scratchScript+"// S.find asks WebSearch\n")
	got := map[string][]string{}
	for _, d := range defs {
		got[d.Stage] = d.Needs
	}
	want := map[string][]string{
		"find":  {"read", "web"},
		"fix":   {"read", "edit", "tests"},
		"scope": {"read", "gate"},
	}
	for stage, needs := range want {
		if !slices.Equal(got[stage], needs) {
			t.Errorf("%s needs %v, want %v", stage, got[stage], needs)
		}
	}
}

func TestPermStageRowsGrowWithAScriptsStages(t *testing.T) {
	dir := scratchWorkflows(t, map[string]string{"a.js": scratchScript})
	agents := []string{"copilot"}
	rows, _, err := permStageRows(dir, agents, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := len(rows)
	src := scratchScript + "const T = { review: stepOf(a.steps, 'review', { effort: 'high' }, OPT) }\n"
	if err := os.WriteFile(filepath.Join(dir, "a.js"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if rows, _, err = permStageRows(dir, agents, t.TempDir()); err != nil || len(rows) != before+1 {
		t.Fatalf("a new stage must add one row: %d rows, was %d, err %v", len(rows), before, err)
	}
	if last := rows[len(rows)-1]; last.Script != "agents dispatch --waves" || rows[len(rows)-2].Stage != "review" {
		t.Fatalf("want the new stage then the waves leaf, got %+v", rows[len(rows)-2:])
	}
}

func TestStageCellDeniesWhatAReadOnlyLaunchCannotDo(t *testing.T) {
	wd := t.TempDir()
	judge, why := permJudge("copilot", "read-only", wd)
	probes := map[string]probe{}
	for _, p := range permProbes(wd) {
		probes[p.job] = p
	}
	c := stageCell(judge, why, "read-only", []string{"read", "edit", "gate"}, probes)
	if c.Verdict != "denied" || len(c.Denied) != 2 || !strings.HasPrefix(c.Denied[0], "edit") {
		t.Fatalf("copilot --read-only must deny edit and gate, keep read: %+v", c)
	}
	if c = stageCell(judge, why, "read-only", []string{"read"}, probes); c.Verdict != "ok" {
		t.Fatalf("a read stage is granted: %+v", c)
	}
	if c = stageCell(nil, "OS sandbox", "tools", []string{"edit"}, probes); c.Verdict != verdictRuntime || c.Why != "OS sandbox" {
		t.Fatalf("no judge is not evaluable: %+v", c)
	}
}

func TestDoctorPermissionsFailsWhenAStageIsDeniedWhatItNeeds(t *testing.T) {
	dir := t.TempDir()
	wf := filepath.Join(dir, "workflows")
	if err := os.Mkdir(wf, 0o700); err != nil {
		t.Fatal(err)
	}
	src := "const S = { gate: stepOf(a.steps, 'gate', { effort: 'low' }, OPT) }\n" +
		"await stageGate(CTX, { scope: SCOPE, check: S.gate })\n"
	if err := os.WriteFile(filepath.Join(wf, "a.js"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var out bytes.Buffer
	if code := doctorPermissions(&out, "copilot", false); code != 1 {
		t.Fatalf("a read-only gate stage must fail the matrix: exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "denied: gate") || !strings.Contains(out.String(), "[read-only]") {
		t.Fatalf("the denied need must be named:\n%s", out.String())
	}
}

func TestDoctorPermissionsJSONListsTheStageSection(t *testing.T) {
	var out bytes.Buffer
	if code := doctorPermissions(&out, "copilot", true); code > 1 { // 1 is a denied row, a finding rather than a failure
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), `"script": "agents dispatch --waves"`) {
		t.Fatalf("the waves leaf row is missing from the JSON:\n%s", out.String())
	}
}

func TestPermNeedsStayInStepWithTheProbesAndScripts(t *testing.T) {
	probes := map[string]bool{}
	for _, p := range permProbes("/wd") {
		probes[p.job] = true
	}
	for _, n := range needJobs {
		for _, job := range n.jobs {
			if !probes[job] {
				t.Errorf("need %s names probe %q, which permProbes lacks", n.need, job)
			}
		}
	}
	scripts, _ := filepath.Glob("../../workflows/*.js")
	if len(scripts) == 0 {
		t.Skip("no workflows/ beside this checkout")
	}
	for _, s := range scripts {
		src, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if len(parseStages(s, string(src))) == 0 {
			t.Errorf("%s: no stepOf stage parsed", s)
		}
		if !strings.Contains(string(src), "who === 'copilot' ? '--read-only' : '--tools'") {
			t.Errorf("%s: stepOf's launch-mode rule changed; update stageLaunchMode", s)
		}
	}
}

// A stage's needs follow what its prompt tells the agent to do, not its tools tier alone.
func TestStageNeedsFollowThePromptNotTheTier(t *testing.T) {
	src := `const S = {
  record: stepOf(a.steps, 'record', { tools: 'edit' }, OPT),
  depscan: stepOf(a.steps, 'depscan', { model: 'sonnet' }, OPT),
}
S.record.task('Write the JSON to a NEW file, then run exactly ` + "`claude-code memory report --file x`" + `.
  Edit nothing else.')
S.record.schema(R)
S.depscan.task('Read-only scan. Run once: ` + "`claude-code worktree gate --dir p`" + ` and list files.')
S.depscan.schema(R)
`
	got := map[string][]string{}
	for _, d := range parseStages("x", src) {
		got[d.Stage] = d.Needs
	}
	if want := []string{"read", "write", "run:claude-code memory report"}; !slices.Equal(got["record"], want) {
		t.Errorf("record needs %v, want %v (an edit-tier stage told to edit nothing does not edit)", got["record"], want)
	}
	if want := []string{"read", "run:claude-code worktree gate"}; !slices.Equal(got["depscan"], want) {
		t.Errorf("depscan needs %v, want %v (a read-only tier whose prompt runs a command needs it)", got["depscan"], want)
	}
	wd := t.TempDir()
	judge, why := permJudge("copilot", "read-only", wd)
	probes := map[string]probe{}
	for _, p := range permProbes(wd) {
		probes[p.job] = p
	}
	if c := stageCell(judge, why, "read-only", got["depscan"], probes); c.Verdict != "denied" {
		t.Errorf("a read-only copilot launch cannot run the scan command: %+v", c)
	}
}

// The waves-leaf row reads the launch mode and the brief from the launch code, so a launch change moves the row.
func TestWavesLeafRowFollowsTheLaunchCode(t *testing.T) {
	yes := true
	for _, c := range []struct {
		args  []string
		tools *bool
		want  string
	}{
		{nil, nil, "no flag"}, {nil, &yes, "tools"}, {[]string{"--tools"}, nil, "tools"},
		{[]string{"--read-only"}, &yes, "read-only"},
	} {
		if got := launchModeOf(c.args, c.tools); got != c.want {
			t.Errorf("launchModeOf(%v, %v) = %q, want %q", c.args, c.tools, got, c.want)
		}
	}
	def, err := wavesLeafDef()
	if err != nil {
		t.Fatal(err)
	}
	args, tools, _, err := waves.LeafLaunch(waves.LaunchOptions{}, "a.go", "go test ./x && go vet ./x")
	if err != nil {
		t.Fatal(err)
	}
	if want := launchModeOf(args, tools); def.Launch != want || want != "tools" {
		t.Errorf("a nil Tools launches every leaf with --tools: row launch %q, launch code %q, want tools", def.Launch, want)
	}
	for _, need := range []string{"edit", "write", "tests", "vet"} {
		if !slices.Contains(def.Needs, need) {
			t.Errorf("leaf needs %v lack %q, which its brief and verify command ask for", def.Needs, need)
		}
	}
}
