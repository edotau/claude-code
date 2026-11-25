package assets

import (
	"maps"
	"path"
	"slices"
	"strings"
	"text/template/parse"
)

// inventory is the closed list of embedded files: a test keeps it equal to files/; doctor --prompts --list prints it.
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
	{"files/router/jev-tier.json", "json", "JevTier", JevTier},
	{"files/proxy/teams.json", "json", "ProxyTeams", ProxyTeams},
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
	{"files/workflows/prompts/preflight.js", "js", "WorkflowPreflightPromptsSource", func() []byte { return []byte(WorkflowPreflightPromptsSource()) }},
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
	{"files/memory/prompts/update.md", "md", "MemoryUpdateProcedure", func() []byte { return read("files/memory/prompts/update.md") }},
	{"files/memory/prompts/update-opening.tmpl", "tmpl", "MemoryUpdateWorker", func() []byte { return read("files/memory/prompts/update-opening.tmpl") }},
	{"files/memory/prompts/dream-opening.tmpl", "tmpl", "MemoryDreamWorker", func() []byte { return read("files/memory/prompts/dream-opening.tmpl") }},
	{"files/memory/prompts/legacy-update-opening.tmpl", "tmpl", "MemoryLegacyUpdateWorker", func() []byte { return read("files/memory/prompts/legacy-update-opening.tmpl") }},
	{"files/memory/prompts/legacy-dream-opening.tmpl", "tmpl", "MemoryLegacyDreamWorker", func() []byte { return read("files/memory/prompts/legacy-dream-opening.tmpl") }},
	{"files/memory/prompts/historical.txt", "txt", "MemoryHistoricalRule", func() []byte { return read("files/memory/prompts/historical.txt") }},
	{"files/memory/prompts/update-worker.tmpl", "tmpl", "MemoryUpdatePrompt", func() []byte { return read("files/memory/prompts/update-worker.tmpl") }},
	{"files/memory/prompts/transcript.tmpl", "tmpl", "MemoryTranscriptPointer,TranscriptHandoff", func() []byte { return read("files/memory/prompts/transcript.tmpl") }},
	{"files/memory/prompts/git-activity.tmpl", "tmpl", "MemoryGitActivity", func() []byte { return read("files/memory/prompts/git-activity.tmpl") }},
	{"files/memory/prompts/session-tags.tmpl", "tmpl", "MemorySessionTags", func() []byte { return read("files/memory/prompts/session-tags.tmpl") }},
	{"files/memory/prompts/dream-worker.tmpl", "tmpl", "MemoryDreamPrompt", func() []byte { return read("files/memory/prompts/dream-worker.tmpl") }},
	{"files/memory/prompts/dream-unchanged.txt", "txt", "MemoryDreamUnchanged", func() []byte { return read("files/memory/prompts/dream-unchanged.txt") }},
	{"files/memory/prompts/misc.tmpl", "tmpl", "MemoryHomeOverflow,MemoryLayoutRow,MemoryMoreEntries,MemoryOpenTasks,MemoryPreviousSession,MemoryScaffoldNote,MemorySessionDigest,MemoryUnresolvedEntries,MemoryUpdateRecovery", func() []byte { return read("files/memory/prompts/misc.tmpl") }},
	{"files/memory/prompts/rtk-misses.tmpl", "tmpl", "MemoryRTKMisses", func() []byte { return read("files/memory/prompts/rtk-misses.tmpl") }},
	{"files/memory/prompts/index-hint.txt", "txt", "MemoryIndexHint", func() []byte { return read("files/memory/prompts/index-hint.txt") }},
	{"files/memory/prompts/block-begin.txt", "txt", "MemoryBlockBegin", func() []byte { return read("files/memory/prompts/block-begin.txt") }},
	{"files/memory/prompts/bank.tmpl", "tmpl", "MemoryBankHead,MemoryBankHeading,MemoryBankLoadNote", func() []byte { return read("files/memory/prompts/bank.tmpl") }},
	{"files/memory/prompts/catalog.tmpl", "tmpl", "MemoryCatalogBank,MemoryCatalogHeader,MemoryCatalogPointer", func() []byte { return read("files/memory/prompts/catalog.tmpl") }},
	{"files/memory/prompts/home-index.txt", "txt", "MemoryHomeIndexHeading", func() []byte { return read("files/memory/prompts/home-index.txt") }},
	{"files/memory/prompts/knowledge.txt", "txt", "MemoryKnowledgeHeading", func() []byte { return read("files/memory/prompts/knowledge.txt") }},
	{"files/memory/folder-roles.json", "json", "MemoryFolderRoles", func() []byte { return read("files/memory/folder-roles.json") }},
	{"files/memory/prompts/layout.txt", "txt", "MemoryLayoutHeading", func() []byte { return read("files/memory/prompts/layout.txt") }},
	{"files/memory/prompts/native.txt", "txt", "MemoryNativeHeading", func() []byte { return read("files/memory/prompts/native.txt") }},
	{"files/memory/prompts/native.tmpl", "tmpl", "MemoryNativeContext,MemoryNativeFolder,MemoryNativeUnparsed", func() []byte { return read("files/memory/prompts/native.tmpl") }},
	{"files/memory/prompts/recall.tmpl", "tmpl", "MemoryRecallHeader,MemoryRecallLine", func() []byte { return read("files/memory/prompts/recall.tmpl") }},
	{"files/memory/prompts/recall-footer.txt", "txt", "MemoryRecallFooter", func() []byte { return read("files/memory/prompts/recall-footer.txt") }},
	{"files/memory/prompts/recall-marker.txt", "txt", "MemoryRecallMarker", func() []byte { return read("files/memory/prompts/recall-marker.txt") }},
	{"files/agent/preamble.tmpl", "tmpl", "AgentPreamble", func() []byte { return read("files/agent/preamble.tmpl") }},
	{"files/agent/web-via-cli.txt", "txt", "AgentWebViaCLI", func() []byte { return read("files/agent/web-via-cli.txt") }},
	{"files/agent/resume.txt", "txt", "AgentResumePrompt", func() []byte { return read("files/agent/resume.txt") }},
	{"files/agent/rubric-lead.txt", "txt", "AgentRubricLead", func() []byte { return read("files/agent/rubric-lead.txt") }},
	{"files/agent/rubric.tmpl", "tmpl", "AgentRubricBody", func() []byte { return read("files/agent/rubric.tmpl") }},
	{"files/agent/misc.tmpl", "tmpl", "AgentHandoff,AgentMidRun,AgentPartial,AgentPersona,AgentReconcile,AgentSchemaInstruction,PackCoverageWarning", func() []byte { return read("files/agent/misc.tmpl") }},
	{"files/agent/recovery.tmpl", "tmpl", "AgentRecovery", func() []byte { return read("files/agent/recovery.tmpl") }},
	{"files/agent/pack-prompt.tmpl", "tmpl", "PackPrompt", func() []byte { return read("files/agent/pack-prompt.tmpl") }},
	{"files/agent/pack-index.tmpl", "tmpl", "PackIndex", func() []byte { return read("files/agent/pack-index.tmpl") }},
	{"files/agent/inspect.tmpl", "tmpl", "InspectAdvisory,InspectNewDecls,InspectNoScope,InspectOutsideModified", func() []byte { return read("files/agent/inspect.tmpl") }},
	{"files/agent/tools.tmpl", "tmpl", "ToolsDryRun", func() []byte { return read("files/agent/tools.tmpl") }},
	{"files/agent/loop.tmpl", "tmpl", "LoopReportHeader,LoopReportResume,LoopReportSummary", func() []byte { return read("files/agent/loop.tmpl") }},
	{"files/agent/pack.tmpl", "tmpl", "PackInventory", func() []byte { return read("files/agent/pack.tmpl") }},
	{"files/agent/pack-no-files.txt", "txt", "PackNoFiles", func() []byte { return read("files/agent/pack-no-files.txt") }},
	{"files/hooks/simplicity.tmpl", "tmpl", "HookSimplicityHeader,HookSimplicityMore", func() []byte { return read("files/hooks/simplicity.tmpl") }},
	{"files/hooks/paths.tmpl", "tmpl", "HookPathDetail,HookPathFailed,HookPathMapped,HookPathsContext", func() []byte { return read("files/hooks/paths.tmpl") }},
	{"files/hooks/misc.tmpl", "tmpl", "HookBusyPeer,HookDiverged", func() []byte { return read("files/hooks/misc.tmpl") }},
	{"files/hooks/notice.tmpl", "tmpl", "HookNoticeDivergeWarn,HookNoticeDrainWarn,HookNoticeLine", func() []byte { return read("files/hooks/notice.tmpl") }},
	{"files/agent/agy-toolless.txt", "txt", "AgyToolLessPreamble", func() []byte { return read("files/agent/agy-toolless.txt") }},
	{"files/agent/stdin-task.txt", "txt", "PiStdinTask", func() []byte { return read("files/agent/stdin-task.txt") }},
	{"files/agent/attach.tmpl", "tmpl", "AttachRef,ClaudeAttachImage", func() []byte { return read("files/agent/attach.tmpl") }},
	{"files/agent/attach-file.tmpl", "tmpl", "AttachFile", func() []byte { return read("files/agent/attach-file.tmpl") }},
	{"files/agent/plan-tasks-head.txt", "txt", "PlanTasksHead", func() []byte { return read("files/agent/plan-tasks-head.txt") }},
	{"files/agent/plan-tasks-empty.txt", "txt", "PlanTasksEmpty", func() []byte { return read("files/agent/plan-tasks-empty.txt") }},
	{"files/agent/plan-closeout.txt", "txt", "PlanCloseout", func() []byte { return read("files/agent/plan-closeout.txt") }},
	{"files/hooks/plan.tmpl", "tmpl", "HookPlanAgentWrite,HookPlanPathChoose,HookPlanPathMoved,HookPlanPathRedirect,HookPlanTemplateBlock", func() []byte { return read("files/hooks/plan.tmpl") }},
	{"files/hooks/docs.tmpl", "tmpl", "HookDocsPlacement,HookDocsRetired,HookDocsWhere", func() []byte { return read("files/hooks/docs.tmpl") }},
	{"files/hooks/memory.tmpl", "tmpl", "HookMemoryNotePath,HookMemoryStoreName", func() []byte { return read("files/hooks/memory.tmpl") }},
	{"files/hooks/review-fix-head.txt", "txt", "HookReviewFixHead", func() []byte { return read("files/hooks/review-fix-head.txt") }},
	{"files/hooks/review-no-line.txt", "txt", "HookReviewNoLine", func() []byte { return read("files/hooks/review-no-line.txt") }},
	{"files/memory/prompts/replacement-unavailable.txt", "txt", "MemoryReplacementUnavailable", func() []byte { return read("files/memory/prompts/replacement-unavailable.txt") }},
	{"files/hooks/lane.tmpl", "tmpl", "HookLaneLanded,HookLaneQueueLine,HookLaneQueueMore", func() []byte { return read("files/hooks/lane.tmpl") }},
	{"files/brief/brief.tmpl", "tmpl", "BriefBody", func() []byte { return read("files/brief/brief.tmpl") }},
	{"files/brief/review-loop.txt", "txt", "ReviewLoop", func() []byte { return read("files/brief/review-loop.txt") }},
	{"files/brief/loop-planner.tmpl", "tmpl", "LoopPlanner", func() []byte { return read("files/brief/loop-planner.tmpl") }},
	{"files/brief/review-neutral.txt", "txt", "ReviewNeutral", func() []byte { return read("files/brief/review-neutral.txt") }},
	{"files/brief/review-adversarial.tmpl", "tmpl", "ReviewAdversarial", func() []byte { return read("files/brief/review-adversarial.tmpl") }},
	{"files/brief/takeover.tmpl", "tmpl", "TakeoverBrief", func() []byte { return read("files/brief/takeover.tmpl") }},
	{"files/brief/explore.tmpl", "tmpl", "ExplorePrompt", func() []byte { return read("files/brief/explore.tmpl") }},
	{"files/safety/message.tmpl", "tmpl", "SafetyMessage", func() []byte { return read("files/safety/message.tmpl") }},
	{"files/safety/messages.tmpl", "tmpl", "SafetyArgMissing,SafetyNoAsk", func() []byte { return read("files/safety/messages.tmpl") }},
	{"files/safety/owner-route.txt", "txt", "SafetyOwnerRoute", func() []byte { return read("files/safety/owner-route.txt") }},
	{"files/safety/attribution-what.txt", "txt", "SafetyAttributionWhat", func() []byte { return read("files/safety/attribution-what.txt") }},
	{"files/safety/attribution-hint.txt", "txt", "SafetyAttributionHint", func() []byte { return read("files/safety/attribution-hint.txt") }},
	{"files/safety/deny-what.txt", "txt", "SafetyDenyWhat", func() []byte { return read("files/safety/deny-what.txt") }},
	{"files/safety/deny-hint.txt", "txt", "SafetyDenyHint", func() []byte { return read("files/safety/deny-hint.txt") }},
	{"files/safety/sudo-what.txt", "txt", "SafetySudoWhat", func() []byte { return read("files/safety/sudo-what.txt") }},
	{"files/safety/sudo-hint.txt", "txt", "SafetySudoHint", func() []byte { return read("files/safety/sudo-hint.txt") }},
	{"files/safety/protected-what.txt", "txt", "SafetyProtectedWhat", func() []byte { return read("files/safety/protected-what.txt") }},
	{"files/safety/protected-hint.txt", "txt", "SafetyProtectedHint", func() []byte { return read("files/safety/protected-hint.txt") }},
	{"files/safety/instruction-what.txt", "txt", "SafetyInstructionWhat", func() []byte { return read("files/safety/instruction-what.txt") }},
	{"files/safety/instruction-hint.txt", "txt", "SafetyInstructionHint", func() []byte { return read("files/safety/instruction-hint.txt") }},
	{"files/safety/interpreter-what.txt", "txt", "SafetyInterpreterWhat", func() []byte { return read("files/safety/interpreter-what.txt") }},
	{"files/safety/interpreter-hint.txt", "txt", "SafetyInterpreterHint", func() []byte { return read("files/safety/interpreter-hint.txt") }},
	{"files/safety/path-unresolved.txt", "txt", "SafetyPathUnresolved", func() []byte { return read("files/safety/path-unresolved.txt") }},
	{"files/rtk/notes.tmpl", "tmpl", "RtkNote,RtkShadowFound,RtkShadowNone", func() []byte { return read("files/rtk/notes.tmpl") }},
	{"files/rtk/claude-print-hint.txt", "txt", "ClaudePrintHint", func() []byte { return read("files/rtk/claude-print-hint.txt") }},
	{"files/copilot/deny-feedback.txt", "txt", "CopilotDenyFeedback", func() []byte { return read("files/copilot/deny-feedback.txt") }},
	{"files/claude/science.toml", "toml", "ScienceConfig", ScienceConfig},
	{"files/clipboard/wl-paste-claude.sh", "shell", "WLPasteProxy", WLPasteProxy},
	{"files/databricks/templates/claude.json", "json", "DatabricksClaudeTemplate", DatabricksClaudeTemplate},
	{"files/databricks/templates/codex.toml", "toml", "DatabricksCodexTemplate", DatabricksCodexTemplate},
	{"files/setup/tmux.conf", "conf", "TmuxConf", TmuxConf},
	{"files/ui/vscode/keybindings.template.json", "jsonc", "VSCodeKeybindingsTemplate", VSCodeKeybindingsTemplate},
	{"files/ui/vscode/machine-settings.template.json", "jsonc", "VSCodeMachineSettingsTemplate", VSCodeMachineSettingsTemplate},
	{"files/pi/system-prompt.txt", "txt", "PiSystemPrompt", func() []byte { return read("files/pi/system-prompt.txt") }},
	{"files/pi/guard/index.ts", "typescript", "PiGuardSource", func() []byte { return []byte(PiGuardSource()) }},
	{"files/hooks/claim.tmpl", "tmpl", "HookClaimHowTo,HookClaimStageHint", func() []byte { return read("files/hooks/claim.tmpl") }},
	{"files/hooks/claim-commit-hint.txt", "txt", "HookClaimCommitHint", func() []byte { return read("files/hooks/claim-commit-hint.txt") }},
	{"files/hooks/claim-bare-hint.txt", "txt", "HookClaimBareHint", func() []byte { return read("files/hooks/claim-bare-hint.txt") }},
	{"files/hooks/claim-nest-hint.txt", "txt", "HookClaimNestHint", func() []byte { return read("files/hooks/claim-nest-hint.txt") }},
	{"files/hooks/claim-lane-hint.txt", "txt", "HookClaimLaneHint", func() []byte { return read("files/hooks/claim-lane-hint.txt") }},
}

// PromptRow is one inventory row: an embedded file, its accessor, its template inputs and size.
type PromptRow struct {
	Path      string   `json:"path"`
	Family    string   `json:"family"`
	Accessors []string `json:"accessors"`
	Fields    []string `json:"fields"`
	Size      int      `json:"size"`
}

// Inventory lists every embedded file; Fields are the template's named inputs ("." for the bare value), empty for text.
func Inventory() []PromptRow {
	rows := make([]PromptRow, 0, len(inventory))
	for _, e := range inventory {
		src := read(e.path)
		names := slices.DeleteFunc(strings.Split(e.reader, ","), func(n string) bool { return n == "" })
		row := PromptRow{Path: e.path, Family: strings.Split(e.path, "/")[1], Accessors: names, Fields: []string{}, Size: len(src)}
		if strings.HasSuffix(e.path, ".tmpl") {
			row.Fields = append(row.Fields, templateFields(path.Base(e.path), src)...)
		}
		rows = append(rows, row)
	}
	return rows
}

// templateFuncs names the functions review templates call; the parser needs a non-nil value, only fields are read.
var templateFuncs = map[string]any{"sh": strings.TrimSpace, "not": strings.TrimSpace, "and": strings.TrimSpace,
	"or": strings.TrimSpace, "eq": strings.TrimSpace, "ne": strings.TrimSpace, "lt": strings.TrimSpace}

// templateFields is the sorted set of field names a template reads, "." for a bare {{.}}.
func templateFields(name string, src []byte) []string {
	trees, err := parse.Parse(name, string(src), "", "", templateFuncs)
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	set := map[string]bool{}
	var walk func(n parse.Node)
	walk = func(n parse.Node) {
		switch n := n.(type) {
		case *parse.FieldNode:
			set[strings.Join(n.Ident, ".")] = true
		case *parse.DotNode:
			set["."] = true
		}
		for _, c := range templateChildren(n) {
			walk(c)
		}
	}
	for _, tree := range trees {
		walk(tree.Root)
	}
	return slices.Sorted(maps.Keys(set))
}

// templateChildren is the nodes directly under n that can hold a field.
func templateChildren(n parse.Node) []parse.Node {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		return n.Nodes
	case *parse.ActionNode:
		return []parse.Node{n.Pipe}
	case *parse.IfNode:
		return []parse.Node{n.Pipe, n.List, n.ElseList}
	case *parse.RangeNode:
		return []parse.Node{n.Pipe, n.List, n.ElseList}
	case *parse.PipeNode:
		var out []parse.Node
		for _, c := range n.Cmds {
			out = append(out, c.Args...)
		}
		return out
	}
	return nil
}
