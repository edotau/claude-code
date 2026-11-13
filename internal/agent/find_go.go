package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"strings"
)

func findGo(p string, data []byte, o FindOptions, re *regexp.Regexp) ([]FindMatch, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, data, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
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
