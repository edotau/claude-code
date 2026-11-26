package assets

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
)

type MemoryUpdateInput struct{ Worker, Procedure, Historical, Digest, Git, Tags string }

func MemoryActiveContextSeed() string { return string(read("memory/templates/activeContext.md")) }

func MemoryConventionsSeed() string { return string(read("memory/templates/conventions.md")) }

func MemoryDecisionLogSeed() string { return string(read("memory/templates/decisionLog.md")) }

func MemoryProgressSeed() string { return string(read("memory/templates/progress.md")) }


func MemorySessionHistorySeed() string {
	return string(read("memory/templates/sessionHistory.md"))
}

func MemoryDreamProcedure() string { return memoryProcedure(read("memory/prompts/dream.md")) }

func MemoryInitProcedure() string { return memoryProcedure(read("memory/prompts/init.md")) }

func MemoryPromoteProcedure() string { return memoryProcedure(read("memory/prompts/promote.md")) }

// MemoryReviewProcedure is dream's Phase 6, so `feedback list --rank` and dream share one scoring rubric.
func MemoryReviewProcedure() string {
	dream := MemoryDreamProcedure()
	start := strings.Index(dream, "## Phase 6: Reflection")
	if start < 0 {
		panic("assets: dream.md lacks its Phase 6 reflection")
	}
	body, _, _ := strings.Cut(dream[start:], "## Phase 7")
	return strings.TrimSpace(body)
}

func MemoryUpdateProcedure() string { return memoryProcedure(read("memory/prompts/update.md")) }

func MemoryUpdateWorker(bank string) string {
	return renderMemory("update-opening.tmpl", tmplFields{"Bank": bank})
}

func MemoryDreamWorker(bank string) string {
	return renderMemory("dream-opening.tmpl", tmplFields{"Bank": bank})
}

func MemoryHistoricalRule() string { return string(read("memory/prompts/historical.txt")) }

func MemoryUpdatePrompt(in MemoryUpdateInput) string {
	return renderMemory("update-worker.tmpl", tmplFields{
		"Worker": in.Worker, "Procedure": in.Procedure, "Historical": in.Historical,
		"Digest": in.Digest, "Git": in.Git, "Tags": in.Tags,
	})
}

func MemoryTranscriptPointer(path string) string {
	return renderMemory("transcript/pointer", tmplFields{"Path": path})
}

func MemorySessionDigest(path, body string) string {
	return renderMemory("misc/session-digest", tmplFields{"Path": path, "Body": body})
}

func MemoryGitActivity(commits, files string) string {
	return renderMemory("git-activity.tmpl", tmplFields{"Commits": commits, "Files": files})
}

func MemorySessionTags(lines []string) string {
	return renderMemory("session-tags.tmpl", lines)
}

func MemoryDreamPrompt(worker, changed, procedure string) string {
	return renderMemory("dream-worker.tmpl", tmplFields{"Worker": worker, "Changed": changed, "Procedure": procedure})
}

func MemoryDreamUnchanged() string { return string(read("memory/prompts/dream-unchanged.txt")) }

func MemoryOpenTasks(tasks string) string {
	return renderMemory("misc/open-tasks", tasks)
}

func MemoryRTKMisses(misses string) string {
	return renderMemory("rtk-misses.tmpl", misses)
}

func MemoryReplacementUnavailable() string {
	return string(read("memory/prompts/replacement-unavailable.txt"))
}

func MemoryIndexHint() string { return string(read("memory/prompts/index-hint.txt")) }

var memoryBlockBegin struct {
	sync.Once
	text string
}

func MemoryBlockBegin() string {
	memoryBlockBegin.Do(func() { memoryBlockBegin.text = string(read("memory/prompts/block-begin.txt")) })
	return memoryBlockBegin.text
}

func MemoryBankHead(folders, router string) string {
	return renderMemory("bank/head", tmplFields{"Folders": folders, "Router": router})
}

func MemoryMoreEntries(count int, router string) string {
	return renderMemory("misc/more-entries", tmplFields{"Count": count, "Router": router})
}

func MemoryUnresolvedEntries(count int) string {
	return renderMemory("misc/unresolved", count)
}

func MemoryCatalogHeader(date string) string {
	return renderMemory("catalog/header", tmplFields{"Date": date})
}

func MemoryCatalogBank(date string) string {
	return renderMemory("catalog/bank", tmplFields{"Date": date})
}

func MemoryHomeIndexHeading() string { return string(read("memory/prompts/home-index.txt")) }

func MemoryKnowledgeHeading() string { return string(read("memory/prompts/knowledge.txt")) }

func MemoryFiledByGo(entries string) string { return renderMemory("misc/filed-by-go", entries) }

func MemoryHandoffRequest(path, identity string) string {
	return renderMemory("misc/handoff-request", tmplFields{"Path": path, "Identity": identity})
}

func MemoryRetryNote(failure string) string { return renderMemory("misc/retry-note", failure) }

func MemoryHomeOverflow(count int) string {
	return renderMemory("misc/home-overflow", count)
}

var memoryFolderRoles struct {
	sync.Once
	rows [][2]string
}

func MemoryFolderRoles() [][2]string {
	memoryFolderRoles.Do(func() {
		if err := json.Unmarshal(read("memory/folder-roles.json"), &memoryFolderRoles.rows); err != nil {
			panic("assets: memory folder roles: " + err.Error())
		}
	})
	return slices.Clone(memoryFolderRoles.rows)
}

func MemoryLayoutHeading() string { return string(read("memory/prompts/layout.txt")) }

func MemoryLayoutRow(folder, role string, count int) string {
	return renderMemory("misc/layout-row", tmplFields{"Folder": folder, "Role": role, "Count": count})
}

func MemoryNativeHeading() string { return string(read("memory/prompts/native.txt")) }

func MemoryNativeFolder(folder string) string {
	return renderMemory("native/folder", folder)
}

func MemoryNativeUnparsed(count int) string {
	return renderMemory("native/unparsed", count)
}

func MemoryRecallLine(file, text string, label bool) string {
	return renderMemory("recall/line", tmplFields{"File": file, "Text": text, "Label": label})
}

func MemoryRecallHeader(expand bool) string {
	return renderMemory("recall/header", expand)
}

func MemoryRecallFooter() string { return string(read("memory/prompts/recall-footer.txt")) }

func MemoryRecallMarker() string { return string(read("memory/prompts/recall-marker.txt")) }

func MemoryBankLoadNote(prefix string) string {
	return renderMemory("bank/load", prefix)
}

func MemoryPreviousSession(id string) string {
	return renderMemory("misc/previous-session", id)
}

func MemoryUpdateRecovery(id, status, log string) string {
	return renderMemory("misc/update-recovery", tmplFields{"ID": id, "Status": status, "Log": log})
}

func MemoryScaffoldNote(dir string) string {
	return renderMemory("misc/scaffold", dir)
}

func MemoryCatalogPointer(path string) string {
	return renderMemory("catalog/pointer", path)
}

func MemoryBankHeading(root string) string {
	return renderMemory("bank/heading", root)
}

func MemoryNativeContext(index string) string {
	return renderMemory("native/context", index)
}

func TranscriptHandoff(cwd, transcript, note string) string {
	return renderMemory("transcript/handoff", tmplFields{"CWD": cwd, "Transcript": transcript, "Note": note})
}

var memoryTemplates = loadTemplates("memory/prompts/*.tmpl")

func renderMemory(name string, input any) string {
	return renderSet(memoryTemplates(), "memory template", name, input)
}

func memoryProcedure(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		panic("assets: empty memory procedure")
	}
	return text
}
