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

// flagCall is one flag declaration: its verb, name ("?" when not a literal) and the name literal, nil for a fixed name.
type flagCall struct {
	verb, name string
	lit        ast.Expr
}

// eachFlagDecl calls visit for every flag declaration made through a tracked flag set in f's functions.
func eachFlagDecl(f *ast.File, visit func(flagCall)) {
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		sets := flagSets(fd)
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if fc, ok := flagDecl(call, sets); ok {
					visit(fc)
				}
			}
			return true
		})
	}
}

// declaredFlags counts flag declarations per verb NUL flag name; a nil file has none.
func declaredFlags(f *ast.File) map[string]int {
	counts := map[string]int{}
	if f != nil {
		eachFlagDecl(f, func(fc flagCall) { counts[fc.verb+"\x00"+fc.name]++ })
	}
	return counts
}

// commandLine keys the package-level flag.CommandLine set in flagSets, attributed to the enclosing func.
const commandLine = "flag.CommandLine"

// flagSets maps each flag set variable to its verb: the NewFlagSet literal, or the func name for a parameter.
func flagSets(fd *ast.FuncDecl) map[string]string {
	sets := map[string]string{commandLine: fd.Name.Name}
	for _, f := range fd.Type.Params.List {
		for _, n := range f.Names {
			if isFlagSetType(f.Type) {
				sets[n.Name] = fd.Name.Name
			}
		}
	}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		var lhs, rhs ast.Expr
		switch d := n.(type) {
		case *ast.AssignStmt:
			if len(d.Lhs) == 1 && len(d.Rhs) == 1 {
				lhs, rhs = d.Lhs[0], d.Rhs[0]
			}
		case *ast.ValueSpec:
			if len(d.Names) == 1 && len(d.Values) == 1 {
				lhs, rhs = d.Names[0], d.Values[0]
			}
		}
		id, isID := lhs.(*ast.Ident)
		if name, named := newFlagSetName(rhs); isID && named {
			sets[id.Name] = name
		}
		return true
	})
	return sets
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

// setKey names a flag set expression: an identifier, or flag.CommandLine.
func setKey(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok && id.Name == "flag" && v.Sel.Name == "CommandLine" {
			return commandLine
		}
	}
	return ""
}

// flagDecl reports the declaration of a call on a tracked flag set.
func flagDecl(call *ast.CallExpr, sets map[string]string) (flagCall, bool) {
	sel, isSel := call.Fun.(*ast.SelectorExpr)
	if !isSel {
		return flagCall{}, false
	}
	if idx, known := flagNameArg[sel.Sel.Name]; known && sets[setKey(sel.X)] != "" {
		return nameAt(sets[setKey(sel.X)], call, idx), true
	}
	idx, helper := helperFlagArg[sel.Sel.Name]
	if !helper || len(call.Args) == 0 || sets[setKey(call.Args[0])] == "" {
		return flagCall{}, false
	}
	verb := sets[setKey(call.Args[0])]
	if idx < 0 {
		return flagCall{verb: verb, name: strings.ToLower(strings.TrimSuffix(sel.Sel.Name, "Flag"))}, true
	}
	return nameAt(verb, call, idx), true
}

func nameAt(verb string, call *ast.CallExpr, idx int) flagCall {
	if idx < len(call.Args) {
		if s, ok := stringLit(call.Args[idx]); ok {
			return flagCall{verb: verb, name: s, lit: call.Args[idx]}
		}
	}
	return flagCall{verb: verb, name: "?"}
}
