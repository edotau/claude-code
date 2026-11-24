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
		{"stepof", WorkflowStepOfSource(), "77fd9df09c3f5cf30d535b6633edfd22b807577a854ca4c1c129606aef1f4117"},
		{"stages", WorkflowStagesSource(), "77faa84b816251f63bced0d5fa6957241fb161faab7740b470ccd312adcbf91f"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.source))); got != tc.want {
			t.Errorf("%s: sha256 %s, want pre-migration %s", tc.name, got, tc.want)
		}
	}
}
