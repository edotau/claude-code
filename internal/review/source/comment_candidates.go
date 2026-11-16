package source

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// commentCandidate is one comment worth cleaning up: kind is stacked, long, restates or trailing.
type commentCandidate struct {
	line         int
	kind, symbol string
	text         string
}

// commentFiller are words a restating doc comment adds around the symbol's own name.
var commentFiller = map[string]bool{"a": true, "an": true, "the": true, "is": true, "are": true, "of": true, "for": true,
	"to": true, "and": true, "returns": true, "return": true, "this": true, "that": true, "new": true, "get": true,
	"gets": true, "set": true, "sets": true, "creates": true, "create": true, "makes": true, "holds": true,
	"func": true, "type": true, "struct": true, "field": true, "var": true, "const": true, "function": true}

// camelWords lower-cases a symbol's camelCase and snake_case words.
func camelWords(name string) map[string]bool {
	words := map[string]bool{}
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words[strings.ToLower(string(cur))] = true
			cur = cur[:0]
		}
	}
	for i, r := range name {
		if r == '_' || (i > 0 && unicode.IsUpper(r)) {
			flush()
		}
		if r != '_' {
			cur = append(cur, r)
		}
	}
	flush()
	return words
}

// restatesSymbol: a one-line doc that starts with the symbol and adds only its own words and filler.
func restatesSymbol(name, doc string) bool {
	fields := strings.Fields(doc)
	if len(fields) == 0 || !strings.EqualFold(strings.Trim(fields[0], ":,."), name) {
		return false
	}
	own := camelWords(name)
	for _, w := range fields[1:] {
		w = strings.ToLower(strings.Trim(w, ".,:;()`"))
		if w != "" && !own[w] && !commentFiller[w] {
			return false
		}
	}
	return true
}

// goCommentCandidates lists the cleanup candidates of one Go source text; unparsable source yields none.
func goCommentCandidates(path, text string) []commentCandidate {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || isGeneratedGo(file) {
		return nil
	}
	out := blockCandidates(text, file, fset)
	out = append(out, restateCandidates(file, fset)...)
	out = append(out, trailingCandidates(text, file, fset)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

var generatedHeader = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// isGeneratedGo reports the Go generated-file marker in a comment line before the package clause.
func isGeneratedGo(file *ast.File) bool {
	for _, g := range file.Comments {
		if g.End() > file.Package {
			break
		}
		for _, c := range g.List {
			if generatedHeader.MatchString(c.Text) {
				return true
			}
		}
	}
	return false
}

// declSymbols maps each declaration's start line to its name, for the symbol column.
func declSymbols(file *ast.File, fset *token.FileSet) map[int]string {
	syms := map[int]string{}
	line := func(p token.Pos) int { return fset.Position(p).Line }
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			syms[line(v.Pos())] = v.Name.Name
		case *ast.TypeSpec:
			syms[line(v.Pos())] = v.Name.Name
		case *ast.ValueSpec:
			syms[line(v.Pos())] = v.Names[0].Name
		case *ast.Field:
			if len(v.Names) > 0 {
				syms[line(v.Pos())] = v.Names[0].Name
			}
		}
		return true
	})
	return syms
}

func blockCandidates(text string, file *ast.File, fset *token.FileSet) []commentCandidate {
	syms := declSymbols(file, fset)
	var out []commentCandidate
	for _, b := range scanCommentBlocks(text, "go") {
		after := b.start + len(b.lines)
		for _, s := range blockSmells(b, "go") {
			c := commentCandidate{kind: "stacked", line: b.start, symbol: syms[after]}
			if prose := proseLines(b, "go"); len(prose) > 0 {
				c.text = fmt.Sprintf("%d lines: %s", len(prose), prose[0])
			}
			if s.Type == "long_comment_line" {
				c.kind = "long"
				_, _ = fmt.Sscanf(s.Location, "line %d", &c.line)
				c.text = strings.TrimSpace(b.lines[c.line-b.start])
			}
			out = append(out, c)
		}
	}
	return out
}

func restateCandidates(file *ast.File, fset *token.FileSet) []commentCandidate {
	var out []commentCandidate
	check := func(name string, doc *ast.CommentGroup) {
		if doc == nil || name == "" || ast.IsExported(name) || len(doc.List) != 1 || !strings.HasPrefix(doc.List[0].Text, "//") {
			return
		}
		if t := strings.TrimSpace(strings.TrimPrefix(doc.List[0].Text, "//")); restatesSymbol(name, t) {
			out = append(out, commentCandidate{line: fset.Position(doc.Pos()).Line, kind: "restates", symbol: name, text: t})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			check(v.Name.Name, v.Doc)
		case *ast.GenDecl:
			if len(v.Specs) == 1 && v.Lparen == token.NoPos {
				if ts, ok := v.Specs[0].(*ast.TypeSpec); ok {
					check(ts.Name.Name, v.Doc)
				}
				if vs, ok := v.Specs[0].(*ast.ValueSpec); ok {
					check(vs.Names[0].Name, v.Doc)
				}
			}
		case *ast.TypeSpec:
			check(v.Name.Name, v.Doc)
		case *ast.ValueSpec:
			check(v.Names[0].Name, v.Doc)
		case *ast.Field:
			if len(v.Names) > 0 {
				check(v.Names[0].Name, v.Doc)
			}
		}
		return true
	})
	return out
}

func trailingCandidates(text string, file *ast.File, fset *token.FileSet) []commentCandidate {
	lines := strings.Split(text, "\n")
	declared := declaredLines(file, fset)
	var out []commentCandidate
	for _, g := range file.Comments {
		for _, c := range g.List {
			pos := fset.Position(c.Pos())
			if !strings.HasPrefix(c.Text, "//") || pos.Line > len(lines) || pos.Column > len(lines[pos.Line-1])+1 {
				continue
			}
			if strings.TrimSpace(lines[pos.Line-1][:pos.Column-1]) == "" || textHasDirective(c.Text) || declared(pos.Line) {
				continue
			}
			out = append(out, commentCandidate{line: pos.Line, kind: "trailing", symbol: enclosingFunc(file, fset, pos.Line),
				text: strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))})
		}
	}
	return out
}

// declaredLines reports whether a line's innermost enclosing node is a field, package const/var spec or literal key.
func declaredLines(file *ast.File, fset *token.FileSet) func(line int) bool {
	topSpecs := map[ast.Node]bool{}
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
			for _, sp := range gd.Specs {
				topSpecs[sp] = true
			}
		}
	}
	return func(line int) bool {
		inDecl := false
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil || fset.Position(n.Pos()).Line > line || fset.Position(n.End()).Line < line {
				return false
			}
			switch n.(type) {
			case *ast.BlockStmt:
				inDecl = false
			case *ast.StructType, *ast.InterfaceType, *ast.KeyValueExpr:
				inDecl = true
			default:
				if topSpecs[n] {
					inDecl = true
				}
			}
			return true
		})
		return inDecl
	}
}

func textHasDirective(c string) bool {
	return strings.HasPrefix(c, "//go:") || strings.HasPrefix(c, "//nolint") || strings.HasPrefix(c, "//lint:") ||
		strings.HasPrefix(c, "// +build") || strings.HasPrefix(c, "//line ")
}

func enclosingFunc(file *ast.File, fset *token.FileSet, line int) string {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fset.Position(fn.Pos()).Line <= line && line <= fset.Position(fn.End()).Line {
			return fn.Name.Name
		}
	}
	return ""
}

// RunCommentCandidates prints the TSV candidate list for the Go files under paths; it never needs git.
func RunCommentCandidates(paths []string, stdout, stderr io.Writer) int {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		switch {
		case err != nil:
			fmt.Fprintf(stderr, "review quality: %v\n", err)
			return 1
		case st.IsDir():
			walkSourceFiles(p, stderr, func(f string) {
				if strings.HasSuffix(f, ".go") {
					files = append(files, f)
				}
			})
		default:
			files = append(files, p)
		}
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "review quality: %v\n", err)
			return 1
		}
		for _, c := range goCommentCandidates(f, string(data)) {
			fmt.Fprintf(stdout, "%s:%d\t%s\t%s\t%s\n", f, c.line, c.kind, c.symbol, strings.ReplaceAll(c.text, "\t", " "))
		}
	}
	return 0
}
