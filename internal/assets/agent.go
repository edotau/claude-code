package assets

import (
	"io/fs"
	"path"
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
			roots[path.Base(name)] = parseTemplate(path.Base(name), read(name))
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
	return renderTemplate(name, root, input)
}

func AgentPreamble(text string) string { return renderAgent("preamble.tmpl", text) }

func AgentWebViaCLI() string { return string(read("files/agent/web-via-cli.txt")) }

func AgentResumePrompt() string { return string(read("files/agent/resume.txt")) }

func AgentRubricLead() string { return string(read("files/agent/rubric-lead.txt")) }

func AgentRubricBody(standard, task string) string {
	return renderAgent("rubric.tmpl", tmplFields{"Standard": standard, "Task": task})
}

func AgentPersona(in PersonaFrame) string {
	return renderAgent("persona.tmpl", tmplFields{"Name": in.Name, "Description": in.Description, "Prompt": in.Prompt})
}

func AgentMidRun(who, partial, reconcile, prompt string, edit bool) string {
	return renderAgent("mid-run.tmpl", tmplFields{
		"Who": who, "Partial": partial, "Reconcile": reconcile, "Prompt": prompt, "Edit": edit,
	})
}

func AgentPartial(block string) string {
	return renderAgent("partial.tmpl", tmplFields{"Block": block})
}

func AgentReconcile(log string) string { return renderAgent("reconcile.tmpl", tmplFields{"Log": log}) }

func AgentHandoff(agentName, block string) string {
	return renderAgent("handoff.tmpl", tmplFields{"Agent": agentName, "Block": block})
}

func AgentRecovery(in RecoveryFrame) string {
	return renderAgent("recovery.tmpl", tmplFields{
		"Prompt": in.Prompt, "ID": in.ID, "Status": in.Status, "Cutoff": in.Cutoff,
		"HasFiles": in.HasFiles, "Files": in.Files, "Log": in.Log,
	})
}

func AgentSchemaInstruction(schema string) string { return renderAgent("schema.tmpl", schema) }

func PackPrompt(task, inventory, blocks string) string {
	return renderAgent("pack-prompt.tmpl", tmplFields{"Task": task, "Inventory": inventory, "Blocks": blocks})
}

func PackIndex(index string) string {
	return renderAgent("pack-index.tmpl", tmplFields{"Index": index})
}

func PackCoverageWarning(in PackCoverage) string {
	return renderAgent("pack-coverage.tmpl", tmplFields{
		"Included": in.Included, "Total": in.Total, "KB": in.KB, "Skipped": in.Skipped, "Reasons": in.Reasons,
	})
}

func PackNoFiles() string { return string(read("files/agent/pack-no-files.txt")) }

func HookSimplicityHeader(n int, rel string) string {
	return renderHook("simplicity-header.tmpl", tmplFields{"N": n, "Rel": rel})
}

func HookSimplicityMore(count int) string {
	return renderHook("simplicity-more.tmpl", tmplFields{"Count": count})
}

func HookPathFailed(path, err string) string {
	return renderHook("paths-failed.tmpl", tmplFields{"Path": path, "Err": err})
}

func HookPathMapped(win, path, detail string) string {
	return renderHook("paths-mapped.tmpl", tmplFields{"Win": win, "Path": path, "Detail": detail})
}

func HookPathDetail(path, detail string) string {
	return renderHook("paths-detail.tmpl", tmplFields{"Path": path, "Detail": detail})
}

func HookPathsContext(lines, images string) string {
	return renderHook("paths-context.tmpl", tmplFields{"Lines": lines, "Images": images})
}

func HookBusyPeer(comm string, pid int, branch string, lanes bool) string {
	return renderHook("busy-peer.tmpl", tmplFields{"Comm": comm, "PID": pid, "Branch": branch, "Lanes": lanes})
}

func HookNoticeLine(who, text string) string {
	return renderHook("notice-line.tmpl", tmplFields{"Who": who, "Text": text})
}

func HookNoticeDrainWarn(err string) string {
	return renderHook("notice-drain-warn.tmpl", tmplFields{"Err": err})
}

func HookNoticeDivergeWarn(err string) string {
	return renderHook("notice-diverge-warn.tmpl", tmplFields{"Err": err})
}

func HookDiverged(branch, up string, ahead, behind int, rewritten bool) string {
	return renderHook("diverged.tmpl", tmplFields{
		"Branch": branch, "Up": up, "Ahead": ahead, "Behind": behind, "Rewritten": rewritten,
	})
}
