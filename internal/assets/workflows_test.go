package assets

import (
	"crypto/sha256"
	"fmt"
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
		ref := template.Must(template.New("").Parse(string(read("files/workflows/skeletons/" + file + ".js.tmpl"))))
		var want strings.Builder
		if err := ref.Execute(&want, struct{ Name string }{workflowNameEscaper.Replace(name)}); err != nil {
			t.Fatal(err)
		}
		if got := render(name); got != want.String() {
			t.Errorf("%s skeleton differs from text/template output", file)
		}
	}
}

func TestWorkflowBlockBytes(t *testing.T) {
	for _, tc := range []struct {
		name, source, want string
	}{
		{"stepof", WorkflowStepOfSource(), "59e4ffbf5145cdb1e2da1a99847c4c6896e2500f8acc7920389c39bf6e8ae3ef"},
		{"stages", WorkflowStagesSource(), "033664829e9c242c4c669752f8c43ab3401950d4483293d43c43045a89c93d17"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.source))); got != tc.want {
			t.Errorf("%s: sha256 %s, want pre-migration %s", tc.name, got, tc.want)
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
