// Instruction files — CLAUDE.md: main session only; any subagent's write is blocked.

package safety

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/shell"
	"github.com/edotau/claude-code/internal/textutil"
)

// instructionNames are the guarded basenames, matched case-folded where the filesystem folds case.
var instructionNames = []string{"CLAUDE.md"}

// instructionVerdict blocks a subagent's (agent_id or a dispatch id) write to CLAUDE.md; a user-started Pi is main.
func instructionVerdict(in *core.Input, cmd string) (core.Decision, string, string) {
	if in.AgentID == "" && os.Getenv(proc.DispatchIDEnv) == "" {
		return core.Allow, "", ""
	}
	f := instructionTarget(in, cmd)
	if f == "" {
		return core.Allow, "", ""
	}
	msg := core.SafetyMessage(in, core.Block, assets.SafetyInstructionWhat(), f, assets.SafetyInstructionHint())
	return core.Block, msg, "instruction file"
}

// instructionTarget is the first instruction file the call writes, moves, links or removes ("" if none).
func instructionTarget(in *core.Input, cmd string) string {
	switch tool := strings.ToLower(in.CanonicalTool()); {
	case tool == "" || tool == "bash":
		return bashInstructionTarget(in, cmd)
	case editTools[tool]:
		var targets []string
		for _, e := range in.EditedPaths() {
			targets = append(targets, paths.AnchorPath(e.Path, in.CWD))
		}
		return firstInstruction(targets)
	}
	return ""
}

// bashInstructionTarget checks written, removed, patched and linked paths, then $var redirect targets.
func bashInstructionTarget(in *core.Input, cmd string) string {
	raw := in.ToolCommand() // dropPatchBodies took the apply_patch text out of cmd
	if cmd == "" && raw == "" {
		return ""
	}
	targets := slices.Concat(core.WrittenPathsPrecise(cmd, in.CWD), core.RemovedPaths(cmd, in.CWD), patchTargets(raw, in.CWD))
	named := namesInstruction(raw)
	if named {
		targets = append(targets, core.SegmentTargets(cmd, in.CWD, extraTargets)...)
	}
	if f := firstInstruction(targets); f != "" || !named {
		return f
	}
	return assignedTarget(cmd, targets)
}

// namesInstruction reports text naming an instruction file, the gate before the costlier parses.
func namesInstruction(text string) bool {
	folded := paths.FoldCase(text)
	return slices.ContainsFunc(instructionNames, func(n string) bool { return strings.Contains(folded, paths.FoldCase(n)) })
}

// firstInstruction is the first path that is, or resolves through a symlink to, an instruction file.
func firstInstruction(targets []string) string {
	for _, p := range targets {
		if instructionFile(p) || filepath.IsAbs(p) && instructionFile(paths.Real(p)) {
			return p
		}
	}
	return ""
}

func instructionFile(p string) bool {
	base := paths.FoldCase(filepath.Base(p))
	return slices.ContainsFunc(instructionNames, func(n string) bool { return base == paths.FoldCase(n) })
}

// patchTargets are the files an apply_patch body in a shell command adds, updates, moves or deletes.
func patchTargets(raw, cwd string) []string {
	if !strings.Contains(raw, "*** Begin Patch") {
		return nil
	}
	var out []string
	for _, e := range textutil.PatchEdits(raw) {
		out = append(out, paths.AnchorPath(e.Path, cwd))
	}
	return out
}

// assignedTarget is an instruction file a `NAME=value` word in cmd holds and a $NAME write target expands to.
func assignedTarget(cmd string, targets []string) string {
	vals := map[string]string{}
	for _, seg := range shell.Segments(cmd) {
		for _, tok := range shell.Tokens(seg) {
			if name, v, _ := strings.Cut(tok, "="); isAssignment(tok) && instructionFile(v) {
				vals[name] = v
			}
		}
	}
	for _, t := range targets {
		for name, v := range vals {
			if strings.Contains(t, "$"+name) || strings.Contains(t, "${"+name+"}") {
				return v
			}
		}
	}
	return ""
}

// gitPathSubcommands are the git subcommands whose path operands are overwritten, moved or removed.
var gitPathSubcommands = map[string]bool{"checkout": true, "restore": true, "rm": true, "mv": true, "clean": true}

// gitValueFlags are the global git flags whose value is the next word (guards.json).
var gitValueFlags = sync.OnceValue(func() map[string]bool { return shell.GuardSet("git_global_value_flags") })

// extraTargets covers what WrittenPaths does not: links, -t DIR files and git path rewrites.
func extraTargets(rest []string) []string {
	rest = shell.DropRtk(rest)
	if len(rest) < 2 {
		return nil
	}
	switch name := filepath.Base(rest[0]); name {
	case "git":
		return gitPathOperands(rest[1:])
	case "ln", "cp", "mv", "install":
		return linkTargets(name == "ln", rest[1:])
	}
	return nil
}

// linkTargets are DIR/<source name> under -t DIR; for ln also its sources, its link and a lone operand's name.
func linkTargets(ln bool, args []string) []string {
	dir, ops := targetDirArgs(args)
	var out []string
	if ln {
		out = append(out, ops...) // a link to an instruction file is a second path to it
	}
	switch {
	case dir != "":
		for _, s := range ops {
			out = append(out, filepath.Join(dir, filepath.Base(s)))
		}
	case ln && len(ops) == 1:
		out = append(out, filepath.Base(ops[0]))
	}
	return out
}

// targetDirArgs splits args into the -t / --target-directory= value and the operands.
func targetDirArgs(args []string) (dir string, ops []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-t" && i+1 < len(args):
			dir, i = args[i+1], i+1
		case strings.HasPrefix(a, "--target-directory="):
			dir = strings.TrimPrefix(a, "--target-directory=")
		case a != "" && a[0] != '-':
			ops = append(ops, a)
		}
	}
	return dir, ops
}

// gitPathOperands are the operands after a checkout, restore, rm, mv or clean subcommand, past global flags.
func gitPathOperands(args []string) []string {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if gitValueFlags()[args[i]] {
			i++
		}
	}
	if i >= len(args) || !gitPathSubcommands[args[i]] {
		return nil
	}
	return pathOperands(args[i+1:])
}

// pathOperands are the non-flag words of args, and every word after a first `--`.
func pathOperands(args []string) []string {
	var out []string
	dash := false
	for _, a := range args {
		switch {
		case a == "--" && !dash:
			dash = true
		case dash || a != "" && a[0] != '-':
			out = append(out, a)
		}
	}
	return out
}
