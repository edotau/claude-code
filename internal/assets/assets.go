// Package assets owns every file embedded in the claude-code binary.
package assets

import (
	"embed"
	"fmt"
	"path"
	"strconv"
	"strings"
	"text/template/parse"
)

//go:embed files
var files embed.FS

// tmplFields is a template's named inputs; a missing name panics like missingkey=error.
type tmplFields map[string]any

// parseRoot names the file's own tree while parsing, so a define named like the file stays a distinct tree.
const parseRoot = "\x00file"

// Parse-only on purpose: executing text/template links MethodByName, keeping all methods (+11 MB hookd).
func parseTrees(name string, src []byte) map[string]*parse.Tree {
	trees := map[string]*parse.Tree{}
	root := parse.New(parseRoot)
	root.Mode = parse.ParseComments
	if _, err := root.Parse(string(src), "", "", trees); err != nil {
		if strings.Contains(err.Error(), "multiple definition of template "+strconv.Quote(parseRoot)) {
			panic(reservedDefine(name))
		}
		panic("assets: " + name + ": " + strings.ReplaceAll(err.Error(), parseRoot, name))
	}
	if !parse.IsEmptyTree(trees[parseRoot].Root) && strings.Contains(string(src), "define") && definesParseRoot(src) {
		panic(reservedDefine(name))
	}
	return trees
}

func reservedDefine(name string) string {
	return "assets: " + name + ": a define may not use the reserved name " + strconv.Quote(parseRoot)
}

// definesParseRoot reparses under another root name: a define-only file owning parseRoot then still holds the key.
func definesParseRoot(src []byte) bool {
	trees := map[string]*parse.Tree{}
	_, err := parse.New(parseRoot+"probe").Parse(string(src), "", "", trees)
	_, owned := trees[parseRoot]
	return err == nil && owned
}

// parseTemplates keys a file's trees: the file by name (unless only defines), each define as "<stem>/<block>".
func parseTemplates(name string, src []byte) map[string]*parse.ListNode {
	trees := parseTrees(name, src)
	if _, shadow := trees[name]; shadow {
		panic("assets: " + name + ": a define may not reuse its file name")
	}
	stem := templateStem(name)
	roots := make(map[string]*parse.ListNode, len(trees))
	for key, tree := range trees {
		switch {
		case key != parseRoot:
			roots[stem+"/"+key] = tree.Root
		case len(trees) == 1 || !parse.IsEmptyTree(tree.Root):
			roots[name] = tree.Root
		}
	}
	return roots
}

// templateStem is the file part of a set key: "lane" for both "lane.tmpl" and "lane/block".
func templateStem(name string) string {
	if stem, _, ok := strings.Cut(name, "/"); ok {
		return stem
	}
	return strings.TrimSuffix(name, path.Ext(name))
}

// maxTemplateDepth bounds {{template}} nesting so a recursive block panics instead of overflowing the stack.
const maxTemplateDepth = 16

// renderedKey, renderedFields and renderedBranch are test seams (executed key; its top-level dot; {{if}} branch run).
var (
	renderedKey    func(key string)
	renderedFields func(key string, dot any)
	renderedBranch func(key string, n *parse.IfNode, then bool)
)

// tmplExec is one render's invariant state: the output builder and the file set being executed.
type tmplExec struct {
	out *strings.Builder
	set map[string]*parse.ListNode
}

// execTemplate covers the subset the embedded templates use: text, {{.}}, {{.Field}}, if/else, range and template.
func (t tmplExec) execTemplate(name string, list *parse.ListNode, dot any, depth int) {
	if list == nil {
		return
	}
	for _, node := range list.Nodes {
		switch n := node.(type) {
		case *parse.TextNode:
			t.out.Write(n.Text)
		case *parse.CommentNode:
		case *parse.ActionNode:
			fmt.Fprint(t.out, templateValue(name, n.Pipe, dot))
		case *parse.IfNode:
			t.ifTemplate(name, n, dot, depth)
		case *parse.RangeNode:
			t.rangeTemplate(name, n, dot, depth)
		case *parse.TemplateNode:
			t.callTemplate(name, n, dot, depth)
		default:
			panic("assets: " + name + ": unsupported template node " + node.String())
		}
	}
}

func (t tmplExec) ifTemplate(name string, n *parse.IfNode, dot any, depth int) {
	then := templateTruth(name, templateValue(name, n.Pipe, dot))
	if renderedBranch != nil {
		renderedBranch(name, n, then)
	}
	if then {
		t.execTemplate(name, n.List, dot, depth)
	} else {
		t.execTemplate(name, n.ElseList, dot, depth)
	}
}

// callTemplate resolves a bare block name against the caller's file first, then as a full set key.
func (t tmplExec) callTemplate(name string, n *parse.TemplateNode, dot any, depth int) {
	key := templateStem(name) + "/" + n.Name
	root, ok := t.set[key]
	if !ok {
		key = n.Name
		root, ok = t.set[key]
	}
	if !ok {
		panic("assets: " + name + ": no template " + n.Name)
	}
	if depth >= maxTemplateDepth {
		panic("assets: " + name + ": template " + n.Name + " nests deeper than the limit (recursive block?)")
	}
	if renderedKey != nil {
		renderedKey(key)
	}
	var arg any
	if n.Pipe != nil {
		arg = templateValue(name, n.Pipe, dot)
	}
	t.execTemplate(key, root, arg, depth+1)
}

func (t tmplExec) rangeTemplate(name string, n *parse.RangeNode, dot any, depth int) {
	items, ok := templateValue(name, n.Pipe, dot).([]string)
	if !ok {
		panic("assets: " + name + ": range needs []string")
	}
	for _, item := range items {
		t.execTemplate(name, n.List, item, depth)
	}
	if len(items) == 0 {
		t.execTemplate(name, n.ElseList, dot, depth)
	}
}

func templateValue(name string, pipe *parse.PipeNode, dot any) any {
	if len(pipe.Decl) != 0 || len(pipe.Cmds) != 1 || len(pipe.Cmds[0].Args) != 1 {
		panic("assets: " + name + ": unsupported pipeline " + pipe.String())
	}
	switch arg := pipe.Cmds[0].Args[0].(type) {
	case *parse.DotNode:
		return dot
	case *parse.FieldNode:
		fields, ok := dot.(tmplFields)
		if ok && len(arg.Ident) == 1 {
			if v, found := fields[arg.Ident[0]]; found {
				return v
			}
		}
		panic("assets: " + name + ": missing field " + arg.String())
	}
	panic("assets: " + name + ": unsupported argument " + pipe.String())
}

func templateTruth(name string, v any) bool {
	switch t := v.(type) {
	case string:
		return t != ""
	case bool:
		return t
	case int:
		return t != 0
	case []string:
		return len(t) != 0
	}
	panic(fmt.Sprintf("assets: %s: unsupported condition type %T", name, v))
}

// read panics on a missing name (a build defect); ReadFile already returns a fresh copy of the bytes.
func read(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	return b
}

// RankTable is the model rank table (tiers, rotation rules, families).
func RankTable() []byte { return read("files/catalog/ranks.json") }

// RetiredWords is the retired-word vocabulary the source review advises on.
func RetiredWords() []byte { return read("files/review/source/retired_words.json") }

// ThemeDefaultConfig is the statusline config.json default.
func ThemeDefaultConfig() []byte { return read("files/ui/theme/default_config.json") }

// ThemeDefaultPalettes is the statusline palette default.
func ThemeDefaultPalettes() []byte { return read("files/ui/theme/default_palettes.json") }
