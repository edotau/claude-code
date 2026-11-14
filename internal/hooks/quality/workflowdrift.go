package quality

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// savedWorkflowsDoc is the doc (relative to the root) whose table must list every workflows/*.js script.
const savedWorkflowsDoc = "skills/subagent-workflows/references/saved-workflows.md"

var docRowName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("^\\|\\s*`([^`|]+)`\\s*\\|") })

// documentedWorkflows returns the backticked first-cell names of the doc's table rows (header/separator rows have none).
func documentedWorkflows(doc string) []string {
	var names []string
	for l := range strings.SplitSeq(doc, "\n") {
		if m := docRowName().FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// workflowDocDrift returns one message per workflows/*.js missing from the doc table and per documented name with
// no script; nil when the doc is absent, one message when it exists but cannot be read.
func workflowDocDrift(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(savedWorkflowsDoc)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{savedWorkflowsDoc + ": " + err.Error()}
	}
	scripts := workflowScripts(root)
	have := map[string]bool{}
	for _, s := range scripts {
		have[strings.TrimSuffix(filepath.Base(s), ".js")] = true
	}
	names := documentedWorkflows(string(data))
	var out []string
	for _, s := range scripts {
		if !slices.Contains(names, strings.TrimSuffix(filepath.Base(s), ".js")) {
			out = append(out, s+": not documented in "+savedWorkflowsDoc)
		}
	}
	for _, n := range names {
		if !have[n] {
			out = append(out, "saved-workflows.md: "+n+" has no workflows/"+n+".js")
		}
	}
	return out
}

const helperStart, helperEnd = "// ── stepOf:", "// ── end stepOf ──"
const stagesStart, stagesEnd = "// ── stages:", "// ── end stages ──"

// helperBlock is one library block that every script calling one of its functions must carry verbatim.
type helperBlock struct {
	name, start, end string
	uses             []string
}

var helperBlocks = []helperBlock{
	{"stepOf", helperStart, helperEnd, []string{"stepOf("}},
	{"stages", stagesStart, stagesEnd, []string{"stageLoop(", "stageGate(", "stageFix(", "stagePolish(", "stageDetect("}},
}

// lintTargets is every workflow script plus every skill script under root.
func lintTargets(root string) []string {
	return append(workflowScripts(root), skillScripts(root)...)
}

// workflowHelperDrift returns one message per script that calls a block's functions with that block missing or differing.
func workflowHelperDrift(root string) []string {
	scripts := lintTargets(root)
	srcs := map[string]string{}
	for _, s := range scripts {
		if data, err := os.ReadFile(s); err == nil { // lintScript already reports an unreadable script
			srcs[s] = string(data)
		}
	}
	var out []string
	for _, b := range helperBlocks {
		out = append(out, blockDrift(b, scripts, srcs)...)
	}
	return out
}

// blockDrift compares every script's copy of b with the first script's copy.
func blockDrift(b helperBlock, scripts []string, srcs map[string]string) []string {
	var out []string
	first, firstName := "", ""
	for _, s := range scripts {
		src, ok := srcs[s]
		if !ok {
			continue
		}
		i, j := strings.Index(src, b.start), strings.Index(src, b.end)
		switch {
		case i < 0 && !usesAny(src, b.uses):
			continue
		case i < 0 || j < i:
			out = append(out, s+": no "+b.name+" block ("+b.start+" … "+b.end+")")
		case firstName == "":
			first, firstName = src[i:j], s
		case src[i:j] != first:
			out = append(out, s+": "+b.name+" block differs from "+filepath.Base(firstName)+" (copy it verbatim)")
		}
	}
	return out
}

// usesAny reports whether src contains any of the call tokens.
func usesAny(src string, uses []string) bool {
	return slices.ContainsFunc(uses, func(u string) bool { return strings.Contains(src, u) })
}

// WorkflowDrift returns the saved-workflow doc and helper-block drift under root; empty when both are in sync.
func WorkflowDrift(root string) []string {
	return append(workflowDocDrift(root), workflowHelperDrift(root)...)
}
