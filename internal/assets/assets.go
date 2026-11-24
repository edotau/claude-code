// Package assets owns every file embedded in the claude-code binary.
package assets

import (
	"embed"
	"fmt"
	"strings"
	"text/template/parse"
)

//go:embed files
var files embed.FS

// tmplFields is a template's named inputs; a missing name panics like missingkey=error.
type tmplFields map[string]any

// Parse-only on purpose: executing text/template links MethodByName, keeping all methods (+11 MB hookd).
func parseTemplate(name string, src []byte) *parse.ListNode {
	trees, err := parse.Parse(name, string(src), "", "")
	if err != nil {
		panic("assets: " + name + ": " + err.Error())
	}
	return trees[name].Root
}

func renderTemplate(name string, root *parse.ListNode, dot any) string {
	var out strings.Builder
	execTemplate(&out, name, root, dot)
	return out.String()
}

// execTemplate covers the subset the embedded templates use: text, {{.}}, {{.Field}}, if/else and range.
func execTemplate(out *strings.Builder, name string, list *parse.ListNode, dot any) {
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
				execTemplate(out, name, n.List, dot)
			} else {
				execTemplate(out, name, n.ElseList, dot)
			}
		case *parse.RangeNode:
			rangeTemplate(out, name, n, dot)
		default:
			panic("assets: " + name + ": unsupported template node " + node.String())
		}
	}
}

func rangeTemplate(out *strings.Builder, name string, n *parse.RangeNode, dot any) {
	items, ok := templateValue(name, n.Pipe, dot).([]string)
	if !ok {
		panic("assets: " + name + ": range needs []string")
	}
	for _, item := range items {
		execTemplate(out, name, n.List, item)
	}
	if len(items) == 0 {
		execTemplate(out, name, n.ElseList, dot)
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
