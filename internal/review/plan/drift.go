package plan

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// savedWorkflowsDoc is the doc (relative to the root) whose table must list every workflows/*.js script.
const savedWorkflowsDoc = "skills/subagent-workflows/references/saved-workflows.md"

var docRowName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("^\\|\\s*`([^`|]+)`\\s*\\|") })

// documentedWorkflows returns the backticked first-cell names of the doc's table rows (header/separator rows have none).
func documentedWorkflows(doc string) []string {
	var names []string
	for _, l := range strings.Split(doc, "\n") {
		if m := docRowName().FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// workflowDocDrift returns one message per workflows/*.js missing from the doc table and per documented name with
// no script; nil when the doc is absent.
func workflowDocDrift(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(savedWorkflowsDoc)))
	if err != nil {
		return nil
	}
	scripts, _ := filepath.Glob(filepath.Join(root, "workflows", "*.js"))
	sort.Strings(scripts)
	have := map[string]bool{}
	for _, s := range scripts {
		have[strings.TrimSuffix(filepath.Base(s), ".js")] = true
	}
	names := documentedWorkflows(string(data))
	documented := map[string]bool{}
	for _, n := range names {
		documented[n] = true
	}
	var out []string
	for _, s := range scripts {
		if !documented[strings.TrimSuffix(filepath.Base(s), ".js")] {
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
