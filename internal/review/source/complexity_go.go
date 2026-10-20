package source

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
)

// analyzeGo scores Go source with go/ast; a parse error yields no findings (fail-open).
func analyzeGo(path, text string, lineCount int, th ComplexityThresholds) []Finding {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var findings []Finding
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			findings = append(findings, goFunctionFindings(fset, fn, th)...)
		}
	}
	if imports := len(file.Imports); imports > th.MaxImports {
		findings = append(findings, Finding{Rule: "import-count", Severity: "warn",
			Message: fmt.Sprintf("%d imports (max %d). High coupling?", imports, th.MaxImports)})
	}
	return findings
}

func goFunctionFindings(fset *token.FileSet, fn *ast.FuncDecl, th ComplexityThresholds) []Finding {
	start, end := fset.Position(fn.Pos()), fset.Position(fn.End())
	name, line := fn.Name.Name, start.Line
	var out []Finding
	if length := end.Line - start.Line + 1; length > th.MaxFunctionLines {
		out = append(out, Finding{Rule: "function-length", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' is %d lines (max %d). Split it.", name, length, th.MaxFunctionLines)})
	}
	if cyclo := goCyclomatic(fn.Body); cyclo > th.MaxCyclomatic {
		out = append(out, Finding{Rule: "cyclomatic-complexity", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' has cyclomatic complexity %d (max %d). Flatten branching.", name, cyclo, th.MaxCyclomatic)})
	}
	if nest := goNesting(fn.Body, 0); nest > th.MaxNesting {
		out = append(out, Finding{Rule: "nesting-depth", Severity: "warn", Line: line,
			Message: fmt.Sprintf("'%s' nests %d levels deep (max %d). Use early returns.", name, nest, th.MaxNesting)})
	}
	return out
}

// goCyclomatic is 1 + if/for/range/case/comm clauses + short-circuit operators.
func goCyclomatic(body *ast.BlockStmt) int {
	score := 1
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.CommClause:
			score++
		case *ast.CaseClause:
			if v.List != nil { // default adds no branch
				score++
			}
		case *ast.BinaryExpr:
			if v.Op == token.LAND || v.Op == token.LOR {
				score++
			}
		}
		return true
	})
	return score
}

// goNesting returns the deepest count of enclosing if/for/range/switch/select blocks.
func goNesting(root ast.Node, depth int) int {
	deepest := depth
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil || n == root {
			return n == root
		}
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if d := goNesting(n, depth+1); d > deepest {
				deepest = d
			}
			return false
		}
		return true
	})
	return deepest
}
