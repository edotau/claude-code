package inspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const findFileLimit = 1 << 20
const findSniffBytes = 8 << 10

// FindDefaultMax is the default result-line budget.
const FindDefaultMax = 200

// FindOptions configures a search over tracked and unignored files from any directory inside a repository.
type FindOptions struct {
	Dir, Pattern                                            string
	Paths                                                   []string
	Name, Func, Def, Callers, IgnoreCase, Word, Files, JSON bool
	Max                                                     int
	Out, Err                                                io.Writer
}

// FindMatch is one matching source line or path.
type FindMatch struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// FindReport uses More as a lower bound because the search stops at the first omitted result.
type FindReport struct {
	Matches []FindMatch `json:"matches"`
	More    int         `json:"more"`
}

// Find returns 0 for matches, 1 for none, 2 for errors; unparseable Go candidates are listed on Err.
func Find(o FindOptions) int {
	rep, err := findReport(o)
	if err == nil {
		err = renderFind(o, rep)
	}
	if err != nil {
		fmt.Fprintf(o.Err, "agents find: %v\n", err)
		return 2
	}
	if len(rep.Matches) == 0 {
		return 1
	}
	return 0
}

func findRegexp(o FindOptions) (*regexp.Regexp, error) {
	modes := 0
	for _, enabled := range []bool{o.Name, o.Func, o.Def, o.Callers} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return nil, errors.New("choose only one of --name, --func, --def, --callers")
	}
	if o.Max < 1 {
		return nil, errors.New("--max must be positive")
	}
	pattern := o.Pattern
	if o.Callers {
		pattern = "^(?:" + regexp.QuoteMeta(pattern) + ")$"
	}
	if o.Word {
		pattern = `\b(?:` + pattern + `)\b`
	}
	if o.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern: %w", err)
	}
	return re, nil
}

func findReport(o FindOptions) (FindReport, error) {
	rep := FindReport{Matches: []FindMatch{}}
	re, err := findRegexp(o)
	if err != nil {
		return rep, err
	}
	root, err := repoRoot(o.Dir)
	if err != nil {
		return rep, err
	}
	files, err := findSearchFiles(root, o)
	if err != nil {
		return rep, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return rep, err
	}
	defer r.Close()
	results, stop := findFiles(r, files, o, re)
	defer stop()
	for i, p := range files {
		<-results[i].ready
		matches, err := results[i].matches, results[i].err
		if skip, ok := errors.AsType[*findSkip](err); ok {
			fmt.Fprintf(o.Err, "find: skipped %s: %v\n", p, skip.error)
			continue
		}
		if err != nil {
			return rep, fmt.Errorf("%s: %w", p, err)
		}
		for _, match := range matches {
			if len(rep.Matches) == o.Max {
				rep.More = 1
				return rep, nil
			}
			rep.Matches = append(rep.Matches, match)
		}
	}
	return rep, nil
}

type findResult struct {
	matches []FindMatch
	err     error
	ready   chan struct{}
}

// findFiles scans on a worker pool, results read in input order as a serial scan; stop ends it and waits.
func findFiles(r *os.Root, files []string, o FindOptions, re *regexp.Regexp) ([]findResult, func()) {
	out := make([]findResult, len(files))
	for i := range out {
		out[i].ready = make(chan struct{})
	}
	var next atomic.Int64
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), len(files)) {
		wg.Go(func() {
			for !stopped.Load() {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				out[i].matches, out[i].err = findFile(r, files[i], o, re)
				close(out[i].ready)
			}
		})
	}
	return out, func() { stopped.Store(true); wg.Wait() }
}

func findTracked(root string, o FindOptions) ([]string, error) {
	sel, err := cleanPaths(root, o.Paths)
	if err != nil {
		return nil, err
	}
	raw, err := proc.RunGitRaw(context.Background(), root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	files := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	slices.Sort(files)
	files = slices.Compact(files)
	files = slices.DeleteFunc(files, func(p string) bool {
		return p == "" || (len(sel) > 0 && !inPaths(sel, p)) ||
			(findGoMode(o) && !strings.HasSuffix(p, ".go"))
	})
	return files, nil
}

func findSearchFiles(root string, o FindOptions) ([]string, error) {
	if !findLiteral(o) {
		return findTracked(root, o)
	}
	var hits findHits
	var wg sync.WaitGroup
	wg.Go(func() { hits = findGitHits(root, o) })
	files, err := findTracked(root, o)
	wg.Wait()
	return hits.filter(files), err
}

func findPrefilter(root string, files []string, o FindOptions) []string {
	if !findLiteral(o) {
		return files
	}
	return findGitHits(root, o).filter(files)
}

func findGoMode(o FindOptions) bool { return o.Func || o.Def || o.Callers }

type findHits map[string]bool

func (hits findHits) filter(files []string) []string {
	if hits == nil {
		return files
	}
	return slices.DeleteFunc(files, func(p string) bool { return !hits[p] })
}

// A nil result keeps the full scan on git errors; exit 1 means no candidates.
func findGitHits(root string, o FindOptions) findHits {
	args := []string{"grep", "-l", "-z", "-F", "--untracked"}
	if o.IgnoreCase {
		args = append(args, "--ignore-case")
	}
	args = append(append(args, "-e", o.Pattern, "--"), findPathspecs(root, o)...)
	raw, err := proc.RunGitRaw(context.Background(), root, args...)
	if exit, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || exit.ExitCode() != 1) {
		return nil
	}
	hits := map[string]bool{}
	for p := range strings.SplitSeq(raw, "\x00") {
		hits[p] = true
	}
	return hits
}

// findPathspecs scopes git grep to a superset of findTracked's files; globs and "." fall back to the Go suffix alone.
func findPathspecs(root string, o FindOptions) []string {
	sel, err := cleanPaths(root, o.Paths)
	specs := make([]string, 0, len(sel))
	for _, s := range sel {
		if s == "." || strings.ContainsAny(s, "*?") {
			specs = nil
			break
		}
		specs = append(specs, ":(top,literal)"+s)
	}
	if err == nil && len(specs) > 0 {
		return specs
	}
	if findGoMode(o) {
		return []string{":(top)*.go"}
	}
	return nil
}

// findLiteral reports a content search whose pattern git -F can match as Go would; -i skips K, S and non-ASCII (Go folds them).
func findLiteral(o FindOptions) bool {
	if o.Name || o.Pattern == "" ||
		regexp.QuoteMeta(o.Pattern) != o.Pattern || strings.ContainsAny(o.Pattern, "\r\n\x00") {
		return false
	}
	return !o.IgnoreCase || !strings.ContainsFunc(o.Pattern, func(c rune) bool { return c >= 0x80 || strings.ContainsRune("kKsS", c) })
}

func findFile(root *os.Root, p string, o FindOptions, re *regexp.Regexp) ([]FindMatch, error) {
	if o.Name && !re.MatchString(p) {
		return nil, nil
	}
	data, err := readFindTracked(root, p, o.Name)
	if err != nil || data == nil {
		return nil, err
	}
	if o.Name {
		return []FindMatch{{Path: p}}, nil
	}
	if findGoMode(o) {
		return findGo(p, data, o, re)
	}
	return findContent(p, data, o, re), nil
}

func findContent(p string, data []byte, o FindOptions, re *regexp.Regexp) []FindMatch {
	matches := []FindMatch{}
	content := bytes.Split(data, []byte{'\n'})
	for i, line := range content {
		if i == len(content)-1 && len(line) == 0 {
			break
		}
		if !re.Match(line) {
			continue
		}
		matches = append(matches, FindMatch{Path: p, Line: i + 1, Text: string(line)})
		if o.Files {
			return []FindMatch{{Path: p}}
		}
		if len(matches) > o.Max {
			break
		}
	}
	return matches
}

// readFindTracked reads p through os.Root, so a symlinked parent leaving the repo is skipped like a missing file.
func readFindTracked(root *os.Root, p string, sniffOnly bool) ([]byte, error) {
	rel := filepath.FromSlash(p)
	st, err := root.Lstat(rel)
	if err != nil || !st.Mode().IsRegular() || st.Size() > findFileLimit {
		return nil, nil
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, nil
	}
	return readFindFile(f, sniffOnly)
}

func readFindFile(f io.ReadCloser, sniffOnly bool) (data []byte, err error) {
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()
	data, err = io.ReadAll(io.LimitReader(f, findSniffBytes))
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, nil
	}
	if sniffOnly || len(data) < findSniffBytes {
		return data, nil
	}
	rest, err := io.ReadAll(io.LimitReader(f, findFileLimit+1-int64(len(data))))
	if err != nil {
		return nil, err
	}
	if len(data)+len(rest) > findFileLimit {
		return nil, nil
	}
	return append(data, rest...), nil
}

func renderFind(o FindOptions, rep FindReport) error {
	if o.JSON {
		return helptext.WriteJSON(o.Out, rep)
	}
	for _, m := range rep.Matches {
		var err error
		if m.Line == 0 {
			_, err = fmt.Fprintln(o.Out, m.Path)
		} else {
			_, err = fmt.Fprintf(o.Out, "%s:%d:%s\n", m.Path, m.Line, m.Text)
		}
		if err != nil {
			return err
		}
	}
	if rep.More > 0 {
		_, err := fmt.Fprintf(o.Out, "... %d more (raise --max or narrow --path)\n", rep.More)
		return err
	}
	return nil
}

// findSkip marks a file that could not be parsed; the search skips it instead of failing.
type findSkip struct{ error }

func findGo(p string, data []byte, o FindOptions, re *regexp.Regexp) ([]FindMatch, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, data, parser.SkipObjectResolution)
	if err != nil {
		if list, ok := err.(scanner.ErrorList); ok && len(list) > 0 {
			err = list[0]
		}
		return nil, &findSkip{err}
	}
	lines := map[int]bool{}
	add := func(id *ast.Ident) {
		if id != nil && re.MatchString(id.Name) {
			lines[fset.PositionFor(id.Pos(), false).Line] = true
		}
	}
	if o.Callers {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				add(findCalledName(call.Fun))
			}
			return true
		})
	} else {
		for _, decl := range file.Decls {
			findDeclaration(decl, o.Def, add)
		}
	}
	return findGoLines(p, data, lines, o), nil
}

func findGoLines(p string, data []byte, lines map[int]bool, o FindOptions) []FindMatch {
	ordered := make([]int, 0, len(lines))
	for line := range lines {
		ordered = append(ordered, line)
	}
	slices.Sort(ordered)
	matches := []FindMatch{}
	source := strings.Split(string(data), "\n")
	for _, line := range ordered {
		if o.Files {
			return []FindMatch{{Path: p}}
		}
		matches = append(matches, FindMatch{Path: p, Line: line, Text: source[line-1]})
		if len(matches) > o.Max {
			break
		}
	}
	return matches
}

func findCalledName(expr ast.Expr) *ast.Ident {
	switch n := expr.(type) {
	case *ast.Ident:
		return n
	case *ast.SelectorExpr:
		return n.Sel
	case *ast.IndexExpr:
		return findCalledName(n.X)
	case *ast.IndexListExpr:
		return findCalledName(n.X)
	case *ast.ParenExpr:
		return findCalledName(n.X)
	}
	return nil
}

func findDeclaration(decl ast.Decl, all bool, add func(*ast.Ident)) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		add(d.Name)
	case *ast.GenDecl:
		if !all {
			return
		}
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				add(s.Name)
			case *ast.ValueSpec:
				for _, name := range s.Names {
					add(name)
				}
			}
		}
	}
}
