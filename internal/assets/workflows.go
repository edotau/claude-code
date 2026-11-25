package assets

import (
	"strings"
	"sync"
	"text/template/parse"
)

var workflowNameEscaper = strings.NewReplacer(`\`, `\\`, "'", `\'`, "\n", `\n`, "\r", `\r`)

type workflowSkeleton struct {
	once   sync.Once
	parsed map[string]*parse.ListNode
}

var pipelineSkeleton, parallelSkeleton, evaluatorSkeleton, orchestratorSkeleton workflowSkeleton

// WorkflowStepOfSource returns the canonical workflow source.
func WorkflowStepOfSource() string { return string(read("files/workflows/blocks/stepof.js")) }

// WorkflowStagesSource returns the canonical workflow source.
func WorkflowStagesSource() string { return string(read("files/workflows/blocks/stages.js")) }

// WorkflowAnalyzerPromptsSource returns the canonical workflow source.
func WorkflowAnalyzerPromptsSource() string {
	return string(read("files/workflows/prompts/analyzer.js"))
}

// WorkflowEngineerPromptsSource returns the canonical workflow source.
func WorkflowEngineerPromptsSource() string {
	return string(read("files/workflows/prompts/engineer.js"))
}

// WorkflowEvaluatorPromptsSource returns the canonical workflow source.
func WorkflowEvaluatorPromptsSource() string {
	return string(read("files/workflows/prompts/evaluator.js"))
}

// WorkflowWorkersPromptsSource returns the canonical workflow source.
func WorkflowWorkersPromptsSource() string { return string(read("files/workflows/prompts/workers.js")) }

// WorkflowPreflightPromptsSource returns the canonical workflow source.
func WorkflowPreflightPromptsSource() string {
	return string(read("files/workflows/prompts/preflight.js"))
}

// WorkflowLooperPromptsSource returns the canonical workflow source.
func WorkflowLooperPromptsSource() string { return string(read("files/workflows/prompts/looper.js")) }

// WorkflowSkeletonNote returns the canonical workflow source.
func WorkflowSkeletonNote() string { return string(read("files/workflows/skeletons/note.js")) }

// WorkflowPipelineSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowPipelineSkeleton(name string) string {
	return pipelineSkeleton.render(name, func() []byte { return read("files/workflows/skeletons/pipeline.js.tmpl") })
}

// WorkflowParallelSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowParallelSkeleton(name string) string {
	return parallelSkeleton.render(name, func() []byte { return read("files/workflows/skeletons/parallel.js.tmpl") })
}

// WorkflowEvaluatorSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowEvaluatorSkeleton(name string) string {
	return evaluatorSkeleton.render(name, func() []byte { return read("files/workflows/skeletons/evaluator.js.tmpl") })
}

// WorkflowOrchestratorSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowOrchestratorSkeleton(name string) string {
	return orchestratorSkeleton.render(name, func() []byte { return read("files/workflows/skeletons/orchestrator.js.tmpl") })
}

func (s *workflowSkeleton) render(name string, source func() []byte) string {
	s.once.Do(func() { s.parsed = parseTemplates("workflow", source()) })
	return renderFrom(s.parsed, "workflow", tmplFields{"Name": workflowNameEscaper.Replace(name)})
}
