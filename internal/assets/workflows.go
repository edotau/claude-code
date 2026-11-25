package assets

import (
	"strings"
)

var workflowNameEscaper = strings.NewReplacer(`\`, `\\`, "'", `\'`, "\n", `\n`, "\r", `\r`)

var skeletonTemplates = loadTemplates("files/workflows/skeletons/*.tmpl")

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
	return renderSkeleton("pipeline.js.tmpl", name)
}

// WorkflowParallelSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowParallelSkeleton(name string) string {
	return renderSkeleton("parallel.js.tmpl", name)
}

// WorkflowEvaluatorSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowEvaluatorSkeleton(name string) string {
	return renderSkeleton("evaluator.js.tmpl", name)
}

// WorkflowOrchestratorSkeleton renders a workflow with the same single-quoted name escaping as the CLI.
func WorkflowOrchestratorSkeleton(name string) string {
	return renderSkeleton("orchestrator.js.tmpl", name)
}

func renderSkeleton(file, name string) string {
	return renderFrom(skeletonTemplates(), file, tmplFields{"Name": workflowNameEscaper.Replace(name)})
}
