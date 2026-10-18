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

// ErrWorkspaceUsage marks a caller mistake (bad --with, bad name) so the CLI exits 2, not 1.
var ErrWorkspaceUsage = errors.New("usage")

// workspaceIgnoreLine is the one project .claude/ path that never belongs in git.
const workspaceIgnoreLine = ".claude/settings.local.json"

// workspaceSettings is the only project settings.json init writes: project-scoped keys, no hooks.
const workspaceSettings = "{\n  \"permissions\": {\n    \"allow\": []\n  },\n  \"env\": {}\n}\n"

// WorkspaceInitOpts drives WorkspaceInit; Out receives one line per action.
type WorkspaceInitOpts struct {
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
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	cfg := paths.ConfigDir()
	if real, err := filepath.EvalSymlinks(cfg); err == nil {
		cfg = real
	}
	if paths.SamePath(abs, cfg) || strings.HasPrefix(abs, cfg+string(filepath.Separator)) {
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
			return nil, fmt.Errorf("%w: --with %q: want a subset of %s", ErrWorkspaceUsage, w, strings.Join(workspaceParts, ","))
		}
	}
	return with, nil
}

// WorkspaceInit scaffolds root/.claude/ (project settings, empty agents/commands/skills, gitignore line,
// memory bank) without ever overwriting what is already there.
func WorkspaceInit(o WorkspaceInitOpts) error {
	with, err := workspaceWith(o.With)
	if err != nil {
		return err
	}
	root, err := workspaceRoot(o.Root)
	if err != nil {
		return err
	}
	actions := workspacePlan(root, with)
	for _, a := range actions {
		fmt.Fprintf(o.Out, "%-7s %s\n", a.verb, a.path)
		if o.DryRun || a.apply == nil {
			continue
		}
		if err := a.apply(); err != nil {
			return fmt.Errorf("%s %s: %w", a.verb, a.path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "CLAUDE.md")); os.IsNotExist(err) {
		fmt.Fprintln(o.Out, "hint    no CLAUDE.md at the root: run /init in Claude Code to write one")
	}
	if !o.DryRun {
		audit.Log(audit.SetupWorkspace, root, "init:"+strings.Join(with, ","))
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

// workspaceDirAction creates dir with a .gitkeep, or reports it kept.
func workspaceDirAction(dir string) workspaceAction {
	if _, err := os.Stat(dir); err == nil {
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
	if _, err := os.Stat(path); err == nil {
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
	if workspaceHasIgnore(path) {
		return workspaceAction{verb: "keep", path: path}
	}
	verb := "append"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		verb = "create"
	}
	return workspaceAction{verb: verb, path: path, apply: func() error {
		cur, _ := os.ReadFile(path)
		if len(cur) > 0 && !strings.HasSuffix(string(cur), "\n") {
			cur = append(cur, '\n')
		}
		return paths.AtomicWrite(path, append(cur, []byte(workspaceIgnoreLine+"\n")...), 0o644)
	}}
}

// workspaceHasIgnore reports whether .gitignore already lists the settings.local.json line.
func workspaceHasIgnore(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return slices.Contains(strings.Split(string(b), "\n"), workspaceIgnoreLine)
}

// workspaceMemoryAction seeds root's memory bank (~/.claude/memory/<repo>), or reports it kept.
func workspaceMemoryAction(root string) workspaceAction {
	dir := memory.Dir(root)
	if _, err := os.Stat(filepath.Join(dir, memory.BankFiles()[0])); err == nil {
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
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: bad %s name %q", ErrWorkspaceUsage, kind, name)
	}
	switch kind {
	case "agents":
		return strings.TrimSuffix(name, ".md") + ".md", nil
	case "commands":
		ns, cmd, ok := strings.Cut(name, ":")
		if !ok {
			return "", fmt.Errorf("%w: command %q: want <namespace>:<command>", ErrWorkspaceUsage, name)
		}
		return filepath.Join(ns, strings.TrimSuffix(cmd, ".md")+".md"), nil
	case "skills":
		return name, nil
	}
	return "", fmt.Errorf("%w: kind %q: want one of %s", ErrWorkspaceUsage, kind, strings.Join(workspaceKinds, "|"))
}

// WorkspaceLink symlinks root/.claude/<kind>/<name> to the harness's global copy so a project pins
// a subset of the harness without copying it.
func WorkspaceLink(root, kind, name string, out io.Writer) error {
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

// WorkspaceStatus lists each project entry as linked, local, or shadowing a global one of the same name,
// then the settings, memory and gitignore state.
func WorkspaceStatus(root string, out io.Writer) error {
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
	fmt.Fprintf(out, "settings  %s\n", workspacePresent(filepath.Join(dot, "settings.json")))
	fmt.Fprintf(out, "memory    %s  %s\n", workspacePresent(filepath.Join(memory.Dir(root), memory.BankFiles()[0])), memory.Dir(root))
	ignore := "missing"
	if workspaceHasIgnore(filepath.Join(root, ".gitignore")) {
		ignore = "present"
	}
	fmt.Fprintf(out, "gitignore %s  %s\n", ignore, workspaceIgnoreLine)
	return nil
}

// workspaceStatusKind prints one line per entry under dot/<kind>; a missing dir prints nothing.
func workspaceStatusKind(dot, kind string, out io.Writer) error {
	dir := filepath.Join(dot, kind)
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir || d.Name() == ".gitkeep" {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		state, target := workspaceEntryState(kind, rel, p)
		fmt.Fprintf(out, "%-9s %-16s %s%s\n", kind, state, rel, target)
		if state == "linked" || (kind == "skills" && d.IsDir()) {
			return fs.SkipDir
		}
		return nil
	})
}

// workspaceEntryState classifies one entry: a symlink is linked; a local copy whose name also exists
// globally shadows it; anything else is local.
func workspaceEntryState(kind, rel, p string) (state, target string) {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t, _ := os.Readlink(p)
		return "linked", "  → " + t
	}
	if _, err := os.Stat(filepath.Join(paths.ConfigDir(), kind, rel)); err == nil {
		return "shadows global", ""
	}
	return "local", ""
}

// workspacePresent is "present" or "absent" for path.
func workspacePresent(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "present"
	}
	return "absent"
}
