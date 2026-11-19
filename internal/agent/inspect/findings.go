package inspect

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
)

// Finding is one row: `path:line: tool/rule: message`; path, line and rule are left out when the tool gave none.
type Finding struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Tool    string `json:"tool"`
	Rule    string `json:"rule,omitempty"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	where := ""
	if f.Path != "" {
		where = f.Path
		if f.Line > 0 {
			where += ":" + strconv.Itoa(f.Line)
		}
		where += ": "
	}
	tool := f.Tool
	if f.Rule != "" {
		tool += "/" + f.Rule
	}
	return where + tool + ": " + f.Message
}

// parseFn turns a tool's combined output into rows; a line it does not know becomes a row with the tool name.
type parseFn func(out string) []Finding

// step is one file kind's check: globs it covers, the command as a list ({files} and {pkgs} are the only placeholders).
type step struct {
	kind, gate string
	globs, cmd []string
	wholeCmd   []string // the command for --all without --path, when the tool's own config picks the files (mdsmith)
	install    string   // how the Makefile installs the tool; empty for a tool that is always present
	listsFiles bool     // the tool exits 0 and prints the files that need work (gofmt -l)
	optional   bool     // a missing tool skips even under --gates, as the Makefile always did for it
	parse      parseFn
	fix        []string // the command `agents fix` runs on {files}, the one `make fmt` runs for this kind; nil: no fixer
	dry        []string // what lists the files fix would rewrite, when cmd is not that (golangci-lint fmt -d)
	dryParse   parseFn  // reads dry's output; parse when nil
}

const (
	installDev = "make install DEV_TOOLS=1"
	goGlob     = "**/*.go"
	mdGlob     = "**/*.md"
)

// steps is the per-kind check table; `make lint` and `agents diff --check` both run exactly these commands.
var steps = []step{
	{kind: "go", gate: "gofmt", globs: []string{goGlob}, cmd: []string{"gofmt", "-l", "{files}"}, listsFiles: true,
		parse: parseGofmt, fix: []string{"gofmt", "-w", "{files}"}},
	{kind: "go", gate: "vet", globs: []string{goGlob}, cmd: []string{"go", "vet", "{pkgs}"}, parse: parseGoOutput("vet")},
	{kind: "go", gate: "golangci-lint", globs: []string{goGlob}, install: installDev, parse: parseGolangci,
		fix: []string{"golangci-lint", "fmt", "{files}"}, dry: []string{"golangci-lint", "fmt", "-d", "{files}"},
		dryParse: parseFmtDiff,
		cmd:      []string{"golangci-lint", "run", "--allow-parallel-runners", "--max-issues-per-linter=0", "--max-same-issues=0", "{pkgs}"}},
	{kind: "markdown", gate: "docs", globs: []string{mdGlob}, install: "make mdsmith", parse: parseDocs,
		cmd: []string{"mdsmith", "check", "--no-color", "--", "{files}"}, fix: []string{"mdsmith", "fix", "--no-color", "--", "{files}"},
		wholeCmd: []string{"mdsmith", "check", "--no-color"}},
	{kind: "yaml", gate: "yaml", globs: []string{"**/*.yml", "**/*.yaml"}, install: "make install", optional: true,
		parse: parseYamlfmt,
		cmd:   []string{"yamlfmt", "-lint", "{files}"}, fix: []string{"yamlfmt", "{files}"}},
	{kind: "shell", gate: "shell", globs: []string{"scripts/*.sh", "scripts/lib/*.sh", "skills/*/scripts/*.sh"},
		install: "install shellcheck; make does not", optional: true, parse: parseShellcheck,
		cmd: []string{"shellcheck", "-S", "error", "-f", "gcc", "{files}"}},
}

func stepFor(gate string) step {
	i := slices.IndexFunc(steps, func(s step) bool { return s.gate == gate })
	return steps[i]
}

// match keeps the paths any glob covers, in order.
func match(files, globs []string) []string {
	var out []string
	for _, p := range files {
		if slices.ContainsFunc(globs, func(g string) bool { return paths.GlobMatch(g, path.Clean(p)) }) {
			out = append(out, p)
		}
	}
	return out
}

// lineRule maps one line shape to a row; the regexp's groups feed build.
// lazyRE compiles its pattern on first use, so a process that never parses tool output pays nothing.
func lazyRE(expr string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(expr) })
}

type lineRule struct {
	re    func() *regexp.Regexp
	build func(m []string) Finding
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// parseLines applies the first matching rule per line; skip lines are tool chrome, any other line is kept as a row.
func parseLines(tool string, skip func() *regexp.Regexp, rules ...lineRule) parseFn {
	return func(out string) []Finding {
		var rows []Finding
		for l := range strings.Lines(out) {
			l = strings.TrimRight(l, " \t\r\n")
			if strings.TrimSpace(l) == "" || (skip != nil && skip().MatchString(l)) {
				continue
			}
			rows = append(rows, parseLine(tool, l, rules))
		}
		return sortRows(rows)
	}
}

func parseLine(tool, l string, rules []lineRule) Finding {
	for _, r := range rules {
		if m := r.re().FindStringSubmatch(l); m != nil {
			return r.build(m)
		}
	}
	return Finding{Tool: tool, Message: l}
}

// sortRows orders rows by path then line, keeping the tool's own order among equals.
func sortRows(rows []Finding) []Finding {
	slices.SortStableFunc(rows, func(a, b Finding) int { return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Line, b.Line)) })
	return rows
}

var (
	reCompile   = lazyRE(`^(?:vet: )?(.+?\.go):(\d+):\d+: (.*)$`)
	reGofmtName = lazyRE(`^.+\.go$`)
	reGolangci  = lazyRE(`^(.+?):(\d+):\d+: (.*) \(([\w-]+)\)$`)
	reCaret     = lazyRE(`^\s*\^\s*$`)
	reLintTail  = lazyRE(`^(\d+ issues?:|\* [\w-]+: \d+)$`)
	reMdsmith   = lazyRE(`^(.+?):(\d+):\d+ (MDS\d+) (.*)$`)
	reMemory    = lazyRE(`^(.+?\.md): memory fmt --check: (.*)$`)
	reMdChrome  = lazyRE(`^(\s*\d+ \| ?.*|[·\s]*\^|stats: .*)$`)
	reFmtDiff   = lazyRE(`^diff (.+)\.orig .+$`)
	reYamlHead  = lazyRE(`^(.+?\.ya?ml):$`)
	reYamlChrom = lazyRE(`^(The following formatting differences.*|- .*|\s.*)$`)
	reShell     = lazyRE(`^(.+?):(\d+):\d+: \w+: (.*) \[(SC\d+)\]$`)
	reComplex   = lazyRE(`^(.+?) line (\d+) \[(\w+)\] (\S+): (.*)$`)
	reSmell     = lazyRE(`^(\S+) (\S+) \[(\w+)\] (\S+): (.*)$`)
	reQuality   = lazyRE(`^(.+?):(\d+): (.*)$`)
	reQualEnd   = lazyRE(`^(\d+ new violation|no new )`)
	reTestPkg   = lazyRE(`^FAIL\t(\S+)\t`)
	reTestLine  = lazyRE(`^\s+(.+?\.go):(\d+): (.*)$`)
	reTestChrom = lazyRE(`^(ok\s|\?\s|FAIL$|=== |--- PASS|PASS$)`)
)

func compileRule(tool string) lineRule {
	return lineRule{reCompile, func(m []string) Finding { return Finding{m[1], atoi(m[2]), tool, "", m[3]} }}
}

// parseGoOutput reads `go build|vet` output: `# pkg` headers are chrome, `path:line:col: msg` rows.
func parseGoOutput(tool string) parseFn {
	return parseLines(tool, lazyRE(`^# `), compileRule(tool))
}

func parseGofmt(out string) []Finding {
	return parseLines("gofmt", nil, compileRule("gofmt"), lineRule{reGofmtName, func(m []string) Finding {
		return Finding{Path: m[0], Tool: "gofmt", Message: "needs formatting"}
	}})(out)
}

// parseGolangci reads golangci-lint's default text: each issue may be followed by its source line and a caret line.
func parseGolangci(out string) []Finding {
	var rows []Finding
	afterIssue := false
	for l := range strings.Lines(out) {
		l = strings.TrimRight(l, " \t\r\n")
		switch m := reGolangci().FindStringSubmatch(l); {
		case strings.TrimSpace(l) == "" || reLintTail().MatchString(l) || reCaret().MatchString(l):
			afterIssue = false
		case m != nil:
			rows, afterIssue = append(rows, Finding{m[1], atoi(m[2]), "golangci-lint", m[4], m[3]}), true
		case afterIssue && !reQuality().MatchString(l):
			afterIssue = false // the source line printed under an issue
		default:
			rows, afterIssue = append(rows, Finding{Tool: "golangci-lint", Message: l}), false
		}
	}
	return sortRows(rows)
}

var parseDocs = parseLines("docs", reMdChrome,
	lineRule{reMdsmith, func(m []string) Finding { return Finding{m[1], atoi(m[2]), "mdsmith", m[3], m[4]} }},
	lineRule{reMemory, func(m []string) Finding { return Finding{Path: m[1], Tool: "memory-fmt", Message: m[2]} }})

// parseFmtDiff reads `golangci-lint fmt -d`: one `diff a.go.orig a.go` header per file it would rewrite.
var parseFmtDiff = parseLines("golangci-lint", nil, lineRule{reFmtDiff, func(m []string) Finding {
	return Finding{Path: m[1], Tool: "golangci-lint", Message: "needs formatting"}
}})

var parseYamlfmt = parseLines("yamlfmt", reYamlChrom, lineRule{reYamlHead, func(m []string) Finding {
	return Finding{Path: m[1], Tool: "yamlfmt", Message: "needs formatting"}
}})

var parseShellcheck = parseLines("shellcheck", nil,
	lineRule{reShell, func(m []string) Finding { return Finding{m[1], atoi(m[2]), "shellcheck", m[4], m[3]} }})

// parseComplexity reads the complexity gate's own lines (see simplifyReport.findings).
var parseComplexity = parseLines("complexity", nil,
	lineRule{reComplex, func(m []string) Finding { return Finding{m[1], atoi(m[2]), "complexity", m[4], m[3] + " " + m[5]} }},
	lineRule{reSmell, func(m []string) Finding { return Finding{m[1], 0, "complexity", m[4], m[2] + " " + m[3] + " " + m[5]} }})

// parseQuality reads `review simplify --fail`: `path:line: message`, then a count line.
var parseQuality = parseLines("quality", reQualEnd,
	lineRule{reQuality, func(m []string) Finding { return Finding{m[1], atoi(m[2]), "quality", "", m[3]} }})

var parseTest = parseLines("test", reTestChrom,
	lineRule{reTestPkg, func(m []string) Finding { return Finding{Path: m[1], Tool: "test", Message: "FAIL"} }},
	lineRule{reTestLine, func(m []string) Finding { return Finding{m[1], atoi(m[2]), "test", "", m[3]} }})

// capRows keeps every parsed row but at most limit rows with no path, then one row counting the rest.
func capRows(tool string, rows []Finding, limit int) []Finding {
	var out []Finding
	extra := 0
	kept := 0
	for _, r := range rows {
		if r.Path == "" && kept >= limit {
			extra++
			continue
		}
		if r.Path == "" {
			kept++
		}
		out = append(out, r)
	}
	if extra > 0 {
		out = append(out, Finding{Tool: tool, Message: fmt.Sprintf("+%d more lines cut", extra)})
	}
	return out
}
