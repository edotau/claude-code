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
		{"stages", WorkflowStagesSource(), "d137bd307b524c6e4b4e66029dd05f3ed1f962c5625df76bb7b916fee8cb38f3"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.source))); got != tc.want {
			t.Errorf("%s: sha256 %s, want pre-migration %s", tc.name, got, tc.want)
		}
	}
}
