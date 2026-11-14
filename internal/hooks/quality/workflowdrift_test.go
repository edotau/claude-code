package quality

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const driftDocRel = "skills/subagent-workflows/references/saved-workflows.md"

const okBody = "export const meta = { name: 'x' }\nawait agent('x', { model: 'sonnet', effort: 'low' })\n"

// writeDriftRoot builds a config root with one workflows/<name>.js per script and, when doc is set, the saved-workflows doc.
func writeDriftRoot(t *testing.T, scripts []string, doc *string) string {
	t.Helper()
	root := t.TempDir()
	wf := filepath.Join(root, "workflows")
	if err := os.Mkdir(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range scripts {
		writeScript(t, wf, s+".js", okBody)
	}
	if doc != nil {
		p := filepath.Join(root, driftDocRel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(*doc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Saved-workflows doc vs workflows/*.js: each side must name the other, else the verb exits 1.
func TestWorkflowLintDrift(t *testing.T) {
	noNode(t)
	table := func(names ...string) string {
		s := "| Workflow | Use |\n| --- | --- |\n"
		var sSb18 strings.Builder
		for _, n := range names {
			sSb18.WriteString("| `" + n + "` | does " + n + " |\n")
		}
		s += sSb18.String()
		return s
	}
	cases := []struct {
		name    string
		scripts []string
		doc     *string
		wantRC  int
		wantOut string
		wantErr []string
	}{
		{"in sync", []string{"a", "b"}, new(table("a", "b")), 0, "workflow lint: 2 script(s) clean", nil},
		{"undocumented script", []string{"a", "b"}, new(table("a")), 1, "",
			[]string{filepath.Join("workflows", "b.js") + ": not documented in " + driftDocRel}},
		{"documented but missing", []string{"a"}, new(table("a", "ghost")), 1, "",
			[]string{"saved-workflows.md: ghost has no workflows/ghost.js"}},
		{"no doc skips check", []string{"a", "b"}, nil, 0, "workflow lint: 2 script(s) clean", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeDriftRoot(t, tc.scripts, tc.doc)
			var out, errb bytes.Buffer
			rc := RunWorkflowLint([]string{root}, &out, &errb)
			if rc != tc.wantRC {
				t.Fatalf("rc=%d want %d; out=%q err=%q", rc, tc.wantRC, out.String(), errb.String())
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("stdout %q lacks %q", out.String(), tc.wantOut)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(errb.String(), w) {
					t.Fatalf("stderr %q lacks %q", errb.String(), w)
				}
			}
			if left := strings.ReplaceAll(errb.String(), "workflow lint: parse check skipped (node not on PATH)\n", ""); tc.wantRC == 0 && left != "" {
				t.Fatalf("unexpected stderr %q", left)
			}
		})
	}
}

func TestWorkflowHelperDrift(t *testing.T) {
	block := func(tag string) string {
		return helperStart + " x\nfunction stepOf() {" + tag + "}\n" + helperEnd + "\n"
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	put := func(name, body string) { writeScript(t, filepath.Join(root, "workflows"), name, body) }
	put("a.js", block("1"))
	put("b.js", block("1"))
	put("none.js", "const x = 1\n")
	if got := workflowHelperDrift(root); got != nil {
		t.Fatalf("identical blocks: %v", got)
	}
	put("b.js", block("2"))
	got := workflowHelperDrift(root)
	if len(got) != 1 || !strings.Contains(got[0], "b.js") || !strings.Contains(got[0], "differs from a.js") {
		t.Fatalf("differing block: %v", got)
	}
	put("b.js", block("1"))
	put("c.js", "stepOf(1)\n")
	got = workflowHelperDrift(root)
	if len(got) != 1 || !strings.Contains(got[0], "c.js") || !strings.Contains(got[0], "no stepOf block") {
		t.Fatalf("missing block: %v", got)
	}
}

// A stages block is held to the same verbatim rule as stepOf; a stage call without the block is a hit.
func TestWorkflowStagesDrift(t *testing.T) {
	block := func(tag string) string {
		return stagesStart + " x\nasync function stageLoop() {" + tag + "}\n" + stagesEnd + "\n"
	}
	root := t.TempDir()
	dir := filepath.Join(root, "workflows")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "a.js", block("1"))
	writeScript(t, dir, "b.js", block("2"))
	writeScript(t, dir, "c.js", "await stageFix(ctx, {})\n")
	got := strings.Join(workflowHelperDrift(root), "\n")
	if !strings.Contains(got, "b.js: stages block differs from a.js") || !strings.Contains(got, "c.js: no stages block") {
		t.Fatalf("stages drift: %q", got)
	}
}

// A workflow script under skills/*/scripts is checked for a stepOf block like workflows/*.js.
func TestWorkflowHelperDriftSkillScripts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills", "s", "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeScript(t, dir, "w.js", "export const meta = {}\nstepOf(1)\n")
	got := workflowHelperDrift(root)
	if len(got) != 1 || !strings.Contains(got[0], "w.js") || !strings.Contains(got[0], "no stepOf block") {
		t.Fatalf("skill script without block: %v", got)
	}
}

// An unreadable script is lintScript's to report; workflowHelperDrift must skip it, not add a second line.
func TestHelperDriftSkipsUnreadableScript(t *testing.T) {
	root := t.TempDir()
	wf := filepath.Join(root, "workflows")
	if err := os.Mkdir(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gone.js"), filepath.Join(wf, "dangling.js")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if got := workflowHelperDrift(root); len(got) != 0 {
		t.Fatalf("workflowHelperDrift = %q, want no report for an unreadable script", got)
	}
}

func TestWorkflowStageLoopDrift(t *testing.T) {
	for _, call := range []string{"stageLoop("} {
		t.Run(call, func(t *testing.T) {
			root := writeDriftRoot(t, nil, nil)
			writeScript(t, filepath.Join(root, "workflows"), "missing.js", "await "+call+")\n")
			got := workflowHelperDrift(root)
			if len(got) != 1 || !strings.Contains(got[0], "no stages block") {
				t.Fatalf("drift: %v", got)
			}
		})
	}
	root := writeDriftRoot(t, nil, nil)
	writeScript(t, filepath.Join(root, "workflows"), "legacy.js", "await stageGate(ctx, {})\n")
	if got := workflowHelperDrift(root); len(got) != 0 {
		t.Fatalf("removed stageGate must not require the shared block: %v", got)
	}
}

// looperProof drives code-looper.js's iteration exits under node with a stubbed agent; argv[1] is the script body.
const looperProof = `
const assert = require('assert');
const Async = Object.getPrototypeOf(async function(){}).constructor;
const run = new Async('args','agent','parallel','log','phase', process.argv[1]);
const failure = {file:'x.go',line:1,text:'broken'};
async function check(args, sequence, texts, stable) {
  let calls=0, fixes=0, prompt='';
  const r = await run(args, async (p,o) => {
    if (o.phase === 'Fix') {
      fixes++; prompt=p; assert(!p.includes('run the whole gate'));
      return {status:'done',files:['x.go'],summary:'claim'};
    }
    calls++;
    const exit = sequence.shift();
    const hash = '0'.repeat(39) + (stable ? 0 : calls);
    return {gate:'false',exit,changed:['x.go '+hash,'old.go '+'1'.repeat(40)],
      failures:exit ? [{...failure,text:(texts||[])[calls-1]||failure.text}] : []};
  }, async jobs => Promise.all(jobs.map(f=>f())), ()=>{}, ()=>{});
  assert.equal(r.gateRuns,calls); assert(!('status' in r));
  return {r,fixes,prompt};
}
(async()=> {
  let x = await check({scope:['.'],gate:'false'},[1,0]);
  assert.equal(x.r.exit,'green'); assert.equal(x.r.iterations,1); assert.equal(x.r.gateRuns,2);
  assert(x.prompt.includes('package directory'), 'a Go failure is owned by its package: the cause may be outside the test file');
  x = await check({scope:['.'],gate:'false'},[1,1]);
  assert.equal(x.r.exit,'stalled'); assert.equal(x.r.iterations,1);
  x = await check({scope:['.'],gate:'false'},[1,1],['broken','other'],true);
  assert.equal(x.r.exit,'stalled'); assert.deepEqual(x.r.filesChanged,[]); // a failure moved but no file did
  x = await check({scope:['.'],gate:'false'},[1,0]); assert.deepEqual(x.r.filesChanged,['x.go']); // old.go was dirty before
  x = await check({scope:['.'],gate:'false',maxIterations:0},[1]);
  assert.equal(x.r.exit,'red'); assert.equal(x.fixes,0);
  x = await check({gate:'false'},[]); assert.equal(x.r.exit,'error'); assert.equal(x.r.gateRuns,0);
  x = await check({scope:['.'],gate:'false',iterationsUsed:5},[1]); assert.equal(x.fixes,0);
  let snaps = 0;
  const m = await run({scope:['.'],gate:'true',measure:true}, async (p,o) => {
    if (o.phase !== 'Measure') return {gate:'true',exit:0,changed:[],failures:[]};
    const peer = {path:'peer.go',added:1+8*snaps,deleted:0}, mine = {path:'x.go',added:2,deleted:0};
    return {files: snaps++ ? [mine, peer] : [peer], outside:['peer.go']};
  }, async jobs => Promise.all(jobs.map(f=>f())), ()=>{}, ()=>{});
  assert.deepEqual(m.lines.stages.flatMap(r=>r.files),['x.go'], 'peer.go must not be measured: '+JSON.stringify(m.lines.stages));
})().catch(e => {console.error(e); process.exitCode=1});`

func TestLooperObservedExits(t *testing.T) {
	useNode(t)
	node, err := lookNode()
	if err != nil {
		t.Fatal(err)
	}
	src, _ := realScript(t, "code-looper.js")
	body := strings.Replace(string(src), "export const meta", "const meta", 1)
	if out, err := exec.Command(node, "-e", looperProof, body).CombinedOutput(); err != nil {
		t.Fatalf("looper: %v\n%s", err, out)
	}
}
