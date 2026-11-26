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
	{"catalog/ranks.json", "json", "RankTable", RankTable},
	{"review/autofix/briefs/fix.tmpl", "template", "AutofixBrief", nil},
	{"review/bench/briefs/fix.tmpl", "template", "BenchFixBrief", nil},
	{"review/bench/briefs/review.tmpl", "template", "BenchReviewBrief", nil},
	{"review/bench/no-diff.txt", "text", "BenchNoDiff", func() []byte { return []byte(BenchNoDiff() + "\n") }},
	{"tools/search-description.txt", "text", "WebSearchDescription", func() []byte { return []byte(WebSearchDescription() + "\n") }},
	{"tools/extract-description.txt", "text", "WebExtractDescription", func() []byte { return []byte(WebExtractDescription() + "\n") }},
	{"tools/search-query.txt", "text", "WebSearchQueryDescription", func() []byte { return []byte(WebSearchQueryDescription() + "\n") }},
	{"tools/search-max-results.txt", "text", "WebSearchMaxResultsDescription", func() []byte { return []byte(WebSearchMaxResultsDescription() + "\n") }},
	{"tools/extract-urls.txt", "text", "WebExtractURLsDescription", func() []byte { return []byte(WebExtractURLsDescription() + "\n") }},
	{"tools/proxy-search-description.txt", "text", "ProxySearchDescription", func() []byte { return []byte(ProxySearchDescription() + "\n") }},
	{"tools/proxy-search-schema.json", "json", "ProxySearchSchema", func() []byte { return []byte(ProxySearchSchema() + "\n") }},
	{"tools/databricks-smoke.txt", "text", "DatabricksSmokePrompt", func() []byte { return []byte(DatabricksSmokePrompt() + "\n") }},
	{"tools/databricks-probe.txt", "text", "DatabricksProbePrompt", func() []byte { return []byte(DatabricksProbePrompt() + "\n") }},
	{"tools/databricks-tool.txt", "text", "DatabricksToolDescription", func() []byte { return []byte(DatabricksToolDescription() + "\n") }},
	{"tools/preflight-pong.txt", "text", "PreflightPongPrompt", func() []byte { return []byte(PreflightPongPrompt() + "\n") }},
	{"providers/defaults.json", "json", "ProviderDefaults", ProviderDefaults},
	{"models/catalog.json", "json", "ModelCatalog", ModelCatalog},
	{"router/table.json", "json", "RouteTable", RouteTable},
	{"router/jev-tier.json", "json", "JevTier", JevTier},
	{"proxy/teams.json", "json", "ProxyTeams", ProxyTeams},
	{"shell/guards.json", "json", "ShellGuards", ShellGuards},
	{"agentconfig/agents.json", "json", "AgentDefaults", AgentDefaults},
	{"agentconfig/overlay.schema.json", "json", "OverlaySchemaSnapshot", OverlaySchemaSnapshot},
	{"agentconfig/template/opencode.json", "json", "OpencodeConfigTemplate", OpencodeConfigTemplate},
	{"agentconfig/template/opencode-tui.json", "json", "OpencodeTUITemplate", OpencodeTUITemplate},
	{"settings/automode.json", "json", "SettingsAutoMode", SettingsAutoMode},
	{"settings/template.json", "json", "SettingsTemplate", SettingsTemplate},
	{"settings/claude-code-settings.schema.json", "json", "SettingsSchema", SettingsSchema},
	{"review/source/retired_words.json", "json", "RetiredWords", RetiredWords},
	{"ui/theme/default_config.json", "json", "ThemeDefaultConfig", ThemeDefaultConfig},
	{"ui/theme/default_palettes.json", "json", "ThemeDefaultPalettes", ThemeDefaultPalettes},
	{"workflows/blocks/stepof.js", "js", "WorkflowStepOfSource", func() []byte { return []byte(WorkflowStepOfSource()) }},
	{"workflows/blocks/stages.js", "js", "WorkflowStagesSource", func() []byte { return []byte(WorkflowStagesSource()) }},
	{"workflows/prompts/analyzer.js", "js", "WorkflowAnalyzerPromptsSource", func() []byte { return []byte(WorkflowAnalyzerPromptsSource()) }},
	{"workflows/prompts/engineer.js", "js", "WorkflowEngineerPromptsSource", func() []byte { return []byte(WorkflowEngineerPromptsSource()) }},
	{"workflows/prompts/evaluator.js", "js", "WorkflowEvaluatorPromptsSource", func() []byte { return []byte(WorkflowEvaluatorPromptsSource()) }},
	{"workflows/prompts/workers.js", "js", "WorkflowWorkersPromptsSource", func() []byte { return []byte(WorkflowWorkersPromptsSource()) }},
	{"workflows/prompts/preflight.js", "js", "WorkflowPreflightPromptsSource", func() []byte { return []byte(WorkflowPreflightPromptsSource()) }},
	{"workflows/prompts/looper.js", "js", "WorkflowLooperPromptsSource", func() []byte { return []byte(WorkflowLooperPromptsSource()) }},
	{"workflows/skeletons/note.js", "js", "WorkflowSkeletonNote", func() []byte { return []byte(WorkflowSkeletonNote()) }},
	{"workflows/skeletons/pipeline.js.tmpl", "js", "WorkflowPipelineSkeleton", func() []byte { return []byte(WorkflowPipelineSkeleton("{{.Name}}")) }},
	{"workflows/skeletons/parallel.js.tmpl", "js", "WorkflowParallelSkeleton", func() []byte { return []byte(WorkflowParallelSkeleton("{{.Name}}")) }},
	{"workflows/skeletons/evaluator.js.tmpl", "js", "WorkflowEvaluatorSkeleton", func() []byte { return []byte(WorkflowEvaluatorSkeleton("{{.Name}}")) }},
	{"workflows/skeletons/orchestrator.js.tmpl", "js", "WorkflowOrchestratorSkeleton", func() []byte { return []byte(WorkflowOrchestratorSkeleton("{{.Name}}")) }},
	{"memory/templates/activeContext.md", "md", "MemoryActiveContextSeed", func() []byte { return []byte(MemoryActiveContextSeed()) }},
	{"memory/templates/conventions.md", "md", "MemoryConventionsSeed", func() []byte { return []byte(MemoryConventionsSeed()) }},
	{"memory/templates/decisionLog.md", "md", "MemoryDecisionLogSeed", func() []byte { return []byte(MemoryDecisionLogSeed()) }},
	{"memory/templates/progress.md", "md", "MemoryProgressSeed", func() []byte { return []byte(MemoryProgressSeed()) }},
	{"memory/templates/sessionHistory.md", "md", "MemorySessionHistorySeed", func() []byte { return []byte(MemorySessionHistorySeed()) }},
	{"memory/prompts/dream.md", "md", "MemoryDreamProcedure", func() []byte { return read("memory/prompts/dream.md") }},
	{"memory/prompts/init.md", "md", "MemoryInitProcedure", func() []byte { return read("memory/prompts/init.md") }},
	{"memory/prompts/promote.md", "md", "MemoryPromoteProcedure", func() []byte { return read("memory/prompts/promote.md") }},
	{"memory/prompts/update.md", "md", "MemoryUpdateProcedure", func() []byte { return read("memory/prompts/update.md") }},
	{"memory/prompts/update-opening.tmpl", "tmpl", "MemoryUpdateWorker", func() []byte { return read("memory/prompts/update-opening.tmpl") }},
	{"memory/prompts/dream-opening.tmpl", "tmpl", "MemoryDreamWorker", func() []byte { return read("memory/prompts/dream-opening.tmpl") }},
	{"memory/prompts/historical.txt", "txt", "MemoryHistoricalRule", func() []byte { return read("memory/prompts/historical.txt") }},
	{"memory/prompts/update-worker.tmpl", "tmpl", "MemoryUpdatePrompt", func() []byte { return read("memory/prompts/update-worker.tmpl") }},
	{"memory/prompts/transcript.tmpl", "tmpl", "MemoryTranscriptPointer,TranscriptHandoff", func() []byte { return read("memory/prompts/transcript.tmpl") }},
	{"memory/prompts/git-activity.tmpl", "tmpl", "MemoryGitActivity", func() []byte { return read("memory/prompts/git-activity.tmpl") }},
	{"memory/prompts/session-tags.tmpl", "tmpl", "MemorySessionTags", func() []byte { return read("memory/prompts/session-tags.tmpl") }},
	{"memory/prompts/dream-worker.tmpl", "tmpl", "MemoryDreamPrompt", func() []byte { return read("memory/prompts/dream-worker.tmpl") }},
	{"memory/prompts/dream-unchanged.txt", "txt", "MemoryDreamUnchanged", func() []byte { return read("memory/prompts/dream-unchanged.txt") }},
	{"memory/prompts/misc.tmpl", "tmpl", "MemoryFiledByGo,MemoryHandoffRequest,MemoryHomeOverflow,MemoryLayoutRow,MemoryMoreEntries,MemoryOpenTasks,MemoryPreviousSession,MemoryRetryNote,MemoryScaffoldNote,MemorySessionDigest,MemoryUnresolvedEntries,MemoryUpdateRecovery", func() []byte { return read("memory/prompts/misc.tmpl") }},
	{"memory/prompts/rtk-misses.tmpl", "tmpl", "MemoryRTKMisses", func() []byte { return read("memory/prompts/rtk-misses.tmpl") }},
	{"memory/prompts/index-hint.txt", "txt", "MemoryIndexHint", func() []byte { return read("memory/prompts/index-hint.txt") }},
	{"memory/prompts/block-begin.txt", "txt", "MemoryBlockBegin", func() []byte { return read("memory/prompts/block-begin.txt") }},
	{"memory/prompts/bank.tmpl", "tmpl", "MemoryBankHead,MemoryBankHeading,MemoryBankLoadNote", func() []byte { return read("memory/prompts/bank.tmpl") }},
	{"memory/prompts/catalog.tmpl", "tmpl", "MemoryCatalogBank,MemoryCatalogHeader,MemoryCatalogPointer", func() []byte { return read("memory/prompts/catalog.tmpl") }},
	{"memory/prompts/home-index.txt", "txt", "MemoryHomeIndexHeading", func() []byte { return read("memory/prompts/home-index.txt") }},
	{"memory/prompts/knowledge.txt", "txt", "MemoryKnowledgeHeading", func() []byte { return read("memory/prompts/knowledge.txt") }},
	{"memory/folder-roles.json", "json", "MemoryFolderRoles", func() []byte { return read("memory/folder-roles.json") }},
	{"memory/prompts/layout.txt", "txt", "MemoryLayoutHeading", func() []byte { return read("memory/prompts/layout.txt") }},
	{"memory/prompts/native.txt", "txt", "MemoryNativeHeading", func() []byte { return read("memory/prompts/native.txt") }},
	{"memory/prompts/native.tmpl", "tmpl", "MemoryNativeContext,MemoryNativeFolder,MemoryNativeUnparsed", func() []byte { return read("memory/prompts/native.tmpl") }},
	{"memory/prompts/recall.tmpl", "tmpl", "MemoryRecallHeader,MemoryRecallLine", func() []byte { return read("memory/prompts/recall.tmpl") }},
	{"memory/prompts/recall-footer.txt", "txt", "MemoryRecallFooter", func() []byte { return read("memory/prompts/recall-footer.txt") }},
	{"memory/prompts/recall-marker.txt", "txt", "MemoryRecallMarker", func() []byte { return read("memory/prompts/recall-marker.txt") }},
	{"agent/preamble.tmpl", "tmpl", "AgentPreamble", func() []byte { return read("agent/preamble.tmpl") }},
	{"agent/web-via-cli.txt", "txt", "AgentWebViaCLI", func() []byte { return read("agent/web-via-cli.txt") }},
	{"agent/resume.txt", "txt", "AgentResumePrompt", func() []byte { return read("agent/resume.txt") }},
	{"agent/rubric-lead.txt", "txt", "AgentRubricLead", func() []byte { return read("agent/rubric-lead.txt") }},
	{"agent/rubric.tmpl", "tmpl", "AgentRubricBody", func() []byte { return read("agent/rubric.tmpl") }},
	{"agent/misc.tmpl", "tmpl", "AgentHandoff,AgentMidRun,AgentPartial,AgentPersona,AgentReconcile,AgentSchemaInstruction,ContractBlock,PackCoverageWarning", func() []byte { return read("agent/misc.tmpl") }},
	{"agent/recovery.tmpl", "tmpl", "AgentRecovery", func() []byte { return read("agent/recovery.tmpl") }},
	{"agent/pack-prompt.tmpl", "tmpl", "PackPrompt", func() []byte { return read("agent/pack-prompt.tmpl") }},
	{"agent/pack-index.tmpl", "tmpl", "PackIndex", func() []byte { return read("agent/pack-index.tmpl") }},
	{"agent/inspect.tmpl", "tmpl", "InspectAdvisory,InspectNewDecls,InspectNoScope,InspectOutsideModified", func() []byte { return read("agent/inspect.tmpl") }},
	{"agent/tools.tmpl", "tmpl", "ToolsDryRun", func() []byte { return read("agent/tools.tmpl") }},
	{"agent/loop.tmpl", "tmpl", "LoopReportHeader,LoopReportResume,LoopReportSummary", func() []byte { return read("agent/loop.tmpl") }},
	{"agent/pack.tmpl", "tmpl", "PackInventory", func() []byte { return read("agent/pack.tmpl") }},
	{"agent/pack-no-files.txt", "txt", "PackNoFiles", func() []byte { return read("agent/pack-no-files.txt") }},
	{"hooks/simplicity.tmpl", "tmpl", "HookSimplicityHeader,HookSimplicityMore", func() []byte { return read("hooks/simplicity.tmpl") }},
	{"hooks/paths.tmpl", "tmpl", "HookPathDetail,HookPathFailed,HookPathMapped,HookPathsContext", func() []byte { return read("hooks/paths.tmpl") }},
	{"hooks/misc.tmpl", "tmpl", "HookBusyPeer,HookDiverged", func() []byte { return read("hooks/misc.tmpl") }},
	{"hooks/notice.tmpl", "tmpl", "HookFailureClaim,HookFailureLine,HookFailuresMore,HookFailuresWarn,HookFixNow,HookNoticeDivergeWarn,HookNoticeDrainWarn,HookNoticeLine", func() []byte { return read("hooks/notice.tmpl") }},
	{"agent/agy-toolless.txt", "txt", "AgyToolLessPreamble", func() []byte { return read("agent/agy-toolless.txt") }},
	{"agent/stdin-task.txt", "txt", "PiStdinTask", func() []byte { return read("agent/stdin-task.txt") }},
	{"agent/attach.tmpl", "tmpl", "AttachRef,ClaudeAttachImage", func() []byte { return read("agent/attach.tmpl") }},
	{"agent/attach-file.tmpl", "tmpl", "AttachFile", func() []byte { return read("agent/attach-file.tmpl") }},
	{"agent/plan-tasks-head.txt", "txt", "PlanTasksHead", func() []byte { return read("agent/plan-tasks-head.txt") }},
	{"agent/plan-tasks-empty.txt", "txt", "PlanTasksEmpty", func() []byte { return read("agent/plan-tasks-empty.txt") }},
	{"agent/plan-closeout.txt", "txt", "PlanCloseout", func() []byte { return read("agent/plan-closeout.txt") }},
	{"hooks/plan.tmpl", "tmpl", "HookPlanAgentWrite,HookPlanPathChoose,HookPlanPathMoved,HookPlanPathRedirect,HookPlanTemplateBlock", func() []byte { return read("hooks/plan.tmpl") }},
	{"hooks/docs.tmpl", "tmpl", "HookDocsPlacement,HookDocsRetired,HookDocsWhere", func() []byte { return read("hooks/docs.tmpl") }},
	{"hooks/memory.tmpl", "tmpl", "HookMemoryNotePath,HookMemoryStoreName", func() []byte { return read("hooks/memory.tmpl") }},
	{"hooks/review-fix-head.txt", "txt", "HookReviewFixHead", func() []byte { return read("hooks/review-fix-head.txt") }},
	{"hooks/review-no-line.txt", "txt", "HookReviewNoLine", func() []byte { return read("hooks/review-no-line.txt") }},
	{"memory/prompts/replacement-unavailable.txt", "txt", "MemoryReplacementUnavailable", func() []byte { return read("memory/prompts/replacement-unavailable.txt") }},
	{"hooks/lane.tmpl", "tmpl", "HookLaneQueueLine,HookLaneQueueMore", func() []byte { return read("hooks/lane.tmpl") }},
	{"brief/brief.tmpl", "tmpl", "BriefBody", func() []byte { return read("brief/brief.tmpl") }},
	{"brief/review-loop.txt", "txt", "ReviewLoop", func() []byte { return read("brief/review-loop.txt") }},
	{"brief/loop-planner.tmpl", "tmpl", "LoopPlanner", func() []byte { return read("brief/loop-planner.tmpl") }},
	{"brief/review-neutral.txt", "txt", "ReviewNeutral", func() []byte { return read("brief/review-neutral.txt") }},
	{"brief/review-adversarial.tmpl", "tmpl", "ReviewAdversarial", func() []byte { return read("brief/review-adversarial.tmpl") }},
	{"brief/takeover.tmpl", "tmpl", "TakeoverBrief", func() []byte { return read("brief/takeover.tmpl") }},
	{"safety/message.tmpl", "tmpl", "SafetyMessage", func() []byte { return read("safety/message.tmpl") }},
	{"safety/messages.tmpl", "tmpl", "SafetyArgMissing,SafetyNoAsk", func() []byte { return read("safety/messages.tmpl") }},
	{"safety/owner-route.txt", "txt", "SafetyOwnerRoute", func() []byte { return read("safety/owner-route.txt") }},
	{"safety/attribution-what.txt", "txt", "SafetyAttributionWhat", func() []byte { return read("safety/attribution-what.txt") }},
	{"safety/attribution-hint.txt", "txt", "SafetyAttributionHint", func() []byte { return read("safety/attribution-hint.txt") }},
	{"safety/deny-what.txt", "txt", "SafetyDenyWhat", func() []byte { return read("safety/deny-what.txt") }},
	{"safety/deny-hint.txt", "txt", "SafetyDenyHint", func() []byte { return read("safety/deny-hint.txt") }},
	{"safety/sudo-what.txt", "txt", "SafetySudoWhat", func() []byte { return read("safety/sudo-what.txt") }},
	{"safety/sudo-hint.txt", "txt", "SafetySudoHint", func() []byte { return read("safety/sudo-hint.txt") }},
	{"safety/protected-what.txt", "txt", "SafetyProtectedWhat", func() []byte { return read("safety/protected-what.txt") }},
	{"safety/protected-hint.txt", "txt", "SafetyProtectedHint", func() []byte { return read("safety/protected-hint.txt") }},
	{"safety/instruction-what.txt", "txt", "SafetyInstructionWhat", func() []byte { return read("safety/instruction-what.txt") }},
	{"safety/instruction-hint.txt", "txt", "SafetyInstructionHint", func() []byte { return read("safety/instruction-hint.txt") }},
	{"safety/interpreter-what.txt", "txt", "SafetyInterpreterWhat", func() []byte { return read("safety/interpreter-what.txt") }},
	{"safety/interpreter-hint.txt", "txt", "SafetyInterpreterHint", func() []byte { return read("safety/interpreter-hint.txt") }},
	{"safety/path-unresolved.txt", "txt", "SafetyPathUnresolved", func() []byte { return read("safety/path-unresolved.txt") }},
	{"rtk/notes.tmpl", "tmpl", "RtkNote,RtkShadowFound,RtkShadowNone", func() []byte { return read("rtk/notes.tmpl") }},
	{"rtk/claude-print-hint.txt", "txt", "ClaudePrintHint", func() []byte { return read("rtk/claude-print-hint.txt") }},
	{"copilot/deny-feedback.txt", "txt", "CopilotDenyFeedback", func() []byte { return read("copilot/deny-feedback.txt") }},
	{"claude/science.toml", "toml", "ScienceConfig", ScienceConfig},
	{"clipboard/wl-paste-claude.sh", "shell", "WLPasteProxy", WLPasteProxy},
	{"databricks/templates/claude.json", "json", "DatabricksClaudeTemplate", DatabricksClaudeTemplate},
	{"databricks/templates/codex.toml", "toml", "DatabricksCodexTemplate", DatabricksCodexTemplate},
	{"setup/tmux.conf", "conf", "TmuxConf", TmuxConf},
	{"ui/vscode/keybindings.template.json", "jsonc", "VSCodeKeybindingsTemplate", VSCodeKeybindingsTemplate},
	{"ui/vscode/machine-settings.template.json", "jsonc", "VSCodeMachineSettingsTemplate", VSCodeMachineSettingsTemplate},
	{"pi/system-prompt.txt", "txt", "PiSystemPrompt", func() []byte { return read("pi/system-prompt.txt") }},
	{"pi/guard/index.ts", "typescript", "PiGuardSource", func() []byte { return []byte(PiGuardSource()) }},
	{"hooks/claim.tmpl", "tmpl", "HookClaimHowTo", func() []byte { return read("hooks/claim.tmpl") }},
	{"hooks/claim-lane-hint.txt", "txt", "HookClaimLaneHint", func() []byte { return read("hooks/claim-lane-hint.txt") }},
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
		row := PromptRow{Path: e.path, Family: strings.Split(e.path, "/")[0], Accessors: names, Fields: []string{}, Size: len(src)}
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
