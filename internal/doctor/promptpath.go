// PromptPath audits the instruction path a session follows (CLAUDE.md, rules, skills, agents).

package doctor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/edotau/claude-code/internal/agentconfig/project"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/review/planlint"
)

const claudeMDCap = 200

// Probe payload for H7; the hooks only read it, the session id is a fixed name.
const probeSession, probePrompt = "doctor-prompts-probe", "plan memory recall probe"

var (
	backtickPath = regexp.MustCompile("`((?:rules|skills|agents|commands|workflows|plans|internal|cmd)/[A-Za-z0-9_./-]+)`")
	symPair      = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]*)` \\(`([A-Za-z0-9_./-]+\\.go)`\\)")
	nameLine     = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)
	descLine     = regexp.MustCompile(`(?m)^description:\s*\S`)
	modelLine    = regexp.MustCompile(`(?m)^model:\s*(\S+)\s*$`)
	modelOK      = regexp.MustCompile(`^(inherit|sonnet|opus|haiku|[A-Za-z0-9][A-Za-z0-9._:/-]*)$`)
	namedPath    = regexp.MustCompile("(?:^|[\\s(\\[\"'`])((?:~|/)[A-Za-z0-9._@+/~-]*[A-Za-z0-9_])")
)

// Seams for H7: production runs the real hooks and sync; tests swap them for fixtures.
var (
	memoryStart  = memhook.Start
	memoryRecall = memhook.Recall
	agentsSync   = func() int { return project.Sync(project.Options{DryRun: true, Check: true, Out: io.Discard}) }
)

// PromptPath returns one check per implemented hop for the config tree at root.
func PromptPath(root string) []launch.Check {
	return []launch.Check{
		hop("H1 CLAUDE.md", claudeMD(root)),
		hop("H2 rules", rules(root)),
		hop("H3 skills", skills(root)),
		hop("H4 plan template", planTemplate(root)),
		hop("H5 workflows", workflows(root)),
		hop("H6 agents", agents(root)),
		hop("H7 session prompts", sessionPrompts(root)),
	}
}

func hop(name string, findings []string) launch.Check {
	if len(findings) == 0 {
		return launch.Check{Name: name, OK: true, Hard: true, Detail: "ok"}
	}
	return launch.Check{Name: name, Hard: true, Detail: strings.Join(findings, "\n")}
}

func lines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

func claudeMD(root string) []string {
	ls, err := lines(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		return []string{"CLAUDE.md:1: unreadable: " + err.Error() + " (fix at CLAUDE.md)"}
	}
	var out []string
	if n := len(ls) - 1; n > claudeMDCap {
		out = append(out, fmt.Sprintf("CLAUDE.md:%d: %d lines exceeds the %d cap (fix at CLAUDE.md)", n, n, claudeMDCap))
	}
	for i, l := range ls {
		for _, m := range backtickPath.FindAllStringSubmatch(l, -1) {
			if p := strings.TrimRight(m[1], "/"); !strings.ContainsAny(p, "*<>") && !exists(root, p) {
				out = append(out, fmt.Sprintf("CLAUDE.md:%d: `%s` does not exist (fix at CLAUDE.md)", i+1, m[1]))
			}
		}
	}
	return out
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

func rules(root string) []string {
	files, _ := filepath.Glob(filepath.Join(root, "rules", "*", "*.md"))
	more, _ := filepath.Glob(filepath.Join(root, "rules", "*", "*", "*.md"))
	var out []string
	for _, f := range append(files, more...) {
		rel, _ := filepath.Rel(root, f)
		ls, err := lines(f)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(rel, "rules/standards/") && !hasPaths(ls) {
			out = append(out, rel+":1: no `paths:` frontmatter (fix at "+rel+")")
		}
		out = append(out, symbolPairs(root, rel, ls)...)
	}
	return out
}

func hasPaths(ls []string) bool {
	if len(ls) == 0 || ls[0] != "---" {
		return false
	}
	for _, l := range ls[1:] {
		if l == "---" {
			return false
		}
		if strings.HasPrefix(l, "paths:") {
			return true
		}
	}
	return false
}

// symbolPairs checks every `sym` (`file.go`) pair names a symbol in that file.
func symbolPairs(root, rel string, ls []string) []string {
	var out []string
	for i, l := range ls {
		for _, m := range symPair.FindAllStringSubmatch(l, -1) {
			src := m[2]
			if strings.Contains(src, "/") && !exists(root, src) {
				continue
			}
			if !fileNamesSymbol(root, src, m[1]) {
				out = append(out, fmt.Sprintf("%s:%d: `%s` not found in `%s` (fix at %s)", rel, i+1, m[1], src, rel))
			}
		}
	}
	return out
}

func fileNamesSymbol(root, src, sym string) bool {
	sym = sym[strings.LastIndex(sym, ".")+1:]
	var cands []string
	if strings.Contains(src, "/") {
		cands = []string{filepath.Join(root, src)}
	} else {
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Name() == src {
				cands = append(cands, p)
			}
			return nil
		})
	}
	for _, c := range cands {
		if b, err := os.ReadFile(c); err == nil && strings.Contains(string(b), sym) {
			return true
		}
	}
	return len(cands) == 0
}

func skills(root string) []string {
	dirs, _ := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	var out []string
	for _, f := range dirs {
		dir := filepath.Base(filepath.Dir(f))
		if dir == "synced" || isSymlink(filepath.Dir(f)) {
			continue
		}
		rel, _ := filepath.Rel(root, f)
		out = append(out, frontName(f, rel, dir)...)
	}
	return out
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func frontName(path, rel, want string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := nameLine.FindSubmatch(b)
	if m == nil {
		return []string{rel + ":1: missing name (fix at " + rel + ")"}
	}
	if string(m[1]) != want {
		return []string{fmt.Sprintf("%s:1: name %q != %q (fix at %s)", rel, m[1], want, rel)}
	}
	return nil
}

func agents(root string) []string {
	files, _ := filepath.Glob(filepath.Join(root, "agents", "*.md"))
	var out []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		stem := strings.TrimSuffix(filepath.Base(f), ".md")
		out = append(out, frontName(f, rel, stem)...)
		b, _ := os.ReadFile(f)
		head := string(b)
		if i := strings.Index(head[min(4, len(head)):], "\n---"); i >= 0 {
			head = head[:i+4]
		}
		if !descLine.MatchString(head) {
			out = append(out, rel+":1: no description (fix at "+rel+")")
		}
		if m := modelLine.FindStringSubmatch(head); m != nil && !modelOK.MatchString(m[1]) {
			out = append(out, fmt.Sprintf("%s:1: model %q invalid (fix at %s)", rel, m[1], rel))
		}
	}
	return out
}

// planTemplate checks the plan template carries every heading the plan gate requires.
func planTemplate(root string) []string {
	path := plans.TemplatePath(root)
	rel, _ := filepath.Rel(root, path)
	ls, err := lines(path)
	if err != nil {
		return []string{fmt.Sprintf("%s:1: unreadable: %v (fix at %s)", rel, err, rel)}
	}
	var out []string
	for _, want := range docs.PlanTemplateHeadings() {
		if !hasHeading(ls, want) {
			out = append(out, fmt.Sprintf("%s:1: template lacks %q (fix at %s)", rel, want, rel))
		}
	}
	return out
}

func hasHeading(ls []string, want string) bool {
	for _, l := range ls {
		t := strings.TrimSpace(l)
		if t == want || strings.HasPrefix(t, want+" ") {
			return true
		}
	}
	return false
}

// workflows reports saved-workflow doc and helper-block drift, each as file:1 with its origin.
func workflows(root string) []string {
	var out []string
	for _, m := range planlint.WorkflowDrift(root) {
		head, what, _ := strings.Cut(m, ": ")
		origin := head
		if head == "saved-workflows.md" {
			origin = "skills/subagent-workflows/references/saved-workflows.md"
		} else if strings.HasPrefix(head, root+string(filepath.Separator)) {
			origin, _ = filepath.Rel(root, head)
		}
		out = append(out, fmt.Sprintf("%s:1: %s (fix at %s)", origin, what, origin))
	}
	return out
}

// sessionPrompts runs memory-start and memory-recall on the probe payload and checks the agents projection is clean.
func sessionPrompts(root string) []string {
	start := &core.Input{SessionID: probeSession, CWD: root, HookEventName: "SessionStart", Source: "startup"}
	recall := &core.Input{SessionID: probeSession, CWD: root, HookEventName: "UserPromptSubmit", Prompt: probePrompt}
	var out []string
	out = append(out, missingPaths("memory-start", memoryStart(start).Context)...)
	out = append(out, missingPaths("memory-recall", memoryRecall(recall).Context)...)
	if rc := agentsSync(); rc != 0 {
		out = append(out, fmt.Sprintf("agents sync --check exits %d:1: projection differs from agents/ (fix at agents/)", rc))
	}
	return out
}

// missingPaths returns one finding per absolute or ~ path the hook context names that does not exist.
func missingPaths(hook, ctx string) []string {
	var out []string
	for _, m := range namedPath.FindAllStringSubmatch(ctx, -1) {
		p := m[1]
		if strings.HasPrefix(p, "~") {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		if _, err := os.Stat(p); err != nil {
			out = append(out, fmt.Sprintf("%s:1: context names %s which does not exist (fix at the memory entry that names it)", hook, m[1]))
		}
	}
	return out
}
