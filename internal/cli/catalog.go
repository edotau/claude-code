package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// catalogVerbs run for every agent; menuVerbs edit opencode's model menu and exist only there.
var (
	catalogVerbs = []string{"list", "check", "sync"}
	menuVerbs    = []string{"add", "remove", "import", "reset"}
)

// catalogSpec is the completion node for `<agent> catalog`: the subcommands every agent accepts and the bare flags.
func catalogSpec() *helptext.Node {
	return &helptext.Node{Args: []func() []string{func() []string { return catalogVerbs }},
		Flags: []helptext.Flag{helptext.ValuedFlag("profile", providers.ProfileNames), helptext.CompletionFormatFlag()}}
}

// catalogPage is `<agent> catalog -h`.
func catalogPage(name string) helptext.Page {
	s, _ := agent.LookupSchema(name)
	page := helptext.Page{Usage: "claude-code " + name + " catalog [list|check|sync] [flags]", Hint: s.CLI.CatalogHint,
		About: "On a Databricks profile (--profile, else " + name + "'s pin, else the session) bare catalog is list; " +
			"otherwise " + name + "'s own model catalog, or a note that it has none. TESTED is the last check on " +
			"that profile (ok, fail, transient), blank when never probed; every agent shares one check cache.",
		Commands: [][2]string{
			{"list", "READY chat endpoints " + name + " can use, with TESTED"},
			{"check", "probe them; --all: every chat endpoint; or name ids"},
			{"sync", "discover endpoints; --apply seals the snapshot"},
		}}
	switch {
	case name == "opencode":
		page.About = "On a Databricks profile bare catalog is list. add, remove, import and reset edit the " +
			"menu; claude-code opencode catalog <command> -h lists each one's flags."
		page.Commands = [][2]string{{"list", "the model menu; --live adds served/missing and last check"},
			{"check", "probe the menu; --all: every chat endpoint; or name ids"},
			{"sync", "discover endpoints; --apply seals, adds to the menu"}}
	case len(s.CLI.Dialects) == 0:
		page.About = name + " has no Databricks route: list|check|sync print a note; bare catalog is " + name + "'s own."
	}
	return page
}

// catalogOpts are the flag values of one `<agent> catalog [verb]` run.
type catalogOpts struct {
	name, verb, profile, format string
	all, apply, dryRun, refresh bool
	timeout                     time.Duration
}

// bind registers the flags verb ("" = bare) takes on a new FlagSet whose -h renders the catalog page.
func (o *catalogOpts) bind() *flag.FlagSet {
	fs := helptext.NewFlagSet(strings.TrimSpace(o.name+" catalog "+o.verb), catalogPage(o.name))
	fs.StringVar(&o.profile, "profile", "", "Databricks profile (default: "+o.name+"'s pin, else the session)")
	switch o.verb {
	case "check":
		fs.BoolVar(&o.all, "all", false, "probe every chat endpoint, not just the ones "+o.name+" can use")
		fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "per-request timeout")
	case "sync":
		fs.BoolVar(&o.apply, "apply", false, "seal the discovery snapshot in the vault")
		fs.BoolVar(&o.dryRun, "dry-run", false, "preview without writing (default)")
		fs.BoolVar(&o.refresh, "refresh", false, "rediscover instead of using the sealed snapshot")
		return fs
	}
	fs.StringVar(&o.format, "format", helptext.Text, "output format: text or json")
	return fs
}

// agentCatalog is `<agent> catalog [list|check|sync] [flags]`; opencode also takes add|remove|import|reset.
func agentCatalog(name string, args []string) int {
	verb := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		verb, args = args[0], args[1:]
	}
	if code, done := catalogVerbGate(name, verb, args); done {
		return code
	}
	o := &catalogOpts{name: name, verb: verb, format: helptext.Text}
	pos, err := helptext.ParseArgs(o.bind(), args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 0 && verb != "check" {
		return helptext.FailUsage("%s catalog: unexpected argument %q", name, pos[0])
	}
	if err := helptext.CheckFormat(o.format); err != nil {
		return helptext.BadFormat(name+" catalog", err)
	}
	return o.run(pos, args)
}

// catalogVerbGate hands opencode's verbs to its menu and refuses a verb name does not take; done ends the run.
func catalogVerbGate(name, verb string, args []string) (code int, done bool) {
	menu := slices.Contains(menuVerbs, verb)
	switch {
	case name == "opencode" && (menu || slices.Contains(catalogVerbs, verb)):
		return opencode.RunMenu(append([]string{verb}, args...)), true
	case menu:
		return helptext.FailUsage("%s catalog: %s edits opencode's model menu; only opencode has it", name, verb), true
	case verb != "" && !slices.Contains(catalogVerbs, verb):
		return helptext.FailUsage("%s catalog: unexpected argument %q", name, verb), true
	}
	return 0, false
}

// run resolves the profile and runs the verb on Databricks, else bare catalog's own listing.
func (o *catalogOpts) run(ids, args []string) int {
	s, _ := agent.LookupSchema(o.name)
	dialects := s.CLI.Dialects
	if len(dialects) == 0 {
		if o.verb == "" {
			return ask.Catalog(o.name, o.format, cmdModels)
		}
		fmt.Printf("%s catalog %s: %s has no Databricks route (it runs on its own login); nothing to %s\n",
			o.name, o.verb, o.name, o.verb)
		return 0
	}
	p, err := catalog.Profile(o.name, o.profile)
	if err != nil {
		return helptext.Fail("%s catalog: %v", o.name, err)
	}
	dbx := p != nil && p.Preset == providers.PresetDatabricks
	switch {
	case !dbx && o.verb == "":
		return ask.Catalog(o.name, o.format, cmdModels)
	case !dbx:
		return helptext.Fail("%s catalog %s: needs a Databricks profile; pass --profile", o.name, o.verb)
	case o.verb == "check":
		return o.check(p, dialects, ids)
	case o.verb == "sync":
		return o.done(o.sync(p, dialects))
	case o.name == "opencode":
		return opencode.RunMenu(append([]string{"list"}, args...))
	}
	return o.done(listCatalog(p, dialects, o.format))
}

func (o *catalogOpts) done(err error) int {
	if err != nil {
		return helptext.Fail("%s catalog %s: %v", o.name, cmp.Or(o.verb, "list"), err)
	}
	return 0
}

func listCatalog(p *providers.Provider, dialects []string, format string) error {
	rows, err := catalog.List(p, dialects)
	if err != nil {
		return err
	}
	return catalog.WriteList(os.Stdout, p.Name, rows, format)
}

func (o *catalogOpts) check(p *providers.Provider, dialects, ids []string) int {
	if o.all && len(ids) > 0 {
		return helptext.FailUsage("%s catalog check: --all and explicit ids are exclusive", o.name)
	}
	results, cacheErr, err := catalog.CheckUsable(p, dialects, ids, o.all, o.timeout)
	if err != nil {
		return o.done(err)
	}
	if cacheErr != nil {
		fmt.Fprintf(os.Stderr, "%s catalog check: cache not written: %v\n", o.name, cacheErr)
	}
	return o.done(catalog.WriteCheck(os.Stdout, results, o.format))
}

// sync discovers p's endpoints and, with --apply, seals the snapshot; no menu or other state is written.
func (o *catalogOpts) sync(p *providers.Provider, dialects []string) error {
	if o.apply && o.dryRun {
		return errors.New("--apply and --dry-run are mutually exclusive")
	}
	snapshot, err := serving.Discover(context.Background(), p, o.refresh)
	if err != nil {
		return err
	}
	for _, line := range catalog.SyncSummaryLines(snapshot, false) {
		fmt.Println(line)
	}
	usable := catalog.UsableIn(p, snapshot.Models, dialects)
	fmt.Printf("%s catalog sync: %d listed, %d usable by %s\n", o.name, len(snapshot.Models), len(usable), o.name)
	if !o.apply {
		fmt.Printf("%s catalog: dry run; no discovery writes; pass --apply to seal\n", o.name)
		return nil
	}
	sealed, err := catalog.Seal(p, snapshot, false)
	if err == nil && sealed {
		fmt.Printf("%s catalog: discovery snapshot sealed in vault\n", o.name)
	}
	return err
}
