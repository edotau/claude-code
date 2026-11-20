package workflows

import (
	_ "embed"
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

// documentedWorkflows returns the backticked first-cell names of the doc's table rows.
func documentedWorkflows(doc string) []string {
	var names []string
	for l := range strings.SplitSeq(doc, "\n") {
		if m := docRowName().FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// workflowDocDrift returns one message per workflows/*.js missing from the doc table or documented name with no script.
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

const helperStart, helperEnd = "// <block:stepOf>\n", "// </block:stepOf>"
const stagesStart, stagesEnd = "// <block:stages>\n", "// </block:stages>"

//go:embed blocks/stepof.js
var stepOfSource string

//go:embed blocks/stages.js
var stagesSource string

// helperBlock is one library block that every script calling one of its functions must carry verbatim.
type helperBlock struct {
	name, start, end, source string
	uses                     []string
}

var helperBlocks = []helperBlock{
	{"stepOf", helperStart, helperEnd, stepOfSource, []string{"stepOf("}},
	{"stages", stagesStart, stagesEnd, stagesSource, []string{"stageLoop(", "stageFix(", "stagePolish(", "stageDetect("}},
}

// lintTargets is every workflow script plus every skill script under root.
func lintTargets(root string) []string {
	return append(workflowScripts(root), skillScripts(root)...)
}

// workflowHelperDrift returns one message per script calling a block's functions with that block missing or differing.
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

// blockDrift checks each copy against the embedded source, including the first script.
func blockDrift(b helperBlock, scripts []string, srcs map[string]string) []string {
	var out []string
	for _, s := range scripts {
		src, ok := srcs[s]
		if !ok {
			continue
		}
		i, j, err := syncBlockBounds(src, b)
		if err != nil {
			out = append(out, s+": "+err.Error())
		} else if i >= 0 && src[i:j] != b.start+b.source+b.end {
			out = append(out, s+": "+b.name+" block differs from embedded source (run claude-code workflow sync)")
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
