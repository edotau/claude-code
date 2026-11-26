package assets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
)

func TestWorkflowSkeletonMatchesTextTemplate(t *testing.T) {
	name := "a'b\\c\nd"
	for file, render := range map[string]func(string) string{
		"pipeline": WorkflowPipelineSkeleton, "parallel": WorkflowParallelSkeleton,
		"evaluator": WorkflowEvaluatorSkeleton, "orchestrator": WorkflowOrchestratorSkeleton,
	} {
		ref := template.Must(template.New("").Parse(string(read("workflows/skeletons/" + file + ".js.tmpl"))))
		var want strings.Builder
		if err := ref.Execute(&want, struct{ Name string }{workflowNameEscaper.Replace(name)}); err != nil {
			t.Fatal(err)
		}
		if got := render(name); got != want.String() {
			t.Errorf("%s skeleton differs from text/template output", file)
		}
	}
}

func TestWorkflowSkeletonKeysDistinct(t *testing.T) {
	keys := map[string]bool{}
	renderedKey = func(key string) { keys[key] = true }
	defer func() { renderedKey = nil }()
	WorkflowPipelineSkeleton("n")
	WorkflowParallelSkeleton("n")
	WorkflowEvaluatorSkeleton("n")
	WorkflowOrchestratorSkeleton("n")
	if len(keys) != 4 {
		t.Errorf("skeletons rendered under %d keys, want 4: %v", len(keys), keys)
	}
}

func TestWorkflowStepOfCopies(t *testing.T) {
	for _, name := range []string{"analyzer", "engineer", "evaluator", "looper", "workers"} {
		file := filepath.Join("..", "..", "workflows", "code-"+name+".js")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		_, body, found := strings.Cut(string(raw), "// <block:stepOf>\n")
		body, _, end := strings.Cut(body, "// </block:stepOf>")
		if !found || !end || body != WorkflowStepOfSource() {
			t.Errorf("%s: stepOf block differs from the canonical bytes", file)
		}
	}
}
