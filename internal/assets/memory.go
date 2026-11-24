package assets

import (
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"text/template/parse"
)

type MemoryUpdateInput struct{ Worker, Procedure, Historical, Digest, Git, Tags string }

func MemoryActiveContextSeed() string { return string(read("files/memory/templates/activeContext.md")) }

func MemoryConventionsSeed() string { return string(read("files/memory/templates/conventions.md")) }

func MemoryDecisionLogSeed() string { return string(read("files/memory/templates/decisionLog.md")) }

func MemoryProgressSeed() string { return string(read("files/memory/templates/progress.md")) }

func MemoryProjectContextSeed() string {
	return string(read("files/memory/templates/projectContext.md"))
}

func MemorySessionHistorySeed() string {
	return string(read("files/memory/templates/sessionHistory.md"))
}

func MemoryDreamProcedure() string { return memoryProcedure(read("files/memory/prompts/dream.md")) }

func MemoryInitProcedure() string { return memoryProcedure(read("files/memory/prompts/init.md")) }

func MemoryPromoteProcedure() string { return memoryProcedure(read("files/memory/prompts/promote.md")) }

func MemoryReviewProcedure() string { return memoryProcedure(read("files/memory/prompts/review.md")) }

func MemoryUpdateProcedure() string { return memoryProcedure(read("files/memory/prompts/update.md")) }

func MemoryUpdateWorker(bank string) string {
	return renderMemory("update-opening.tmpl", tmplFields{"Bank": bank})
}

func MemoryDreamWorker(bank string) string {
	return renderMemory("dream-opening.tmpl", tmplFields{"Bank": bank})
}

func MemoryLegacyUpdateWorker(bank, home string) string {
	return renderMemory("legacy-update-opening.tmpl", tmplFields{"Bank": bank, "Home": home})
}

func MemoryLegacyDreamWorker(bank, home string) string {
	return renderMemory("legacy-dream-opening.tmpl", tmplFields{"Bank": bank, "Home": home})
}

func MemoryHistoricalRule() string { return string(read("files/memory/prompts/historical.txt")) }

func MemoryUpdatePrompt(in MemoryUpdateInput) string {
	return renderMemory("update-worker.tmpl", tmplFields{
		"Worker": in.Worker, "Procedure": in.Procedure, "Historical": in.Historical,
		"Digest": in.Digest, "Git": in.Git, "Tags": in.Tags,
	})
}

func MemoryTranscriptPointer(path string) string {
	return renderMemory("transcript-pointer.tmpl", tmplFields{"Path": path})
}

func MemorySessionDigest(path, body string) string {
	return renderMemory("session-digest.tmpl", tmplFields{"Path": path, "Body": body})
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

func MemoryDreamUnchanged() string { return string(read("files/memory/prompts/dream-unchanged.txt")) }

func MemoryOpenTasks(tasks string) string {
	return renderMemory("open-tasks.tmpl", tasks)
}

func MemoryRTKMisses(misses string) string {
	return renderMemory("rtk-misses.tmpl", misses)
}

func MemoryReplacementUnavailable() string {
	return string(read("files/memory/prompts/replacement-unavailable.txt"))
}

func MemoryIndexHint() string { return string(read("files/memory/prompts/index-hint.txt")) }

var memoryBlockBegin struct {
	sync.Once
	text string
}

func MemoryBlockBegin() string {
	memoryBlockBegin.Do(func() { memoryBlockBegin.text = string(read("files/memory/prompts/block-begin.txt")) })
	return memoryBlockBegin.text
}

func MemoryBankHead(folders, router string) string {
	return renderMemory("bank-head.tmpl", tmplFields{"Folders": folders, "Router": router})
}

func MemoryMoreEntries(count int, router string) string {
	return renderMemory("more-entries.tmpl", tmplFields{"Count": count, "Router": router})
}

func MemoryUnresolvedEntries(count int) string {
	return renderMemory("unresolved.tmpl", count)
}

func MemoryCatalogHeader(date string) string {
	return renderMemory("catalog-header.tmpl", tmplFields{"Date": date})
}

func MemoryCatalogBank(hook, date string) string {
	return renderMemory("catalog-bank.tmpl", tmplFields{"Hook": hook, "Date": date})
}

func MemoryHomeIndexHeading() string { return string(read("files/memory/prompts/home-index.txt")) }

func MemoryKnowledgeHeading() string { return string(read("files/memory/prompts/knowledge.txt")) }

func MemoryHomeOverflow(count int) string {
	return renderMemory("home-overflow.tmpl", count)
}

var memoryFolderRoles struct {
	sync.Once
	rows [][2]string
}

func MemoryFolderRoles() [][2]string {
	memoryFolderRoles.Do(func() {
		if err := json.Unmarshal(read("files/memory/folder-roles.json"), &memoryFolderRoles.rows); err != nil {
			panic("assets: memory folder roles: " + err.Error())
		}
	})
	return slices.Clone(memoryFolderRoles.rows)
}

func MemoryLayoutHeading() string { return string(read("files/memory/prompts/layout.txt")) }

func MemoryLayoutRow(folder, role string, count int) string {
	return renderMemory("layout-row.tmpl", tmplFields{"Folder": folder, "Role": role, "Count": count})
}

func MemoryNativeHeading() string { return string(read("files/memory/prompts/native.txt")) }

func MemoryNativeFolder(folder string) string {
	return renderMemory("native-folder.tmpl", folder)
}

func MemoryNativeUnparsed(count int) string {
	return renderMemory("native-unparsed.tmpl", count)
}

func MemoryRecallLine(file, text string, label bool) string {
	return renderMemory("recall-line.tmpl", tmplFields{"File": file, "Text": text, "Label": label})
}

func MemoryRecallHeader(expand bool) string {
	return renderMemory("recall-header.tmpl", expand)
}

func MemoryRecallFooter() string { return string(read("files/memory/prompts/recall-footer.txt")) }

func MemoryRecallMarker() string { return string(read("files/memory/prompts/recall-marker.txt")) }

func MemoryBankLoadNote(prefix string) string {
	return renderMemory("bank-load.tmpl", prefix)
}

func MemoryPreviousSession(id string) string {
	return renderMemory("previous-session.tmpl", id)
}

func MemoryUpdateRecovery(id, status, log string) string {
	return renderMemory("update-recovery.tmpl", tmplFields{"ID": id, "Status": status, "Log": log})
}

func MemoryScaffoldNote(dir string) string {
	return renderMemory("scaffold.tmpl", dir)
}

func MemoryCatalogPointer(path string) string {
	return renderMemory("catalog-pointer.tmpl", path)
}

func MemoryBankHeading(root string) string {
	return renderMemory("bank-heading.tmpl", root)
}

func MemoryNativeContext(index string) string {
	return renderMemory("native-context.tmpl", index)
}

func TranscriptHandoff(cwd, transcript, note string) string {
	return renderMemory("transcript-handoff.tmpl", tmplFields{"CWD": cwd, "Transcript": transcript, "Note": note})
}

var memoryTemplates = sync.OnceValue(func() map[string]*parse.ListNode {
	names, err := fs.Glob(files, "files/memory/prompts/*.tmpl")
	if err != nil {
		panic("assets: memory templates: " + err.Error())
	}
	roots := make(map[string]*parse.ListNode, len(names))
	for _, name := range names {
		roots[path.Base(name)] = parseTemplate(path.Base(name), read(name))
	}
	return roots
})

func renderMemory(name string, input any) string {
	root, ok := memoryTemplates()[name]
	if !ok {
		panic("assets: no memory template " + name)
	}
	return renderTemplate(name, root, input)
}

func memoryProcedure(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		panic("assets: empty memory procedure")
	}
	return text
}
