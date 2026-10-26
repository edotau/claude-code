package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/providers"
)

// opencodeMenuVerbs are the `opencode models` subverbs this file owns; bare `models` stays the CLI's own catalog.
var opencodeMenuVerbs = []string{"list", "add", "remove", "import", "reset"}

func isOpencodeMenuVerb(vendor string, args []string) bool {
	return vendor == "opencode" && len(args) > 0 && slices.Contains(opencodeMenuVerbs, args[0])
}

func opencodeMenuPage(verb string) helptext.Page {
	usage := map[string][2]string{
		"list": {"list [--live] [--provider P] [--format F]",
			"Show the menu opencode offers: id, label, Responses, source."},
		"add": {"add <id>[=Label] [flags]",
			"Add or update an entry; refuses ids the workspace does not serve."},
		"remove": {"remove <id>", "Drop an entry from the menu."},
		"import": {"import <file> [--apply]",
			"Replace the menu from an opencode.json or `id[,Label][,responses]` lines; dry run unless --apply."},
		"reset": {"reset", "Delete the vault menu and fall back to the built-in one."},
	}[verb]
	return helptext.Page{Usage: "claude-code opencode models " + usage[0], About: usage[1]}
}

func opencodeMenuFlags(verb string) *flag.FlagSet {
	return helptext.NewFlagSet("opencode models "+verb, opencodeMenuPage(verb))
}

// cmdOpencodeMenu is `claude-code opencode models list|add|remove|import|reset`.
func cmdOpencodeMenu(args []string) int {
	verb, rest := args[0], args[1:]
	fs := opencodeMenuFlags(verb)
	var live, force, apply, responses bool
	var provider string
	format := new(string)
	if verb == "list" {
		format = outfmt.Flag(fs)
	}
	switch verb {
	case "list":
		fs.BoolVar(&live, "live", false, "mark each entry served or missing on the workspace")
		fs.StringVar(&provider, "provider", "", "workspace provider (default: opencode's env)")
	case "add":
		fs.BoolVar(&responses, "responses", false, "serve through the Responses API")
		fs.BoolVar(&force, "force", false, "skip the workspace check")
		fs.StringVar(&provider, "provider", "", "workspace provider (default: opencode's env)")
	case "import":
		fs.BoolVar(&apply, "apply", false, "write the change (default: print the diff)")
	}
	pos, err := outfmt.Parse(fs, rest)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if verb == "list" {
		if err := outfmt.Check(*format); err != nil {
			return fail("opencode models list: %v", err)
		}
	}
	fail := func(f string, a ...any) int { return fail("opencode models "+verb+": "+f, a...) }
	switch verb {
	case "list":
		return opencodeMenuList(live, provider, *format, fail)
	case "add":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode models add <id>[=Label]")
		}
		set := false
		fs.Visit(func(f *flag.Flag) { set = set || f.Name == "responses" })
		return opencodeMenuAdd(pos[0], responses, set, force, provider, fail)
	case "remove":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode models remove <id>")
		}
		next, ok := providers.MenuRemove(providers.OpencodeMenu(), pos[0])
		if !ok {
			return fail("%s is not in the menu", providers.MenuID(pos[0]))
		}
		return opencodeMenuWrite(next, fmt.Sprintf("removed %s", providers.MenuID(pos[0])), fail)
	case "import":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode models import <file> [--apply]")
		}
		return opencodeMenuImport(pos[0], apply, fail)
	}
	if len(pos) != 0 {
		return fail("takes no arguments")
	}
	return opencodeMenuWrite(nil, "reset to the built-in menu", fail)
}

// opencodeMenuWrite stores next. opencode's databricks config is built from the menu at every launch (and
// codex's catalog likewise), so there is no rendered file to refresh; the next launch picks the change up.
func opencodeMenuWrite(next []providers.OpencodeModel, msg string, fail func(string, ...any) int) int {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("opencode models: %s; relaunch `claude-code opencode` to apply\n", msg)
	return 0
}

// opencodeProvider resolves the workspace provider: --provider, else the opencode row's account.
func opencodeProvider(name string) (*providers.Registry, *providers.Provider, error) {
	reg, err := providers.Load()
	if err != nil {
		return nil, nil, err
	}
	if name == "" {
		row, _ := reg.AgentRow("opencode")
		if name = row.Account; name == "" {
			return nil, nil, fmt.Errorf("no provider on the opencode row; pass --provider")
		}
	}
	p, err := reg.Get(name)
	return reg, p, err
}

// opencodeServed lists the workspace's endpoints by name.
func opencodeServed(name string) (map[string]providers.ModelInfo, *providers.Provider, error) {
	reg, p, err := opencodeProvider(name)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	infos, err := providers.ListModels(ctx, reg, p)
	if err != nil {
		return nil, p, err
	}
	out := make(map[string]providers.ModelInfo, len(infos))
	for _, m := range infos {
		out[m.ID] = m
	}
	return out, p, nil
}

func servedInfo(served map[string]providers.ModelInfo, p *providers.Provider, id string) (providers.ModelInfo, bool) {
	if m, ok := served[p.ModelPrefix+id]; ok {
		return m, true
	}
	m, ok := served[id]
	return m, ok
}

type ocMenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
}

func opencodeMenuList(live bool, provider, format string, fail func(string, ...any) int) int {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	if live {
		var err error
		if served, p, err = opencodeServed(provider); err != nil {
			return fail("%v", err)
		}
	}
	var rows []ocMenuRow
	for _, m := range providers.OpencodeMenu() {
		r := ocMenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := servedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
		}
		rows = append(rows, r)
	}
	if format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, rows)
		return 0
	}
	idW, labW := len("ID"), len("LABEL")
	for _, r := range rows {
		idW, labW = max(idW, len(r.ID)), max(labW, len(r.Label))
	}
	head := fmt.Sprintf("%-*s  %-*s  %-9s  %s", idW, "ID", labW, "LABEL", "RESPONSES", "SOURCE")
	if live {
		head += "  LIVE"
	}
	fmt.Println(head)
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-9v  %s", idW, r.ID, labW, r.Label, r.Responses, r.Source)
		if live {
			line += "  " + strings.TrimSpace(r.Live+" "+r.Ready)
		}
		fmt.Println(line)
	}
	return 0
}

func opencodeMenuAdd(arg string, responses, responsesSet, force bool, provider string, fail func(string, ...any) int) int {
	id, label, _ := strings.Cut(arg, "=")
	id = providers.MenuID(id)
	if id == "" {
		return fail("empty model id")
	}
	if !force {
		served, p, err := opencodeServed(provider)
		if err != nil {
			return fail("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := servedInfo(served, p, id); !ok {
			return fail("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return opencodeMenuWrite(providers.MenuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r),
		"added "+id, fail)
}

func opencodeMenuImport(file string, apply bool, fail func(string, ...any) int) int {
	data, err := os.ReadFile(file)
	if err != nil {
		return fail("%v", err)
	}
	next, err := providers.ParseMenuImport(data)
	if err != nil {
		return fail("%s: %v", file, err)
	}
	changes := providers.DiffMenu(providers.OpencodeMenu(), next)
	for _, c := range changes {
		line := fmt.Sprintf("%s %s  %s", c.Op, c.Model.ID, c.Model.Label)
		if c.Op == "~" {
			line += fmt.Sprintf("  (was %q responses=%v; now responses=%v)", c.Was.Label, c.Was.Responses, c.Model.Responses)
		}
		fmt.Println(line)
	}
	if len(changes) == 0 {
		fmt.Println("opencode models: no change")
		return 0
	}
	if !apply {
		fmt.Printf("opencode models: dry run, %d change(s); pass --apply to write\n", len(changes))
		return 0
	}
	return opencodeMenuWrite(next, fmt.Sprintf("imported %d model(s)", len(next)), fail)
}
