package assets

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

const importPath = "github.com/edotau/claude-code/internal/assets"

// inventory is the closed list of embedded files: adding or deleting one fails until this table changes.
var inventory = []struct {
	path, kind, reader string
	read               func() []byte
}{
	{"files/catalog/ranks.json", "json", "RankTable", RankTable},
	{"files/review/autofix/briefs/fix.tmpl", "template", "AutofixBrief", nil},
	{"files/review/bench/briefs/fix.tmpl", "template", "BenchFixBrief", nil},
	{"files/review/bench/briefs/review.tmpl", "template", "BenchReviewBrief", nil},
	{"files/review/bench/no-diff.txt", "text", "BenchNoDiff", func() []byte { return []byte(BenchNoDiff() + "\n") }},
	{"files/tools/search-description.txt", "text", "WebSearchDescription", func() []byte { return []byte(WebSearchDescription() + "\n") }},
	{"files/tools/extract-description.txt", "text", "WebExtractDescription", func() []byte { return []byte(WebExtractDescription() + "\n") }},
	{"files/tools/search-query.txt", "text", "WebSearchQueryDescription", func() []byte { return []byte(WebSearchQueryDescription() + "\n") }},
	{"files/tools/search-max-results.txt", "text", "WebSearchMaxResultsDescription", func() []byte { return []byte(WebSearchMaxResultsDescription() + "\n") }},
	{"files/tools/extract-urls.txt", "text", "WebExtractURLsDescription", func() []byte { return []byte(WebExtractURLsDescription() + "\n") }},
	{"files/tools/proxy-search-description.txt", "text", "ProxySearchDescription", func() []byte { return []byte(ProxySearchDescription() + "\n") }},
	{"files/tools/proxy-search-schema.json", "json", "ProxySearchSchema", func() []byte { return []byte(ProxySearchSchema() + "\n") }},
	{"files/tools/databricks-smoke.txt", "text", "DatabricksSmokePrompt", func() []byte { return []byte(DatabricksSmokePrompt() + "\n") }},
	{"files/tools/databricks-probe.txt", "text", "DatabricksProbePrompt", func() []byte { return []byte(DatabricksProbePrompt() + "\n") }},
	{"files/tools/databricks-tool.txt", "text", "DatabricksToolDescription", func() []byte { return []byte(DatabricksToolDescription() + "\n") }},
	{"files/tools/preflight-pong.txt", "text", "PreflightPongPrompt", func() []byte { return []byte(PreflightPongPrompt() + "\n") }},
	{"files/providers/defaults.json", "json", "ProviderDefaults", ProviderDefaults},
	{"files/models/catalog.json", "json", "ModelCatalog", ModelCatalog},
	{"files/router/table.json", "json", "RouteTable", RouteTable},
	{"files/shell/guards.json", "json", "ShellGuards", ShellGuards},
	{"files/agentconfig/agents.json", "json", "AgentDefaults", AgentDefaults},
	{"files/agentconfig/overlay.schema.json", "json", "OverlaySchemaSnapshot", OverlaySchemaSnapshot},
	{"files/agentconfig/template/opencode.json", "json", "OpencodeConfigTemplate", OpencodeConfigTemplate},
	{"files/agentconfig/template/opencode-tui.json", "json", "OpencodeTUITemplate", OpencodeTUITemplate},
	{"files/settings/automode.json", "json", "SettingsAutoMode", SettingsAutoMode},
	{"files/settings/template.json", "json", "SettingsTemplate", SettingsTemplate},
	{"files/settings/claude-code-settings.schema.json", "json", "SettingsSchema", SettingsSchema},
	{"files/review/source/retired_words.json", "json", "RetiredWords", RetiredWords},
	{"files/ui/theme/default_config.json", "json", "ThemeDefaultConfig", ThemeDefaultConfig},
	{"files/ui/theme/default_palettes.json", "json", "ThemeDefaultPalettes", ThemeDefaultPalettes},
	{"files/workflows/blocks/stepof.js", "js", "WorkflowStepOfSource", func() []byte { return []byte(WorkflowStepOfSource()) }},
	{"files/workflows/blocks/stages.js", "js", "WorkflowStagesSource", func() []byte { return []byte(WorkflowStagesSource()) }},
	{"files/workflows/prompts/analyzer.js", "js", "WorkflowAnalyzerPromptsSource", func() []byte { return []byte(WorkflowAnalyzerPromptsSource()) }},
	{"files/workflows/prompts/engineer.js", "js", "WorkflowEngineerPromptsSource", func() []byte { return []byte(WorkflowEngineerPromptsSource()) }},
	{"files/workflows/prompts/evaluator.js", "js", "WorkflowEvaluatorPromptsSource", func() []byte { return []byte(WorkflowEvaluatorPromptsSource()) }},
	{"files/workflows/prompts/workers.js", "js", "WorkflowWorkersPromptsSource", func() []byte { return []byte(WorkflowWorkersPromptsSource()) }},
	{"files/workflows/prompts/looper.js", "js", "WorkflowLooperPromptsSource", func() []byte { return []byte(WorkflowLooperPromptsSource()) }},
	{"files/workflows/skeletons/note.js", "js", "WorkflowSkeletonNote", func() []byte { return []byte(WorkflowSkeletonNote()) }},
	{"files/workflows/skeletons/pipeline.js.tmpl", "js", "WorkflowPipelineSkeleton", func() []byte { return []byte(WorkflowPipelineSkeleton("{{.Name}}")) }},
	{"files/workflows/skeletons/parallel.js.tmpl", "js", "WorkflowParallelSkeleton", func() []byte { return []byte(WorkflowParallelSkeleton("{{.Name}}")) }},
	{"files/workflows/skeletons/evaluator.js.tmpl", "js", "WorkflowEvaluatorSkeleton", func() []byte { return []byte(WorkflowEvaluatorSkeleton("{{.Name}}")) }},
	{"files/workflows/skeletons/orchestrator.js.tmpl", "js", "WorkflowOrchestratorSkeleton", func() []byte { return []byte(WorkflowOrchestratorSkeleton("{{.Name}}")) }},
	{"files/memory/templates/activeContext.md", "md", "MemoryActiveContextSeed", func() []byte { return []byte(MemoryActiveContextSeed()) }},
	{"files/memory/templates/conventions.md", "md", "MemoryConventionsSeed", func() []byte { return []byte(MemoryConventionsSeed()) }},
	{"files/memory/templates/decisionLog.md", "md", "MemoryDecisionLogSeed", func() []byte { return []byte(MemoryDecisionLogSeed()) }},
	{"files/memory/templates/progress.md", "md", "MemoryProgressSeed", func() []byte { return []byte(MemoryProgressSeed()) }},
	{"files/memory/templates/projectContext.md", "md", "MemoryProjectContextSeed", func() []byte { return []byte(MemoryProjectContextSeed()) }},
	{"files/memory/templates/sessionHistory.md", "md", "MemorySessionHistorySeed", func() []byte { return []byte(MemorySessionHistorySeed()) }},
	{"files/memory/prompts/dream.md", "md", "MemoryDreamProcedure", func() []byte { return read("files/memory/prompts/dream.md") }},
	{"files/memory/prompts/init.md", "md", "MemoryInitProcedure", func() []byte { return read("files/memory/prompts/init.md") }},
	{"files/memory/prompts/promote.md", "md", "MemoryPromoteProcedure", func() []byte { return read("files/memory/prompts/promote.md") }},
	{"files/memory/prompts/review.md", "md", "MemoryReviewProcedure", func() []byte { return read("files/memory/prompts/review.md") }},
	{"files/memory/prompts/update.md", "md", "MemoryUpdateProcedure", func() []byte { return read("files/memory/prompts/update.md") }},
	{"files/memory/prompts/update-opening.tmpl", "tmpl", "MemoryUpdateWorker", func() []byte { return read("files/memory/prompts/update-opening.tmpl") }},
	{"files/memory/prompts/dream-opening.tmpl", "tmpl", "MemoryDreamWorker", func() []byte { return read("files/memory/prompts/dream-opening.tmpl") }},
	{"files/memory/prompts/legacy-update-opening.tmpl", "tmpl", "MemoryLegacyUpdateWorker", func() []byte { return read("files/memory/prompts/legacy-update-opening.tmpl") }},
	{"files/memory/prompts/legacy-dream-opening.tmpl", "tmpl", "MemoryLegacyDreamWorker", func() []byte { return read("files/memory/prompts/legacy-dream-opening.tmpl") }},
	{"files/memory/prompts/historical.txt", "txt", "MemoryHistoricalRule", func() []byte { return read("files/memory/prompts/historical.txt") }},
	{"files/memory/prompts/update-worker.tmpl", "tmpl", "MemoryUpdatePrompt", func() []byte { return read("files/memory/prompts/update-worker.tmpl") }},
	{"files/memory/prompts/transcript-pointer.tmpl", "tmpl", "MemoryTranscriptPointer", func() []byte { return read("files/memory/prompts/transcript-pointer.tmpl") }},
	{"files/memory/prompts/session-digest.tmpl", "tmpl", "MemorySessionDigest", func() []byte { return read("files/memory/prompts/session-digest.tmpl") }},
	{"files/memory/prompts/git-activity.tmpl", "tmpl", "MemoryGitActivity", func() []byte { return read("files/memory/prompts/git-activity.tmpl") }},
	{"files/memory/prompts/session-tags.tmpl", "tmpl", "MemorySessionTags", func() []byte { return read("files/memory/prompts/session-tags.tmpl") }},
	{"files/memory/prompts/dream-worker.tmpl", "tmpl", "MemoryDreamPrompt", func() []byte { return read("files/memory/prompts/dream-worker.tmpl") }},
	{"files/memory/prompts/dream-unchanged.txt", "txt", "MemoryDreamUnchanged", func() []byte { return read("files/memory/prompts/dream-unchanged.txt") }},
	{"files/memory/prompts/open-tasks.tmpl", "tmpl", "MemoryOpenTasks", func() []byte { return read("files/memory/prompts/open-tasks.tmpl") }},
	{"files/memory/prompts/rtk-misses.tmpl", "tmpl", "MemoryRTKMisses", func() []byte { return read("files/memory/prompts/rtk-misses.tmpl") }},
	{"files/memory/prompts/index-hint.txt", "txt", "MemoryIndexHint", func() []byte { return read("files/memory/prompts/index-hint.txt") }},
	{"files/memory/prompts/block-begin.txt", "txt", "MemoryBlockBegin", func() []byte { return read("files/memory/prompts/block-begin.txt") }},
	{"files/memory/prompts/bank-head.tmpl", "tmpl", "MemoryBankHead", func() []byte { return read("files/memory/prompts/bank-head.tmpl") }},
	{"files/memory/prompts/more-entries.tmpl", "tmpl", "MemoryMoreEntries", func() []byte { return read("files/memory/prompts/more-entries.tmpl") }},
	{"files/memory/prompts/unresolved.tmpl", "tmpl", "MemoryUnresolvedEntries", func() []byte { return read("files/memory/prompts/unresolved.tmpl") }},
	{"files/memory/prompts/catalog-header.tmpl", "tmpl", "MemoryCatalogHeader", func() []byte { return read("files/memory/prompts/catalog-header.tmpl") }},
	{"files/memory/prompts/catalog-bank.tmpl", "tmpl", "MemoryCatalogBank", func() []byte { return read("files/memory/prompts/catalog-bank.tmpl") }},
	{"files/memory/prompts/home-index.txt", "txt", "MemoryHomeIndexHeading", func() []byte { return read("files/memory/prompts/home-index.txt") }},
	{"files/memory/prompts/knowledge.txt", "txt", "MemoryKnowledgeHeading", func() []byte { return read("files/memory/prompts/knowledge.txt") }},
	{"files/memory/prompts/home-overflow.tmpl", "tmpl", "MemoryHomeOverflow", func() []byte { return read("files/memory/prompts/home-overflow.tmpl") }},
	{"files/memory/folder-roles.json", "json", "MemoryFolderRoles", func() []byte { return read("files/memory/folder-roles.json") }},
	{"files/memory/prompts/layout.txt", "txt", "MemoryLayoutHeading", func() []byte { return read("files/memory/prompts/layout.txt") }},
	{"files/memory/prompts/layout-row.tmpl", "tmpl", "MemoryLayoutRow", func() []byte { return read("files/memory/prompts/layout-row.tmpl") }},
	{"files/memory/prompts/native.txt", "txt", "MemoryNativeHeading", func() []byte { return read("files/memory/prompts/native.txt") }},
	{"files/memory/prompts/native-folder.tmpl", "tmpl", "MemoryNativeFolder", func() []byte { return read("files/memory/prompts/native-folder.tmpl") }},
	{"files/memory/prompts/native-unparsed.tmpl", "tmpl", "MemoryNativeUnparsed", func() []byte { return read("files/memory/prompts/native-unparsed.tmpl") }},
	{"files/memory/prompts/recall-line.tmpl", "tmpl", "MemoryRecallLine", func() []byte { return read("files/memory/prompts/recall-line.tmpl") }},
	{"files/memory/prompts/recall-header.tmpl", "tmpl", "MemoryRecallHeader", func() []byte { return read("files/memory/prompts/recall-header.tmpl") }},
	{"files/memory/prompts/recall-footer.txt", "txt", "MemoryRecallFooter", func() []byte { return read("files/memory/prompts/recall-footer.txt") }},
	{"files/memory/prompts/recall-marker.txt", "txt", "MemoryRecallMarker", func() []byte { return read("files/memory/prompts/recall-marker.txt") }},
	{"files/memory/prompts/bank-load.tmpl", "tmpl", "MemoryBankLoadNote", func() []byte { return read("files/memory/prompts/bank-load.tmpl") }},
	{"files/memory/prompts/previous-session.tmpl", "tmpl", "MemoryPreviousSession", func() []byte { return read("files/memory/prompts/previous-session.tmpl") }},
	{"files/memory/prompts/update-recovery.tmpl", "tmpl", "MemoryUpdateRecovery", func() []byte { return read("files/memory/prompts/update-recovery.tmpl") }},
	{"files/memory/prompts/scaffold.tmpl", "tmpl", "MemoryScaffoldNote", func() []byte { return read("files/memory/prompts/scaffold.tmpl") }},
	{"files/memory/prompts/catalog-pointer.tmpl", "tmpl", "MemoryCatalogPointer", func() []byte { return read("files/memory/prompts/catalog-pointer.tmpl") }},
	{"files/memory/prompts/bank-heading.tmpl", "tmpl", "MemoryBankHeading", func() []byte { return read("files/memory/prompts/bank-heading.tmpl") }},
	{"files/memory/prompts/native-context.tmpl", "tmpl", "MemoryNativeContext", func() []byte { return read("files/memory/prompts/native-context.tmpl") }},
	{"files/memory/prompts/transcript-handoff.tmpl", "tmpl", "TranscriptHandoff", func() []byte { return read("files/memory/prompts/transcript-handoff.tmpl") }},
	{"files/agent/preamble.tmpl", "tmpl", "AgentPreamble", func() []byte { return read("files/agent/preamble.tmpl") }},
	{"files/agent/web-via-cli.txt", "txt", "AgentWebViaCLI", func() []byte { return read("files/agent/web-via-cli.txt") }},
	{"files/agent/resume.txt", "txt", "AgentResumePrompt", func() []byte { return read("files/agent/resume.txt") }},
	{"files/agent/rubric-lead.txt", "txt", "AgentRubricLead", func() []byte { return read("files/agent/rubric-lead.txt") }},
	{"files/agent/rubric.tmpl", "tmpl", "AgentRubricBody", func() []byte { return read("files/agent/rubric.tmpl") }},
	{"files/agent/persona.tmpl", "tmpl", "AgentPersona", func() []byte { return read("files/agent/persona.tmpl") }},
	{"files/agent/mid-run.tmpl", "tmpl", "AgentMidRun", func() []byte { return read("files/agent/mid-run.tmpl") }},
	{"files/agent/partial.tmpl", "tmpl", "AgentPartial", func() []byte { return read("files/agent/partial.tmpl") }},
	{"files/agent/reconcile.tmpl", "tmpl", "AgentReconcile", func() []byte { return read("files/agent/reconcile.tmpl") }},
	{"files/agent/handoff.tmpl", "tmpl", "AgentHandoff", func() []byte { return read("files/agent/handoff.tmpl") }},
	{"files/agent/recovery.tmpl", "tmpl", "AgentRecovery", func() []byte { return read("files/agent/recovery.tmpl") }},
	{"files/agent/schema.tmpl", "tmpl", "AgentSchemaInstruction", func() []byte { return read("files/agent/schema.tmpl") }},
	{"files/agent/pack-prompt.tmpl", "tmpl", "PackPrompt", func() []byte { return read("files/agent/pack-prompt.tmpl") }},
	{"files/agent/pack-index.tmpl", "tmpl", "PackIndex", func() []byte { return read("files/agent/pack-index.tmpl") }},
	{"files/agent/pack-coverage.tmpl", "tmpl", "PackCoverageWarning", func() []byte { return read("files/agent/pack-coverage.tmpl") }},
	{"files/agent/pack-no-files.txt", "txt", "PackNoFiles", func() []byte { return read("files/agent/pack-no-files.txt") }},
	{"files/hooks/simplicity-header.tmpl", "tmpl", "HookSimplicityHeader", func() []byte { return read("files/hooks/simplicity-header.tmpl") }},
	{"files/hooks/simplicity-more.tmpl", "tmpl", "HookSimplicityMore", func() []byte { return read("files/hooks/simplicity-more.tmpl") }},
	{"files/hooks/paths-failed.tmpl", "tmpl", "HookPathFailed", func() []byte { return read("files/hooks/paths-failed.tmpl") }},
	{"files/hooks/paths-mapped.tmpl", "tmpl", "HookPathMapped", func() []byte { return read("files/hooks/paths-mapped.tmpl") }},
	{"files/hooks/paths-detail.tmpl", "tmpl", "HookPathDetail", func() []byte { return read("files/hooks/paths-detail.tmpl") }},
	{"files/hooks/paths-context.tmpl", "tmpl", "HookPathsContext", func() []byte { return read("files/hooks/paths-context.tmpl") }},
	{"files/hooks/busy-peer.tmpl", "tmpl", "HookBusyPeer", func() []byte { return read("files/hooks/busy-peer.tmpl") }},
	{"files/hooks/notice-line.tmpl", "tmpl", "HookNoticeLine", func() []byte { return read("files/hooks/notice-line.tmpl") }},
	{"files/hooks/notice-drain-warn.tmpl", "tmpl", "HookNoticeDrainWarn", func() []byte { return read("files/hooks/notice-drain-warn.tmpl") }},
	{"files/hooks/notice-diverge-warn.tmpl", "tmpl", "HookNoticeDivergeWarn", func() []byte { return read("files/hooks/notice-diverge-warn.tmpl") }},
	{"files/hooks/diverged.tmpl", "tmpl", "HookDiverged", func() []byte { return read("files/hooks/diverged.tmpl") }},
	{"files/agent/agy-toolless.txt", "txt", "AgyToolLessPreamble", func() []byte { return read("files/agent/agy-toolless.txt") }},
	{"files/agent/stdin-task.txt", "txt", "PiStdinTask", func() []byte { return read("files/agent/stdin-task.txt") }},
	{"files/agent/attach-image.tmpl", "tmpl", "ClaudeAttachImage", func() []byte { return read("files/agent/attach-image.tmpl") }},
	{"files/agent/attach-file.tmpl", "tmpl", "AttachFile", func() []byte { return read("files/agent/attach-file.tmpl") }},
	{"files/agent/attach-ref.tmpl", "tmpl", "AttachRef", func() []byte { return read("files/agent/attach-ref.tmpl") }},
	{"files/agent/plan-tasks-head.txt", "txt", "PlanTasksHead", func() []byte { return read("files/agent/plan-tasks-head.txt") }},
	{"files/agent/plan-tasks-empty.txt", "txt", "PlanTasksEmpty", func() []byte { return read("files/agent/plan-tasks-empty.txt") }},
	{"files/agent/plan-closeout.txt", "txt", "PlanCloseout", func() []byte { return read("files/agent/plan-closeout.txt") }},
	{"files/hooks/plan-path-choose.tmpl", "tmpl", "HookPlanPathChoose", func() []byte { return read("files/hooks/plan-path-choose.tmpl") }},
	{"files/hooks/plan-path-redirect.tmpl", "tmpl", "HookPlanPathRedirect", func() []byte { return read("files/hooks/plan-path-redirect.tmpl") }},
	{"files/hooks/plan-path-moved.tmpl", "tmpl", "HookPlanPathMoved", func() []byte { return read("files/hooks/plan-path-moved.tmpl") }},
	{"files/hooks/plan-template-block.tmpl", "tmpl", "HookPlanTemplateBlock", func() []byte { return read("files/hooks/plan-template-block.tmpl") }},
	{"files/hooks/plan-agent-write.tmpl", "tmpl", "HookPlanAgentWrite", func() []byte { return read("files/hooks/plan-agent-write.tmpl") }},
	{"files/hooks/docs-placement.tmpl", "tmpl", "HookDocsPlacement", func() []byte { return read("files/hooks/docs-placement.tmpl") }},
	{"files/hooks/docs-where.tmpl", "tmpl", "HookDocsWhere", func() []byte { return read("files/hooks/docs-where.tmpl") }},
	{"files/hooks/docs-retired.tmpl", "tmpl", "HookDocsRetired", func() []byte { return read("files/hooks/docs-retired.tmpl") }},
	{"files/hooks/memory-note-path.tmpl", "tmpl", "HookMemoryNotePath", func() []byte { return read("files/hooks/memory-note-path.tmpl") }},
	{"files/hooks/memory-store-name.tmpl", "tmpl", "HookMemoryStoreName", func() []byte { return read("files/hooks/memory-store-name.tmpl") }},
	{"files/hooks/review-fix-head.txt", "txt", "HookReviewFixHead", func() []byte { return read("files/hooks/review-fix-head.txt") }},
	{"files/hooks/review-no-line.txt", "txt", "HookReviewNoLine", func() []byte { return read("files/hooks/review-no-line.txt") }},
	{"files/memory/prompts/replacement-unavailable.txt", "txt", "MemoryReplacementUnavailable", func() []byte { return read("files/memory/prompts/replacement-unavailable.txt") }},
	{"files/hooks/lane-queue-line.tmpl", "tmpl", "HookLaneQueueLine", func() []byte { return read("files/hooks/lane-queue-line.tmpl") }},
	{"files/hooks/lane-queue-more.tmpl", "tmpl", "HookLaneQueueMore", func() []byte { return read("files/hooks/lane-queue-more.tmpl") }},
	{"files/hooks/lane-merged.tmpl", "tmpl", "HookLaneMerged", func() []byte { return read("files/hooks/lane-merged.tmpl") }},
	{"files/brief/brief.tmpl", "tmpl", "BriefBody", func() []byte { return read("files/brief/brief.tmpl") }},
	{"files/brief/review-loop.txt", "txt", "ReviewLoop", func() []byte { return read("files/brief/review-loop.txt") }},
	{"files/brief/loop-planner.tmpl", "tmpl", "LoopPlanner", func() []byte { return read("files/brief/loop-planner.tmpl") }},
	{"files/brief/review-neutral.txt", "txt", "ReviewNeutral", func() []byte { return read("files/brief/review-neutral.txt") }},
	{"files/brief/review-adversarial.tmpl", "tmpl", "ReviewAdversarial", func() []byte { return read("files/brief/review-adversarial.tmpl") }},
	{"files/brief/takeover.tmpl", "tmpl", "TakeoverBrief", func() []byte { return read("files/brief/takeover.tmpl") }},
	{"files/brief/explore.tmpl", "tmpl", "ExplorePrompt", func() []byte { return read("files/brief/explore.tmpl") }},
	{"files/safety/message.tmpl", "tmpl", "SafetyMessage", func() []byte { return read("files/safety/message.tmpl") }},
	{"files/safety/no-ask.tmpl", "tmpl", "SafetyNoAsk", func() []byte { return read("files/safety/no-ask.tmpl") }},
	{"files/safety/arg-missing.tmpl", "tmpl", "SafetyArgMissing", func() []byte { return read("files/safety/arg-missing.tmpl") }},
	{"files/safety/owner-route.txt", "txt", "SafetyOwnerRoute", func() []byte { return read("files/safety/owner-route.txt") }},
	{"files/safety/attribution-what.txt", "txt", "SafetyAttributionWhat", func() []byte { return read("files/safety/attribution-what.txt") }},
	{"files/safety/attribution-hint.txt", "txt", "SafetyAttributionHint", func() []byte { return read("files/safety/attribution-hint.txt") }},
	{"files/safety/deny-what.txt", "txt", "SafetyDenyWhat", func() []byte { return read("files/safety/deny-what.txt") }},
	{"files/safety/deny-hint.txt", "txt", "SafetyDenyHint", func() []byte { return read("files/safety/deny-hint.txt") }},
	{"files/safety/sudo-what.txt", "txt", "SafetySudoWhat", func() []byte { return read("files/safety/sudo-what.txt") }},
	{"files/safety/sudo-hint.txt", "txt", "SafetySudoHint", func() []byte { return read("files/safety/sudo-hint.txt") }},
	{"files/safety/protected-what.txt", "txt", "SafetyProtectedWhat", func() []byte { return read("files/safety/protected-what.txt") }},
	{"files/safety/protected-hint.txt", "txt", "SafetyProtectedHint", func() []byte { return read("files/safety/protected-hint.txt") }},
	{"files/safety/interpreter-what.txt", "txt", "SafetyInterpreterWhat", func() []byte { return read("files/safety/interpreter-what.txt") }},
	{"files/safety/interpreter-hint.txt", "txt", "SafetyInterpreterHint", func() []byte { return read("files/safety/interpreter-hint.txt") }},
	{"files/safety/path-unresolved.txt", "txt", "SafetyPathUnresolved", func() []byte { return read("files/safety/path-unresolved.txt") }},
	{"files/rtk/note.tmpl", "tmpl", "RtkNote", func() []byte { return read("files/rtk/note.tmpl") }},
	{"files/rtk/shadow-none.tmpl", "tmpl", "RtkShadowNone", func() []byte { return read("files/rtk/shadow-none.tmpl") }},
	{"files/rtk/shadow-found.tmpl", "tmpl", "RtkShadowFound", func() []byte { return read("files/rtk/shadow-found.tmpl") }},
	{"files/rtk/claude-print-hint.txt", "txt", "ClaudePrintHint", func() []byte { return read("files/rtk/claude-print-hint.txt") }},
	{"files/copilot/deny-feedback.txt", "txt", "CopilotDenyFeedback", func() []byte { return read("files/copilot/deny-feedback.txt") }},
	{"files/claude/science.toml", "toml", "ScienceConfig", ScienceConfig},
	{"files/clipboard/wl-paste-claude.sh", "shell", "WLPasteProxy", WLPasteProxy},
	{"files/databricks/templates/claude.json", "json", "DatabricksClaudeTemplate", DatabricksClaudeTemplate},
	{"files/databricks/templates/codex.toml", "toml", "DatabricksCodexTemplate", DatabricksCodexTemplate},
	{"files/setup/tmux.conf", "conf", "TmuxConf", TmuxConf},
	{"files/ui/vscode/keybindings.template.json", "jsonc", "VSCodeKeybindingsTemplate", VSCodeKeybindingsTemplate},
	{"files/ui/vscode/machine-settings.template.json", "jsonc", "VSCodeMachineSettingsTemplate", VSCodeMachineSettingsTemplate},
	{"files/pi/guard/index.ts", "typescript", "PiGuardSource", func() []byte { return []byte(PiGuardSource()) }},
}

// readers maps each exported production accessor to the embedded paths its body reads.
func readers(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, f := range parseProduction(t) {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			out[fn.Name.Name] = append(out[fn.Name.Name], readPaths(fn)...)
		}
	}
	return out
}

// readerDirs maps a template renderer to the directory its first argument is relative to; read takes full paths.
var readerDirs = map[string]string{
	"read": "", "renderMemory": "files/memory/prompts/", "renderAgent": "files/agent/", "renderHook": "files/hooks/", "renderBriefText": "files/brief/", "renderSafety": "files/safety/", "renderRtk": "files/rtk/",
}

func readPaths(fn *ast.FuncDecl) []string {
	var paths []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 1 {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		lit, isLit := call.Args[0].(*ast.BasicLit)
		if !ok || !isLit || lit.Kind != token.STRING {
			return true
		}
		if dir, known := readerDirs[id.Name]; known {
			p, _ := strconv.Unquote(lit.Value)
			paths = append(paths, dir+p)
		}
		return true
	})
	return paths
}

// parseProduction parses this package's non-test Go files.
func parseProduction(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func TestInventoryClosed(t *testing.T) {
	var embedded []string
	err := fs.WalkDir(files, "files", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			embedded = append(embedded, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, e := range inventory {
		if listed[e.path] {
			t.Errorf("%s listed twice", e.path)
		}
		listed[e.path] = true
	}
	for _, p := range embedded {
		if !listed[p] {
			t.Errorf("embedded %s is not in the inventory", p)
		}
		delete(listed, p)
	}
	for _, p := range slices.Sorted(maps.Keys(listed)) {
		t.Errorf("inventory lists %s, which is not embedded", p)
	}
}

func TestInventoryOneReaderEach(t *testing.T) {
	byPath := map[string][]string{}
	for name, paths := range readers(t) {
		for _, p := range paths {
			byPath[p] = append(byPath[p], name)
		}
	}
	for _, e := range inventory {
		if got := byPath[e.path]; len(got) != 1 || got[0] != e.reader {
			t.Errorf("%s: readers %v, want exactly [%s]", e.path, got, e.reader)
		}
		want, err := files.ReadFile(e.path)
		if err != nil || !inventoryBytesMatch(e.read, e.kind, e.reader, want) {
			t.Errorf("%s: %s() does not return the embedded bytes (%v)", e.path, e.reader, err)
		}
		delete(byPath, e.path)
	}
	for _, p := range slices.Sorted(maps.Keys(byPath)) {
		t.Errorf("%v read %s, which is not in the inventory", byPath[p], p)
	}
}

func TestReadersReturnFreshCopies(t *testing.T) {
	for _, e := range inventory {
		if e.read == nil {
			continue
		}
		b := e.read()
		want := bytes.Clone(b)
		clear(b)
		if !bytes.Equal(e.read(), want) {
			t.Errorf("%s: mutating one result changed the next", e.reader)
		}
	}
}

// TestAccessorsUsed requires every exported accessor to have a caller outside this package, tests included.
func TestAccessorsUsed(t *testing.T) {
	used := map[string]bool{}
	for _, root := range []string{"..", "../../cmd"} {
		walkUses(t, root, used)
	}
	for name := range readers(t) {
		if !used[name] {
			t.Errorf("assets.%s has no caller outside internal/assets", name)
		}
	}
}

func walkUses(t *testing.T, root string, used map[string]bool) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || p == filepath.Join("..", "assets")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			collectUses(t, p, used)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func collectUses(t *testing.T, path string, used map[string]bool) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(src, []byte(importPath)) {
		return
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	local := importName(f)
	if local == "" {
		return
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
				used[sel.Sel.Name] = true
			}
		}
		return true
	})
}

// importName is the identifier a file binds the assets import to, or "" when it does not import it by name.
func importName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p != importPath {
			continue
		}
		if imp.Name == nil {
			return "assets"
		}
		if n := imp.Name.Name; n != "_" && n != "." {
			return n
		}
	}
	return ""
}
