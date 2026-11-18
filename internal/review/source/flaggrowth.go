package source

import (
	"fmt"
	"go/ast"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// flagNameArg is the argument index of the flag name for each flag-declaring method of a FlagSet.
var flagNameArg = map[string]int{
	"String": 0, "Bool": 0, "Int": 0, "Int64": 0, "Uint": 0, "Uint64": 0, "Float64": 0, "Duration": 0,
	"Func": 0, "BoolFunc": 0, "StringVar": 1, "BoolVar": 1, "IntVar": 1, "Int64Var": 1, "UintVar": 1,
	"Uint64Var": 1, "Float64Var": 1, "DurationVar": 1, "Var": 1, "TextVar": 1,
}

// helperFlagArg is the name argument (after the FlagSet) of helptext's flag helpers; -1 means a fixed name.
var helperFlagArg = map[string]int{"ListVar": 2, "NameVar": 2, "FormatFlag": -1, "SecretFlag": -1}

// flagGrowthRows is `flags: <verb> +n −m` per verb whose declared flag set differs between base and head.
func flagGrowthRows(pairs []goPair) []string {
	delta := map[string][2]int{}
	for _, p := range pairs {
		if strings.HasSuffix(p.path, "_test.go") {
			continue
		}
		base, head := declaredFlags(p.base), declaredFlags(p.head)
		for k := range maps.Keys(head) {
			bump(delta, k, 0, max(head[k]-base[k], 0))
		}
		for k := range maps.Keys(base) {
			bump(delta, k, 1, max(base[k]-head[k], 0))
		}
	}
	var rows []string
	for _, verb := range slices.Sorted(maps.Keys(delta)) {
		if d := delta[verb]; d[0]+d[1] > 0 {
			rows = append(rows, fmt.Sprintf("flags: %s +%d −%d", verb, d[0], d[1]))
		}
	}
	return rows
}

// bump adds n to the verb of key (verb NUL flag); a zero n still registers the verb.
func bump(delta map[string][2]int, key string, side, n int) {
	verb, _, _ := strings.Cut(key, "\x00")
	d := delta[verb]
	d[side] += n
	delta[verb] = d
}

// declaredFlags counts flag declarations per verb NUL flag name; a nil file has none.
func declaredFlags(f *ast.File) map[string]int {
	counts := map[string]int{}
	if f == nil {
		return counts
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			countFuncFlags(fd, counts)
		}
	}
	return counts
}

// flagSets maps each flag set variable to its verb: the NewFlagSet literal, or the func name for a parameter.
func flagSets(fd *ast.FuncDecl) map[string]string {
	sets := map[string]string{}
	for _, f := range fd.Type.Params.List {
		for _, n := range f.Names {
			if isFlagSetType(f.Type) {
				sets[n.Name] = fd.Name.Name
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, isID := as.Lhs[0].(*ast.Ident)
		if name, named := newFlagSetName(as.Rhs[0]); isID && named {
			sets[id.Name] = name
		}
		return true
	})
	return sets
}

func countFuncFlags(fd *ast.FuncDecl, counts map[string]int) {
	sets := flagSets(fd)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if verb, name, ok := flagDecl(call, sets); ok {
				counts[verb+"\x00"+name]++
			}
		}
		return true
	})
}

func isFlagSetType(t ast.Expr) bool {
	star, ok := t.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "FlagSet"
}

func newFlagSetName(e ast.Expr) (string, bool) {
	call, ok := e.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "NewFlagSet" {
		return "", false
	}
	return stringLit(call.Args[0])
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// flagDecl reports the flag set and flag name of a declaring call on a tracked flag set.
func flagDecl(call *ast.CallExpr, sets map[string]string) (verb, name string, ok bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return
	}
	if id, isID := sel.X.(*ast.Ident); isID {
		if idx, known := flagNameArg[sel.Sel.Name]; known && sets[id.Name] != "" {
			return sets[id.Name], argName(call, idx), true
		}
	}
	idx, helper := helperFlagArg[sel.Sel.Name]
	if !helper || len(call.Args) == 0 {
		return
	}
	id, isID := call.Args[0].(*ast.Ident)
	if !isID || sets[id.Name] == "" {
		return
	}
	if idx < 0 {
		return sets[id.Name], strings.ToLower(strings.TrimSuffix(sel.Sel.Name, "Flag")), true
	}
	return sets[id.Name], argName(call, idx), true
}

func argName(call *ast.CallExpr, idx int) string {
	if idx < len(call.Args) {
		if s, ok := stringLit(call.Args[idx]); ok {
			return s
		}
	}
	return "?"
}
