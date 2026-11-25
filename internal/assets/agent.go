package assets

import (
	"io/fs"
	"path"
	"strings"
	"sync"
	"text/template/parse"
)

type PersonaFrame struct{ Name, Description, Prompt string }

type RecoveryFrame struct {
	Prompt, ID, Status, Cutoff, Files, Log string
	HasFiles                               bool
}

type PackCoverage struct {
	Included, Total, KB, Skipped int
	Reasons                      string
}

func loadTemplates(glob string) func() map[string]*parse.ListNode {
	return sync.OnceValue(func() map[string]*parse.ListNode {
		names, err := fs.Glob(files, glob)
		if err != nil {
			panic("assets: templates " + glob + ": " + err.Error())
		}
		roots := make(map[string]*parse.ListNode, len(names))
		for _, name := range names {
			for key, root := range parseTemplates(path.Base(name), read(name)) {
				if _, dup := roots[key]; dup {
					panic("assets: duplicate template " + key + " in " + glob)
				}
				roots[key] = root
			}
		}
		return roots
	})
}

var (
	agentTemplates = loadTemplates("files/agent/*.tmpl")
	hookTemplates  = loadTemplates("files/hooks/*.tmpl")
)

func renderAgent(name string, input any) string { return renderFrom(agentTemplates(), name, input) }

func renderHook(name string, input any) string { return renderFrom(hookTemplates(), name, input) }

func renderFrom(set map[string]*parse.ListNode, name string, input any) string {
	root, ok := set[name]
	if !ok {
		panic("assets: no template " + name)
	}
	if renderedKey != nil {
		renderedKey(name)
	}
	var out strings.Builder
	tmplExec{&out, set}.execTemplate(name, root, input, 0)
	return out.String()
}

func AgentPreamble(text string) string { return renderAgent("preamble.tmpl", text) }

func AgentWebViaCLI() string { return string(read("files/agent/web-via-cli.txt")) }

func AgentResumePrompt() string { return string(read("files/agent/resume.txt")) }

func AgentRubricLead() string { return string(read("files/agent/rubric-lead.txt")) }

func AgentRubricBody(standard, task string) string {
	return renderAgent("rubric.tmpl", tmplFields{"Standard": standard, "Task": task})
}

func AgentPersona(in PersonaFrame) string {
	return renderAgent("misc/persona", tmplFields{"Name": in.Name, "Description": in.Description, "Prompt": in.Prompt})
}

func AgentMidRun(who, partial, reconcile, prompt string, edit bool) string {
	return renderAgent("misc/mid-run", tmplFields{
		"Who": who, "Partial": partial, "Reconcile": reconcile, "Prompt": prompt, "Edit": edit,
	})
}

func AgentPartial(block string) string {
	return renderAgent("misc/partial", tmplFields{"Block": block})
}

func AgentReconcile(log string) string { return renderAgent("misc/reconcile", tmplFields{"Log": log}) }

func AgentHandoff(agentName, block string) string {
	return renderAgent("misc/handoff", tmplFields{"Agent": agentName, "Block": block})
}

func AgentRecovery(in RecoveryFrame) string {
	return renderAgent("recovery.tmpl", tmplFields{
		"Prompt": in.Prompt, "ID": in.ID, "Status": in.Status, "Cutoff": in.Cutoff,
		"HasFiles": in.HasFiles, "Files": in.Files, "Log": in.Log,
	})
}

func AgentSchemaInstruction(schema string) string { return renderAgent("misc/schema", schema) }

func PackPrompt(task, inventory, blocks string) string {
	return renderAgent("pack-prompt.tmpl", tmplFields{"Task": task, "Inventory": inventory, "Blocks": blocks})
}

func PackIndex(index string) string {
	return renderAgent("pack-index.tmpl", tmplFields{"Index": index})
}

func PackCoverageWarning(in PackCoverage) string {
	return renderAgent("misc/pack-coverage", tmplFields{
		"Included": in.Included, "Total": in.Total, "KB": in.KB, "Skipped": in.Skipped, "Reasons": in.Reasons,
	})
}

func PackNoFiles() string { return string(read("files/agent/pack-no-files.txt")) }

func HookSimplicityHeader(n int, rel string) string {
	return renderHook("simplicity/header", tmplFields{"N": n, "Rel": rel})
}

func HookSimplicityMore(count int) string {
	return renderHook("simplicity/more", tmplFields{"Count": count})
}

func HookPathFailed(path, err string) string {
	return renderHook("paths/failed", tmplFields{"Path": path, "Err": err})
}

func HookPathMapped(win, path, detail string) string {
	return renderHook("paths/mapped", tmplFields{"Win": win, "Path": path, "Detail": detail})
}

func HookPathDetail(path, detail string) string {
	return renderHook("paths/detail", tmplFields{"Path": path, "Detail": detail})
}

func HookPathsContext(lines, images string) string {
	return renderHook("paths/context", tmplFields{"Lines": lines, "Images": images})
}

func HookBusyPeer(comm string, pid int, branch string, lanes bool) string {
	return renderHook("misc/busy-peer", tmplFields{"Comm": comm, "PID": pid, "Branch": branch, "Lanes": lanes})
}

func HookNoticeLine(who, text string) string {
	return renderHook("notice/line", tmplFields{"Who": who, "Text": text})
}

func HookNoticeDrainWarn(err string) string {
	return renderHook("notice/drain-warn", tmplFields{"Err": err})
}

func HookNoticeDivergeWarn(err string) string {
	return renderHook("notice/diverge-warn", tmplFields{"Err": err})
}

func HookDiverged(branch, up string, ahead, behind int, rewritten bool) string {
	return renderHook("misc/diverged", tmplFields{
		"Branch": branch, "Up": up, "Ahead": ahead, "Behind": behind, "Rewritten": rewritten,
	})
}

func HookClaimHowTo() string { return renderHook("claim/how-to", nil) }

func HookClaimStageHint() string { return renderHook("claim/stage-hint", nil) }

func HookClaimCommitHint() string { return string(read("files/hooks/claim-commit-hint.txt")) }

func HookClaimBareHint() string { return string(read("files/hooks/claim-bare-hint.txt")) }

func HookClaimNestHint() string { return string(read("files/hooks/claim-nest-hint.txt")) }

func HookClaimLaneHint() string { return string(read("files/hooks/claim-lane-hint.txt")) }

// The file stores one trailing newline (git diff --check); the preamble ends with a blank line.
func AgyToolLessPreamble() string { return string(read("files/agent/agy-toolless.txt")) + "\n" }

func PiStdinTask() string { return string(read("files/agent/stdin-task.txt")) }

func ClaudeAttachImage(path string) string { return renderAgent("attach/image", path) }

// AttachFile frames one inline attachment; quotedPath is the path already Go-quoted.
func AttachFile(quotedPath, content string) string {
	return renderAgent("attach-file.tmpl", tmplFields{"Path": quotedPath, "Data": content})
}

func AttachRef(path string) string { return renderAgent("attach/ref", path) }

// The file stores one trailing newline (git diff --check); the head ends with a blank line.
func PlanTasksHead() string { return string(read("files/agent/plan-tasks-head.txt")) + "\n" }

func PlanTasksEmpty() string { return string(read("files/agent/plan-tasks-empty.txt")) }

// PlanCloseout is the checklist tail every plan ends with, one item per line in the file.
func PlanCloseout() []string {
	return strings.Split(strings.TrimRight(string(read("files/agent/plan-closeout.txt")), "\n"), "\n")
}

func HookPlanPathChoose(err string) string { return renderHook("plan/path-choose", err) }

func HookPlanPathRedirect(from, quotedDest string) string {
	return renderHook("plan/path-redirect", tmplFields{"From": from, "Dest": quotedDest})
}

func HookPlanPathMoved(from, dest string) string {
	return renderHook("plan/path-moved", tmplFields{"From": from, "Dest": dest})
}

// HookPlanTemplateBlock names the headings a draft lacks; tpl is the user's template path, "" for the repo's.
func HookPlanTemplateBlock(name, missing, tpl, headings string) string {
	return renderHook("plan/template-block", tmplFields{
		"Name": name, "Missing": missing, "Tpl": tpl, "Headings": headings,
	})
}

func HookPlanAgentWrite(home string) string { return renderHook("plan/agent-write", home) }

type DocsPlacement struct{ Base, Where, Dest, Home, Reports, Handoffs string }

func HookDocsPlacement(in DocsPlacement) string {
	return renderHook("docs/placement", tmplFields{
		"Base": in.Base, "Where": in.Where, "Dest": in.Dest, "Home": in.Home, "Reports": in.Reports, "Handoffs": in.Handoffs,
	})
}

func HookDocsWhere(dir string, docs bool) string {
	return renderHook("docs/where", tmplFields{"Dir": dir, "Docs": docs})
}

func HookDocsRetired(dir string) string { return renderHook("docs/retired", dir) }

func HookMemoryNotePath(from, dest string) string {
	return renderHook("memory/note-path", tmplFields{"From": from, "Dest": dest})
}

func HookMemoryStoreName(from, dest string) string {
	return renderHook("memory/store-name", tmplFields{"From": from, "Dest": dest})
}

func HookReviewFixHead() string { return string(read("files/hooks/review-fix-head.txt")) }

func HookReviewNoLine() string { return string(read("files/hooks/review-no-line.txt")) }
