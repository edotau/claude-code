package web

import (
	"os/exec"
	"strings"
	"testing"
)

// --- dashboard template escapes pane status fields before interpolating them into HTML ---

func TestRenderDispatchesTemplateEscapesStatusFields(t *testing.T) {
	src, err := assetsFS.ReadFile("assets/index.html.tmpl")
	if err != nil {
		t.Fatalf("read index.html.tmpl: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "function renderDispatches")
	if start < 0 {
		t.Fatal("renderDispatches function not found in the dashboard template")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of renderDispatches")
	}
	fn := body[start : start+end]
	for _, field := range []string{"p.session", "p.agent", "p.reason"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderDispatches interpolates %s without escapeHTML(...): regression risk for a pane running attacker-controlled text\n%s", field, fn)
		}
	}
}

// --- dashboard template escapes the new activity/dispatch/snapshot/browser fields before interpolating ---

// extractJSFunc returns the body of the first JS function named name in src, the same
// start/end-delimiter approach TestRenderDispatchesTemplateEscapesStatusFields uses.
func extractJSFunc(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "function "+name)
	if start < 0 {
		t.Fatalf("function %s not found in the dashboard template", name)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("could not find the end of %s", name)
	}
	return src[start : start+end]
}

func templateSource(t *testing.T) string {
	t.Helper()
	src, err := assetsFS.ReadFile("assets/index.html.tmpl")
	if err != nil {
		t.Fatalf("read index.html.tmpl: %v", err)
	}
	return string(src)
}

func TestRenderActivityEscapesToolAndText(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderActivity")
	for _, field := range []string{"text"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderActivity interpolates %s without escapeHTML(...): regression risk for an agent's own text/tool output\n%s", field, fn)
		}
	}
}

func TestRenderLegEscapesVendorFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderDispatch")
	for _, field := range []string{"l.fallback", "l.agent", "l.model", "l.duration"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderDispatch interpolates %s without escapeHTML(...): regression risk for vendor-reported agent/model/fallback text\n%s", field, fn)
		}
	}
}

func TestRenderSubagentsEscapesDataAttributes(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderSubagents")
	for _, field := range []string{"sessionId", "s.id"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderSubagents interpolates %s without escapeHTML(...): regression risk in the data-session/data-agent attributes\n%s", field, fn)
		}
	}
}

func TestRenderSnapshotEscapesLabelAndFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderSnapshot")
	for _, field := range []string{"snap.label || snap.id", "snap.workflow", "snap.model || ''", "snap.result", "lines"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderSnapshot interpolates %s without escapeHTML(...): regression risk for agent-controlled label/result/event text\n%s", field, fn)
		}
	}
}

// --- dashboard template escapes workflow-run fields (name/phase; status goes through chip()) ---

func TestRenderWorkflowsEscapesNameAndPhase(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderWorkflows")
	for _, field := range []string{"run.name || run.id", "phase.name"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderWorkflows interpolates %s without escapeHTML(...): regression risk for a workflow run's own name/phase text\n%s", field, fn)
		}
	}
	// run.status/is rendered only through chip(...), which escapes internally — not a bare escapeHTML(...) call.
	if !strings.Contains(fn, "chip(run.status)") {
		t.Errorf("renderWorkflows must route run.status through chip(...) (which itself escapes)\n%s", fn)
	}
}

func TestRenderBrowsersEscapesReportFields(t *testing.T) {
	fn := extractJSFunc(t, templateSource(t), "renderBrowsers")
	for _, field := range []string{"b.displayName || b.name", "b.host || ''"} {
		if !strings.Contains(fn, "escapeHTML("+field) {
			t.Errorf("renderBrowsers interpolates %s without escapeHTML(...): regression risk for browser-report text\n%s", field, fn)
		}
	}
}

// Regression: a fetch that began before an SSE render must not overwrite it; the guard lives in the page script.
func TestIndexTreeFetchGuardedByEpoch(t *testing.T) {
	src := templateSource(t)
	load := extractJSFunc(t, src, "loadTree")
	for _, want := range []string{"const started = treeEpoch", "if (treeEpoch !== started) return;"} {
		if !strings.Contains(load, want) {
			t.Fatalf("loadTree lacks %q", want)
		}
	}
	watch := extractJSFunc(t, src, "watchTree")
	if i, j := strings.Index(watch, "treeEpoch++"), strings.Index(watch, "paintTree("); i < 0 || j < 0 || i > j {
		t.Fatal("watchTree must bump treeEpoch before rendering an SSE snapshot")
	}
	if !strings.Contains(watch, "es.onerror") || !strings.Contains(src, "if (!treeLive) loadTree();") {
		t.Fatal("polling fallback missing")
	}
}

// Runs the page's real loadTree/watchTree in node: a fetch that resolves after an SSE snapshot must not overwrite it.
func TestIndexStaleFetchLosesToSSEInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	src := templateSource(t)
	script := `let treeEpoch = 0; let treeLive = false;
const el = {innerHTML: ''}; const document = {getElementById: () => el};
function renderTree(t) { return t.v; }
let resolveFetch; function fetch() { return new Promise(r => { resolveFetch = r; }); }
let listener; const window = {EventSource: true};
function EventSource() { this.addEventListener = (n, f) => { if (n === 'tree') listener = f; }; this.close = () => {}; }
` + extractJSFunc(t, src, "loadTree") + "\n}\n" + extractJSFunc(t, src, "watchTree") + "\n}" + `
loadTree(); watchTree();
listener({data: JSON.stringify({v: 'sse'})});
resolveFetch({json: () => ({v: 'stale'})});
setTimeout(() => { process.stdout.write(el.innerHTML); }, 10);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if string(out) != "sse" {
		t.Fatalf("tree shows %q after a stale fetch resolved, want the SSE snapshot \"sse\"", out)
	}
}
