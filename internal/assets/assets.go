// Package assets owns every file embedded in the claude-code binary.
package assets

import (
	"embed"
	"fmt"
	"path"
	"strings"
	"text/template/parse"
)

//go:embed files
var files embed.FS

// tmplFields is a template's named inputs; a missing name panics like missingkey=error.
type tmplFields map[string]any

// Parse-only on purpose: executing text/template links MethodByName, keeping all methods (+11 MB hookd).
func parseTemplate(name string, src []byte) *parse.ListNode {
	return parseTrees(name, src)[name].Root
}

func parseTrees(name string, src []byte) map[string]*parse.Tree {
	trees, err := parse.Parse(name, string(src), "", "")
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	return trees
}

// parseTemplates keys a file's trees: the file by name (unless only defines), each define as "<stem>/<block>".
func parseTemplates(name string, src []byte) map[string]*parse.ListNode {
	trees := parseTrees(name, src)
	stem := templateStem(name)
	roots := make(map[string]*parse.ListNode, len(trees))
	for key, tree := range trees {
		switch {
		case key != name:
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

func renderTemplate(name string, root *parse.ListNode, dot any) string {
	var out strings.Builder
	execTemplate(&out, nil, name, root, dot)
	return out.String()
}

// execTemplate covers the subset the embedded templates use: text, {{.}}, {{.Field}}, if/else, range and template.
func execTemplate(out *strings.Builder, set map[string]*parse.ListNode, name string, list *parse.ListNode, dot any) {
	if list == nil {
		return
	}
	for _, node := range list.Nodes {
		switch n := node.(type) {
		case *parse.TextNode:
			out.Write(n.Text)
		case *parse.ActionNode:
			fmt.Fprint(out, templateValue(name, n.Pipe, dot))
		case *parse.IfNode:
			if templateTruth(name, templateValue(name, n.Pipe, dot)) {
				execTemplate(out, set, name, n.List, dot)
			} else {
				execTemplate(out, set, name, n.ElseList, dot)
			}
		case *parse.RangeNode:
			rangeTemplate(out, set, name, n, dot)
		case *parse.TemplateNode:
			callTemplate(out, set, name, n, dot)
		default:
			panic("assets: " + name + ": unsupported template node " + node.String())
		}
	}
}

// callTemplate resolves a bare block name against the caller's file first, then as a full set key.
func callTemplate(out *strings.Builder, set map[string]*parse.ListNode, name string, n *parse.TemplateNode, dot any) {
	key := templateStem(name) + "/" + n.Name
	root, ok := set[key]
	if !ok {
		key = n.Name
		root, ok = set[key]
	}
	if !ok {
		panic("assets: " + name + ": no template " + n.Name)
	}
	var arg any
	if n.Pipe != nil {
		arg = templateValue(name, n.Pipe, dot)
	}
	execTemplate(out, set, key, root, arg)
}

func rangeTemplate(out *strings.Builder, set map[string]*parse.ListNode, name string, n *parse.RangeNode, dot any) {
	items, ok := templateValue(name, n.Pipe, dot).([]string)
	if !ok {
		panic("assets: " + name + ": range needs []string")
	}
	for _, item := range items {
		execTemplate(out, set, name, n.List, item)
	}
	if len(items) == 0 {
		execTemplate(out, set, name, n.ElseList, dot)
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
