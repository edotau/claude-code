package cli

import (
	"errors"
	"flag"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/opencode"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// opencodeMenuVerbs are the `opencode catalog` subverbs this file owns; bare `catalog` stays the CLI's own catalog.
var opencodeMenuVerbs = []string{"list", "add", "remove", "import", "reset", "check", "sync"}

func isOpencodeMenuVerb(name string, args []string) bool {
	return name == "opencode" && len(args) > 0 && slices.Contains(opencodeMenuVerbs, args[0])
}

func opencodeMenuPage(verb string) helptext.Page {
	usage := map[string][2]string{
		"list": {"list [--live] [--profile P] [--format F]",
			"Show the menu opencode offers: id, label, Responses, source."},
		"add": {"add <id>[=Label] [flags]",
			"Add or update an entry; refuses ids the workspace does not serve."},
		"remove": {"remove <id>", "Drop an entry from the menu."},
		"import": {"import <file> [--apply]",
			"Replace the menu from an opencode.json or `id[,Label][,responses]` lines; dry run unless --apply."},
		"reset": {"reset", "Delete the vault menu and fall back to the built-in one."},
		"check": {"check [--all | <id>...] [flags]",
			"Probe endpoints with one tiny request: status, latency, tool calls."},
		"sync": {"sync --profile P [flags]",
			"Preview serving models from authenticated cache or discovery; --apply writes."},
	}[verb]
	return helptext.Page{Usage: "claude-code opencode catalog " + usage[0], About: usage[1]}
}

func opencodeMenuFlags(verb string) *flag.FlagSet {
	return helptext.NewFlagSet("opencode catalog "+verb, opencodeMenuPage(verb))
}

// opencodeSyncArgs validates `opencode catalog sync`'s arguments.
func opencodeSyncArgs(pos []string, apply, dryRun bool, provider string) error {
	if len(pos) != 0 {
		return errors.New("takes no positional arguments")
	}
	if apply && dryRun {
		return errors.New("--apply and --dry-run are mutually exclusive")
	}
	if strings.TrimSpace(provider) == "" {
		return errors.New("--profile is required")
	}
	return nil
}

// menuOpts are the flag values of one `opencode catalog` verb.
type menuOpts struct {
	verb                                                   string
	live, force, apply, responses, dryRun, refresh, verify bool
	all                                                    bool
	provider                                               string
	timeout                                                time.Duration
	format                                                 *string
	fs                                                     *flag.FlagSet
}

func (m *menuOpts) fail(f string, a ...any) int { return fail("opencode catalog "+m.verb+": "+f, a...) }

func (m *menuOpts) done(err error) int {
	if err != nil {
		return m.fail("%v", err)
	}
	return 0
}

// bind registers the flags the verb takes: the shared ones first, then its own.
func (m *menuOpts) bind() {
	fs, verb := m.fs, m.verb
	if verb == "list" || verb == "check" {
		m.format = helptext.FormatFlag(fs)
	}
	if verb == "list" || verb == "check" || verb == "add" {
		fs.StringVar(&m.provider, "profile", "", "workspace profile (default: opencode's record profile)")
	}
	m.bindVerb()
}

func (m *menuOpts) bindVerb() {
	fs := m.fs
	switch m.verb {
	case "list":
		fs.BoolVar(&m.live, "live", false, "mark each entry served or missing on the workspace")
	case "check":
		fs.BoolVar(&m.all, "all", false, "probe every chat endpoint, not just the menu")
		fs.DurationVar(&m.timeout, "timeout", m.timeout, "per-request timeout")
	case "add":
		fs.BoolVar(&m.responses, "responses", false, "serve through the Responses API")
		fs.BoolVar(&m.force, "force", false, "skip the workspace check")
	case "import":
		fs.BoolVar(&m.apply, "apply", false, "write the change (default: print the diff)")
	case "sync":
		fs.StringVar(&m.provider, "profile", "", "Databricks serving profile (required)")
		fs.BoolVar(&m.apply, "apply", false, "seal discovery and persist menu additions")
		fs.BoolVar(&m.dryRun, "dry-run", false, "preview additions without writing (default)")
		fs.BoolVar(&m.refresh, "refresh", false, "rediscover instead of using cached models")
		fs.BoolVar(&m.verify, "verify", false, "probe eligible models; exclude failed additions")
	}
}

// cmdOpencodeMenu is `claude-code opencode catalog list|add|remove|import|reset|check|sync`.
func cmdOpencodeMenu(args []string) int {
	verb, rest := args[0], args[1:]
	m := &menuOpts{verb: verb, timeout: 30 * time.Second, format: new(string), fs: opencodeMenuFlags(verb)}
	m.bind()
	pos, err := helptext.ParseArgs(m.fs, rest)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if verb == "list" || verb == "check" {
		if err := helptext.CheckFormat(*m.format); err != nil {
			return badFormat("opencode catalog "+verb, err)
		}
	}
	return m.run(pos)
}

func (m *menuOpts) run(pos []string) int {
	switch m.verb {
	case "sync":
		return m.sync(pos)
	case "list":
		return m.done(opencode.ListMenu(os.Stdout, m.live, m.provider, *m.format))
	case "check":
		return m.check(pos)
	case "add":
		return m.add(pos)
	case "remove":
		return m.remove(pos)
	case "import":
		if len(pos) != 1 {
			return m.fail("usage: claude-code opencode catalog import <file> [--apply]")
		}
		return m.done(opencode.ImportMenu(os.Stdout, pos[0], m.apply))
	}
	if len(pos) != 0 {
		return m.fail("takes no arguments")
	}
	return m.done(opencode.WriteMenu(os.Stdout, nil, "reset to the built-in menu"))
}

func (m *menuOpts) sync(pos []string) int {
	if err := opencodeSyncArgs(pos, m.apply, m.dryRun, m.provider); err != nil {
		return m.done(err)
	}
	return m.done(opencode.SyncMenu(os.Stdout, m.provider, opencode.SyncOptions{Apply: m.apply, Refresh: m.refresh, Verify: m.verify}))
}

func (m *menuOpts) check(pos []string) int {
	if m.all && len(pos) > 0 {
		return m.fail("--all and explicit ids are exclusive")
	}
	return m.done(opencode.CheckMenu(os.Stdout, os.Stderr, pos, m.all, m.provider, m.timeout, *m.format))
}

func (m *menuOpts) add(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog add <id>[=Label]")
	}
	set := false
	m.fs.Visit(func(f *flag.Flag) { set = set || f.Name == "responses" })
	return m.done(opencode.AddMenu(os.Stdout, pos[0], m.responses, set, m.force, m.provider))
}

func (m *menuOpts) remove(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog remove <id>")
	}
	next, ok := opencode.MenuRemove(providers.OpencodeMenu(), pos[0])
	if !ok {
		return m.fail("%s is not in the menu", opencode.MenuID(pos[0]))
	}
	return m.done(opencode.WriteMenu(os.Stdout, next, "removed "+opencode.MenuID(pos[0])))
}
