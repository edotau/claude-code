package assets

import (
	"crypto/sha256"
	"fmt"
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
		{"stepof", WorkflowStepOfSource(), "5b9abd1b6b3e09994f200ea9fef60dfcdb5f85f866d6cb399ded1c1e8161a50f"},
		{"stages", WorkflowStagesSource(), "c7a83d75762a9961fe3d7c337389bca8906bc97b6124f12991877f7171c4d31c"},
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
