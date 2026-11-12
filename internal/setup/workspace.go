package setup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// workspaceParts are the pieces `setup workspace init --with` can pick; empty = all of them.
var workspaceParts = []string{"agents", "commands", "skills", "settings", "memory"}

// workspaceKinds are the project-local dirs that can hold a symlink to a global entry.
var workspaceKinds = []string{"agents", "commands", "skills"}

// errWorkspaceUsage marks a caller mistake (bad --with, bad name) so the CLI exits 2, not 1.
var errWorkspaceUsage = errors.New("usage")

// workspaceIgnoreLine is the one project .claude/ path that never belongs in git.
const workspaceIgnoreLine = ".claude/settings.local.json"

// workspaceSettings is the only project settings.json init writes: project-scoped keys, no hooks.
const workspaceSettings = "{\n  \"permissions\": {\n    \"allow\": []\n  },\n  \"env\": {}\n}\n"

// workspaceInitOpts drives workspaceInit; Out receives one line per action.
type workspaceInitOpts struct {
	Root   string
	With   []string
	DryRun bool
	Out    io.Writer
}

// workspaceAction is one planned step: printed as "<verb>  <path>", applied unless --dry-run.
type workspaceAction struct {
	verb  string
	path  string
	apply func() error
}

// workspaceRoot resolves root to a git toplevel outside the harness config dir, or explains why not.
func workspaceRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = paths.Canon(abs)
	cfg := paths.Canon(paths.ConfigDir())
	if paths.Within(abs, cfg) {
		return "", fmt.Errorf("%s is inside the harness config dir %s: a project .claude/ cannot nest there", abs, cfg)
	}
	top := memory.GitToplevel(abs)
	if top == "" || !paths.SamePath(top, abs) {
		return "", fmt.Errorf("%s is not a git toplevel (run from the repo root, or git init first)", abs)
	}
	return abs, nil
}

// workspaceWith validates --with against workspaceParts; empty selects everything.
func workspaceWith(with []string) ([]string, error) {
	if len(with) == 0 {
		return workspaceParts, nil
	}
	for _, w := range with {
		if !slices.Contains(workspaceParts, w) {
			return nil, fmt.Errorf("%w: --with %q: want a subset of %s", errWorkspaceUsage, w, strings.Join(workspaceParts, ","))
		}
	}
	return with, nil
}

// workspaceInit scaffolds root/.claude/ (project settings, empty agents/commands/skills, gitignore line,
// memory bank) without ever overwriting what is already there.
func workspaceInit(o workspaceInitOpts) error {
	with, err := workspaceWith(o.With)
	if err != nil {
		return err
	}
	root, err := workspaceRoot(o.Root)
	if err != nil {
		return err
	}
	if err := runWorkspaceActions(o, workspacePlan(root, with)); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); os.IsNotExist(err) {
		fmt.Fprintln(o.Out, "hint    no CLAUDE.md at the root: run /init in Claude Code to write one")
	}
	if !o.DryRun {
		audit.Log(audit.SetupWorkspace, root, "init:"+strings.Join(with, ","))
	}
	return nil
}

// runWorkspaceActions prints each action and applies it unless --dry-run; an error action fails even a dry run.
func runWorkspaceActions(o workspaceInitOpts, actions []workspaceAction) error {
	for _, a := range actions {
		fmt.Fprintf(o.Out, "%-7s %s\n", a.verb, a.path)
		if a.apply == nil || (o.DryRun && a.verb != "error") {
			continue
		}
		if err := a.apply(); err != nil {
			return fmt.Errorf("%s %s: %w", a.verb, a.path, err)
		}
	}
	return nil
}

// workspacePlan lists every action init would take for root, in a stable order.
func workspacePlan(root string, with []string) []workspaceAction {
	dot := filepath.Join(root, ".claude")
	var out []workspaceAction
	for _, kind := range workspaceKinds {
		if slices.Contains(with, kind) {
			out = append(out, workspaceDirAction(filepath.Join(dot, kind)))
		}
	}
	if slices.Contains(with, "settings") {
		out = append(out, workspaceFileAction(filepath.Join(dot, "settings.json"), workspaceSettings))
		out = append(out, workspaceIgnoreAction(root))
	}
	if slices.Contains(with, "memory") {
		out = append(out, workspaceMemoryAction(root))
	}
	return out
}

// workspaceExists reports whether path is present; a dangling symlink is present, and only not-exist means absent.
func workspaceExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// workspaceErrorAction reports a path that cannot be planned; applying it fails with err.
func workspaceErrorAction(path string, err error) workspaceAction {
	return workspaceAction{verb: "error", path: path, apply: func() error { return err }}
}

// workspaceDirAction keeps dir when it is a directory, creates it with a .gitkeep when absent, and errors otherwise.
func workspaceDirAction(dir string) workspaceAction {
	present, err := workspaceExists(dir)
	if err != nil {
		return workspaceErrorAction(dir, err)
	}
	if present {
		fi, err := os.Stat(dir)
		if err != nil {
			return workspaceErrorAction(dir, err)
		}
		if !fi.IsDir() {
			return workspaceErrorAction(dir, errors.New("exists and is not a directory"))
		}
		return workspaceAction{verb: "keep", path: dir}
	}
	return workspaceAction{verb: "create", path: dir, apply: func() error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, ".gitkeep"), nil, 0o644)
	}}
}

// workspaceFileAction writes body to path only when path is absent.
func workspaceFileAction(path, body string) workspaceAction {
	present, err := workspaceExists(path)
	if err != nil {
		return workspaceErrorAction(path, err)
	}
	if present {
		return workspaceAction{verb: "keep", path: path}
	}
	return workspaceAction{verb: "create", path: path, apply: func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return paths.AtomicWrite(path, []byte(body), 0o644)
	}}
}

// workspaceIgnoreAction appends the settings.local.json line to root/.gitignore once.
func workspaceIgnoreAction(root string) workspaceAction {
	path := filepath.Join(root, ".gitignore")
	has, err := workspaceHasIgnore(path)
	if err != nil {
		return workspaceErrorAction(path, err)
	}
	if has {
		return workspaceAction{verb: "keep", path: path}
	}
	verb := "append"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		verb = "create"
	}
	return workspaceAction{verb: verb, path: path, apply: func() error {
		cur, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if len(cur) > 0 && !strings.HasSuffix(string(cur), "\n") {
			cur = append(cur, '\n')
		}
		return paths.AtomicWrite(path, append(cur, []byte(workspaceIgnoreLine+"\n")...), 0o644)
	}}
}

// workspaceHasIgnore reports whether path already lists the settings.local.json line; a missing file
// counts as absence, but any other read error propagates instead of being mistaken for it.
func workspaceHasIgnore(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.TrimSpace(line) == workspaceIgnoreLine {
			return true, nil
		}
	}
	return false, nil
}

// workspaceMemoryAction seeds root's memory bank (~/.claude/memory/<repo>), or reports it kept.
func workspaceMemoryAction(root string) workspaceAction {
	dir := memory.Dir(root)
	present, err := workspaceExists(filepath.Join(dir, memory.BankFiles()[0]))
	if err != nil {
		return workspaceErrorAction(dir, err)
	}
	if present {
		return workspaceAction{verb: "keep", path: dir}
	}
	return workspaceAction{verb: "create", path: dir, apply: func() error {
		_, err := memory.Init(root)
		return err
	}}
}

// workspaceRel maps a link name to its path under <kind>/: agents and commands are .md files
// ("ns:cmd" → ns/cmd.md), skills are directories.
func workspaceRel(kind, name string) (string, error) {
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: bad %s name %q", errWorkspaceUsage, kind, name)
	}
	switch kind {
	case "agents":
		return strings.TrimSuffix(name, ".md") + ".md", nil
	case "commands":
		ns, cmd, ok := strings.Cut(name, ":")
		if !ok || ns == "" || strings.TrimSuffix(cmd, ".md") == "" {
			return "", fmt.Errorf("%w: command %q: want <namespace>:<command>", errWorkspaceUsage, name)
		}
		return filepath.Join(ns, strings.TrimSuffix(cmd, ".md")+".md"), nil
	case "skills":
		return name, nil
	}
	return "", fmt.Errorf("%w: kind %q: want one of %s", errWorkspaceUsage, kind, strings.Join(workspaceKinds, "|"))
}

// workspaceLink symlinks root/.claude/<kind>/<name> to the harness's global copy so a project pins
// a subset of the harness without copying it.
func workspaceLink(root, kind, name string, out io.Writer) error {
	root, err := workspaceRoot(root)
	if err != nil {
		return err
	}
	rel, err := workspaceRel(kind, name)
	if err != nil {
		return err
	}
	src := filepath.Join(paths.ConfigDir(), kind, rel)
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no global %s %q at %s", strings.TrimSuffix(kind, "s"), name, src)
	}
	dst := filepath.Join(root, ".claude", kind, rel)
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("%s already exists (remove it first to relink)", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(src, dst); err != nil {
		return err
	}
	fmt.Fprintf(out, "linked  %s → %s\n", dst, src)
	audit.Log(audit.SetupWorkspace, root, "link:"+kind+"/"+name)
	return nil
}

// workspaceStatus lists each project entry as linked, local, or shadowing a global one of the same name,
// then the settings, memory and gitignore state.
func workspaceStatus(root string, out io.Writer) error {
	root, err := workspaceRoot(root)
	if err != nil {
		return err
	}
	dot := filepath.Join(root, ".claude")
	for _, kind := range workspaceKinds {
		if err := workspaceStatusKind(dot, kind, out); err != nil {
			return err
		}
	}
	settings, err := workspacePresent(filepath.Join(dot, "settings.json"))
	if err != nil {
		return err
	}
	bankState, err := workspacePresent(filepath.Join(memory.Dir(root), memory.BankFiles()[0]))
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "settings  %s\n", settings)
	fmt.Fprintf(out, "memory    %s  %s\n", bankState, memory.Dir(root))
	has, err := workspaceHasIgnore(filepath.Join(root, ".gitignore"))
	if err != nil {
		return err
	}
	ignore := "missing"
	if has {
		ignore = "present"
	}
	fmt.Fprintf(out, "gitignore %s  %s\n", ignore, workspaceIgnoreLine)
	return nil
}

// workspaceStatusKind prints one line per entry under dot/<kind>; a missing dir prints nothing.
func workspaceStatusKind(dot, kind string, out io.Writer) error {
	dir := filepath.Join(dot, kind)
	present, err := workspaceExists(dir)
	if err != nil || !present {
		return err
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir || d.Name() == ".gitkeep" {
			return err
		}
		return workspaceStatusRow(kind, dir, p, d, out)
	})
}

// workspaceStatusRow prints one entry's row; a namespace dir under agents/commands only holds rows of its own.
func workspaceStatusRow(kind, dir, p string, d fs.DirEntry, out io.Writer) error {
	if d.IsDir() && kind != "skills" {
		return nil
	}
	rel, _ := filepath.Rel(dir, p)
	state, target, err := workspaceEntryState(kind, rel, p)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%-9s %-16s %s%s\n", kind, state, rel, target)
	if d.IsDir() {
		return fs.SkipDir // SkipDir on a file would skip its siblings; WalkDir never descends symlinks
	}
	return nil
}

// workspaceEntryState classifies one entry: a symlink is linked; a local copy whose name also exists
// globally shadows it; anything else is local.
func workspaceEntryState(kind, rel, p string) (state, target string, err error) {
	if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		t, rerr := os.Readlink(p)
		if rerr != nil {
			return "", "", rerr
		}
		return "linked", "  → " + t, nil
	}
	shadows, err := workspaceExists(filepath.Join(paths.ConfigDir(), kind, rel))
	if err != nil {
		return "", "", err
	}
	if shadows {
		return "shadows global", "", nil
	}
	return "local", "", nil
}

// workspacePresent is "present" or "absent" for path; any error other than not-exist propagates.
func workspacePresent(path string) (string, error) {
	present, err := workspaceExists(path)
	if err != nil {
		return "", err
	}
	if present {
		return "present", nil
	}
	return "absent", nil
}
