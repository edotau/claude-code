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
	return renderSet(set, "template", name, input)
}

// renderSet executes set[name]; kind labels the panic for a missing name.
func renderSet(set map[string]*parse.ListNode, kind, name string, input any) string {
	root, ok := set[name]
	if !ok {
		panic("assets: no " + kind + " " + name)
	}
	if renderedKey != nil {
		renderedKey(name)
	}
	if renderedFields != nil {
		renderedFields(name, input)
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

// ContractFrame is ContractBlock's input: Terms and Report are ready text, Fleet adds the re-send notes.
type ContractFrame struct {
	Unit, Kind, Plan, PlanTask, Lane, Terms, Report string
	Tokens, Leaves, Depth                           int
	Fleet                                           bool
}

func ContractBlock(in ContractFrame) string {
	return renderAgent("misc/contract", tmplFields{
		"Unit": in.Unit, "Kind": in.Kind, "Plan": in.Plan, "PlanTask": in.PlanTask, "Lane": in.Lane, "Terms": in.Terms,
		"Report": in.Report, "Tokens": in.Tokens, "Leaves": in.Leaves, "Depth": in.Depth, "Fleet": in.Fleet,
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

func InspectNoScope(files int) string {
	return renderAgent("inspect/no-scope", tmplFields{"Count": files})
}

func InspectAdvisory(files int) string {
	return renderAgent("inspect/advisory", tmplFields{"Count": files})
}

func InspectOutsideModified(path, dispatch string) string {
	return renderAgent("inspect/outside-modified", tmplFields{"Path": path, "Dispatch": dispatch})
}

func InspectNewDecls(count int) string {
	return renderAgent("inspect/new-decls", tmplFields{"Count": count})
}

// ToolsDryRun is the dry-run notice; what finishes "would …" without a closing period.
func ToolsDryRun(what string) string { return renderAgent("tools/dry-run", tmplFields{"What": what}) }

type LoopHeader struct {
	ID, Why, Took  string
	Rounds, Tokens int
}

func LoopReportHeader(in LoopHeader) string {
	return renderAgent("loop/header", tmplFields{
		"ID": in.ID, "Why": in.Why, "Took": in.Took, "Rounds": in.Rounds, "Tokens": in.Tokens,
	})
}

func LoopReportSummary(summary string) string { return renderAgent("loop/summary", summary) }

func LoopReportResume(id string) string { return renderAgent("loop/resume", id) }

// PackInventory lists the included and skipped file lines (each ends in a newline) and the omitted count.
func PackInventory(included, skipped []string, omitted int) string {
	return renderAgent("pack/inventory", tmplFields{"Included": included, "Skipped": skipped, "Omitted": omitted})
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

// BusyPeer is HookBusyPeer's input: Name and Files (a ready list) are empty when unknown.
type BusyPeer struct {
	Comm, Name, Branch, Files string
	PID                       int
	Lanes                     bool
}

func HookBusyPeer(p BusyPeer) string {
	return renderHook("misc/busy-peer", tmplFields{"Comm": p.Comm, "Name": p.Name, "PID": p.PID, "Branch": p.Branch,
		"Files": p.Files, "Lanes": p.Lanes})
}

func HookNoticeLine(who, text string) string {
	return renderHook("notice/line", tmplFields{"Who": who, "Text": text})
}

func HookNoticeDrainWarn(err string) string {
	return renderHook("notice/drain-warn", tmplFields{"Err": err})
}

func HookFailureLine(kind, body, holder string) string {
	return renderHook("notice/failure-line", tmplFields{"Kind": kind, "Body": body, "Holder": holder})
}

func HookFailureClaim(id string) string {
	return renderHook("notice/failure-claim", tmplFields{"ID": id})
}

func HookFailuresMore(more int) string {
	return renderHook("notice/failures-more", tmplFields{"More": more})
}

func HookFailuresWarn(err string) string {
	return renderHook("notice/failures-warn", tmplFields{"Err": err})
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
