// promptPath audits the instruction path a session follows (CLAUDE.md, rules, skills, agents).

package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/plans"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/hooks/docs"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/routing/agentconfig/project"
)

const claudeMDCap = 200

// Probe payload for H7; the hooks only read it, the session id is a fixed name.
const probeSession, probePrompt, probeRecallHits = "doctor-prompts-probe", "plan memory recall probe", 8

var (
	backtickPath = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("`((?:rules|skills|agents|commands|workflows|plans|internal|cmd)/[A-Za-z0-9_./-]+)`")
	})
	symPair = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_.]*)` \\(`([A-Za-z0-9_./-]+\\.go)`\\)")
	})
	nameLine  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^name:[ \t]*(\S+)[ \t]*$`) })
	descLine  = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^description:[ \t]*\S`) })
	modelLine = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^model:[ \t]*(\S+)[ \t]*$`) })
	modelOK   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`) })
	namedPath = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?:^|[\\s(\\[\"'`])((?:~|/)[A-Za-z0-9._@+/~-]*[A-Za-z0-9_])")
	})
)

// Seams for H7: production runs the real hooks and a scratch render; tests swap them for fixtures.
var (
	memoryStart  = memhook.StartText // side-effect free: an audit must not scaffold, spawn or consume
	memoryRecall = recallProbe
	agentsRender = project.CheckFreshRender
)

func promptPath(root string) []check {
	return []check{
		hop("H1 CLAUDE.md", claudeMD(root)),
		softHop("H1 instruction-index shapes", shapeFindings(root, "instruction-index")),
		hop("H2 rules", rules(root)),
		softHop("H2 rule shapes", shapeFindings(root, "rule")),
		hop("H3 skills", skills(root)),
		softHop("H3 skill shapes", skillShapes(root)),
		hop("H4 plan template", planTemplate(root)),
		softHop("H5 command shapes", shapeFindings(root, "command")),
		hop("H6 agents", agents(root)),
		hop("H7 session prompts", sessionPrompts(root)),
	}
}

func hop(name string, findings []string) check {
	if len(findings) == 0 {
		return check{Name: name, OK: true, Hard: true, Detail: "ok"}
	}
	return check{Name: name, Hard: true, Detail: strings.Join(findings, "\n")}
}

// softHop is hop for advisory findings: a soft "warn" row, never a doctor failure.
func softHop(name string, findings []string) check {
	if len(findings) == 0 {
		return check{Name: name, OK: true, Detail: "ok"}
	}
	return check{Name: name, Detail: fmt.Sprintf("warn %d advisory finding(s)\n%s", len(findings), strings.Join(findings, "\n"))}
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
		return []string{unreadable("CLAUDE.md", err)}
	}
	var out []string
	n := len(ls)
	if ls[n-1] == "" { // the final element after a trailing newline is not a line
		n--
	}
	if n > claudeMDCap {
		out = append(out, fmt.Sprintf("CLAUDE.md:%d: %d lines exceeds the %d cap (fix at CLAUDE.md)", n, n, claudeMDCap))
	}
	backtick := backtickPath()
	for i, l := range ls {
		for _, m := range backtick.FindAllStringSubmatch(l, -1) {
			if p := strings.TrimRight(m[1], "/"); !strings.ContainsAny(p, "*<>") && !paths.FileExists(filepath.Join(root, p)) && !gitIgnored(root, m[1]) {
				out = append(out, fmt.Sprintf("CLAUDE.md:%d: `%s` does not exist (fix at CLAUDE.md)", i+1, m[1]))
			}
		}
	}
	return out
}

func unreadable(rel string, err error) string {
	return fmt.Sprintf("%s:1: unreadable: %v (fix at %s)", rel, err, rel)
}

// gitIgnored reports a path .gitignore covers (e.g. skills/synced/); keep the trailing / so dir-only patterns match.
func gitIgnored(root, rel string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := proc.RunGitInherit(ctx, root, "check-ignore", "-q", "--no-index", rel)
	return err == nil
}

func rules(root string) []string {
	files, _ := filepath.Glob(filepath.Join(root, "rules", "*", "*.md"))
	more, _ := filepath.Glob(filepath.Join(root, "rules", "*", "*", "*.md"))
	var out []string
	idx := goFiles(root)
	for _, f := range append(files, more...) {
		rel, _ := filepath.Rel(root, f)
		ls, err := lines(f)
		if err != nil {
			out = append(out, unreadable(rel, err))
			continue
		}
		if !strings.HasPrefix(rel, "rules/standards/") && !hasPaths(ls) {
			out = append(out, rel+":1: no `paths:` frontmatter (fix at "+rel+")")
		}
		out = append(out, symbolPairs(root, rel, ls, idx)...)
	}
	return out
}

func hasPaths(ls []string) bool {
	if len(ls) == 0 || strings.TrimRight(ls[0], "\r") != "---" {
		return false
	}
	for _, l := range ls[1:] {
		if strings.TrimRight(l, " \t\r") == "---" {
			return false
		}
		if strings.HasPrefix(l, "paths:") {
			return true
		}
	}
	return false
}

// symbolPairs checks every `sym` (`file.go`) pair names an existing file that contains the symbol.
func symbolPairs(root, rel string, ls []string, idx map[string][]string) []string {
	var out []string
	pair := symPair()
	for i, l := range ls {
		for _, m := range pair.FindAllStringSubmatch(l, -1) {
			if why := symbolMiss(root, m[2], m[1], idx); why != "" {
				out = append(out, fmt.Sprintf("%s:%d: %s (fix at %s)", rel, i+1, why, rel))
			}
		}
	}
	return out
}

// symbolMiss says why sym is not in src (a bare name resolves through idx), or "" if a candidate has it as a whole word
func symbolMiss(root, src, sym string, idx map[string][]string) string {
	var cands []string
	switch {
	case !strings.Contains(src, "/"):
		cands = idx[src]
	case paths.FileExists(filepath.Join(root, src)):
		cands = []string{filepath.Join(root, src)}
	}
	if len(cands) == 0 {
		return fmt.Sprintf("`%s` does not exist", src)
	}
	short := regexp.MustCompile(`\b` + regexp.QuoteMeta(sym[strings.LastIndex(sym, ".")+1:]) + `\b`)
	for _, c := range cands {
		b, err := os.ReadFile(c)
		if err != nil {
			return fmt.Sprintf("`%s` unreadable: %v", src, err)
		}
		if short.Match(b) {
			return ""
		}
	}
	return fmt.Sprintf("`%s` not found in `%s`", sym, src)
}

// goFiles indexes the .go files under internal/ and cmd/ by base name, walked once per audit.
func goFiles(root string) map[string][]string {
	idx := map[string][]string{}
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") {
				idx[d.Name()] = append(idx[d.Name()], p)
			}
			return nil
		})
	}
	return idx
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
		b, err := os.ReadFile(f)
		if err != nil {
			out = append(out, unreadable(rel, err))
			continue
		}
		out = append(out, frontName(frontMatter(b), rel, dir)...)
	}
	return out
}

// skillShapes checks every skill against the shape table; the name rule is H3's hard row, so it is left out here.
func skillShapes(root string) []string {
	shape, ok := assets.ShapeByID("skill")
	if !ok {
		return nil
	}
	var out []string
	for _, f := range shape.Files(root) {
		b, err := os.ReadFile(f)
		if isSymlink(filepath.Dir(f)) || err != nil {
			continue // a symlinked dir is the user's own skill; an unreadable one is H3's finding
		}
		for _, fd := range shape.Validate(f, b) {
			if rel, _ := filepath.Rel(root, fd.Path); fd.Rule != "fm-stem" { // a companion finding names its own file
				out = append(out, fmt.Sprintf("%s:1: [%s] %s (fix at %s)", rel, fd.Rule, fd.Msg, rel))
			}
		}
	}
	return out
}

// shapeFindings formats advisory checks for a table row; hard rows retain their own checks.
func shapeFindings(root, id string) []string {
	shape, ok := assets.ShapeByID(id)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range shape.Files(root) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue // hard rows report unreadable files
		}
		for _, fd := range shape.Validate(f, b) {
			rel, _ := filepath.Rel(root, fd.Path)
			out = append(out, fmt.Sprintf("%s:1: [%s] %s (fix at %s)", rel, fd.Rule, fd.Msg, rel))
		}
	}
	return out
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// frontMatter is the text between the opening and closing "---" lines at the top of b, or "" when there is none.
func frontMatter(b []byte) string {
	rest, ok := strings.CutPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "---\n")
	if !ok {
		return ""
	}
	ls := strings.Split(rest, "\n")
	for i, l := range ls {
		if strings.TrimRight(l, " \t") == "---" {
			return strings.Join(ls[:i], "\n")
		}
	}
	return ""
}

func frontName(head, rel, want string) []string {
	m := nameLine().FindStringSubmatch(head)
	if m == nil {
		return []string{rel + ":1: missing name (fix at " + rel + ")"}
	}
	if m[1] != want {
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
		b, err := os.ReadFile(f)
		if err != nil {
			out = append(out, unreadable(rel, err))
			continue
		}
		head := frontMatter(b)
		out = append(out, frontName(head, rel, stem)...)
		if !descLine().MatchString(head) {
			out = append(out, rel+":1: no description (fix at "+rel+")")
		}
		if m := modelLine().FindStringSubmatch(head); m != nil && !modelOK().MatchString(m[1]) {
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
		return []string{unreadable(rel, err)}
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
	return slices.ContainsFunc(ls, func(l string) bool {
		t := strings.TrimSpace(l)
		return t == want || strings.HasPrefix(t, want+" ")
	})
}

// workflows reports saved-workflow doc and helper-block drift, each as file:1 with its origin.
// recallProbe is the memory-recall text for the probe prompt, read-only: it would consume the hook's seen-state/log.
func recallProbe(prompt string) (string, error) {
	hits := memory.SearchBank(prompt, probeRecallHits)
	var blocks []string
	for _, h := range hits {
		if h.Bank != "archive" { // rotated history may name paths that are gone, or another machine's
			blocks = append(blocks, h.Block)
		}
	}
	return strings.Join(blocks, "\n"), nil
}

// sessionPrompts runs memory-start and memory-recall on the probe payload and checks they render cleanly.
func sessionPrompts(root string) []string {
	start := &core.Input{SessionID: probeSession, CWD: root, HookEventName: "SessionStart", Source: "startup"}
	var out []string
	out = append(out, missingPaths("memory-start", memoryStart(start).Context)...)
	if recall, err := memoryRecall(probePrompt); err != nil {
		out = append(out, fmt.Sprintf("memory-recall:1: probe failed: %v (fix at the memory store)", err))
	} else {
		out = append(out, missingPaths("memory-recall", recall)...)
	}
	if err := agentsRender(root); err != nil {
		out = append(out, fmt.Sprintf("agents projection: %v (fix at agents/ or CLAUDE.md)", err))
	}
	return out
}

// missingPaths returns one finding per absolute or ~ path the hook context names that does not exist.
func missingPaths(hook, ctx string) []string {
	return missingPathsOn(hook, ctx, runtime.GOOS, paths.ExpandHome("~"))
}

// missingPathsOn is missingPaths with the OS and this machine's home root passed in, so tests can drive both.
func missingPathsOn(hook, ctx, goos, home string) []string {
	var out []string
	for _, ix := range namedPath().FindAllStringSubmatchIndex(ctx, -1) {
		name := ctx[ix[2]:ix[3]]
		if isPlaceholder(pathToken(ctx[ix[2]:])) {
			continue // a template like <lens>, a glob or a YYYY-MM-DD name, not a path to stat
		}
		p := paths.ExpandHome(name)
		if p == name && (p == "~" || strings.HasPrefix(p, "~/")) {
			out = append(out, fmt.Sprintf("%s:1: context names %s but the home directory cannot be resolved", hook, name))
			continue
		}
		if foreignHome(p, home, goos) {
			continue // a shared memory bank names the other OS's home (WSL /home on macOS, /Users on Linux)
		}
		if home != "" && strings.HasPrefix(p, strings.TrimSuffix(home, "/")+"/worktrees/") {
			continue // a linked worktree is removed when its lane lands; a note that names one is not stale
		}
		if top, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/"); func() bool { _, err := os.Stat("/" + top); return err != nil }() {
			continue // a URL route like /serving-endpoints or /chat/completions, not a filesystem path
		}
		if _, err := os.Stat(p); err != nil {
			out = append(out, fmt.Sprintf("%s:1: context names %s which does not exist (fix at the memory entry that names it)", hook, name))
		}
	}
	return out
}

// foreignHome reports an absolute path under another OS's home root and outside this machine's home.
func foreignHome(p, home, goos string) bool {
	if !strings.HasPrefix(p, "/") || (home != "~" && home != "" && strings.HasPrefix(p, strings.TrimSuffix(home, "/")+"/")) {
		return false
	}
	switch goos {
	case "darwin":
		return strings.HasPrefix(p, "/home/")
	case "linux":
		return strings.HasPrefix(p, "/Users/")
	case "windows":
		return strings.HasPrefix(p, "/home/") || strings.HasPrefix(p, "/Users/")
	}
	return false
}

// pathToken is the whole path word at the start of s; namedPath stops at the first placeholder character.
func pathToken(s string) string {
	if i := strings.IndexAny(s, " \t\r\n`\"')]"); i >= 0 {
		return s[:i]
	}
	return s
}

var placeholderRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[*?]|<[^>]*>|\{[^}]*\}`) })

var datePartRE = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9])(?:YYYY|MM|DD)(?:[^A-Za-z0-9]|$)`)
})

// isPlaceholder: <...>, {...}, a * or ? glob, or a YYYY/MM/DD segment of the file name.
func isPlaceholder(tok string) bool {
	return placeholderRE().MatchString(tok) || datePartRE().MatchString(filepath.Base(tok))
}
