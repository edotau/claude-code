package cli

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/configure"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// catalogVerbs run for every agent; menuVerbs edit opencode's model menu and exist only there.
var (
	catalogVerbs = []string{"list", "check", "sync"}
	menuVerbs    = []string{"add", "remove", "import", "reset"}
)

// catalogSpec is the completion node for `<agent> catalog`: each shared verb with the flags its real flag set defines.
func catalogSpec(name string) *helptext.Node {
	sub := make(map[string]*helptext.Node, len(catalogVerbs))
	for _, v := range catalogVerbs {
		sub[v] = &helptext.Node{Flags: catalogFlags(name, v)}
	}
	return &helptext.Node{Sub: sub, Flags: catalogFlags(name, "")}
}

// catalogFlags lists the flags `<agent> catalog verb` parses (bare = ""); opencode's verbs bind their own set.
func catalogFlags(name, verb string) []helptext.Flag {
	values := map[string]func() []string{"profile": providers.ProfileNames}
	if name == "opencode" && verb != "" {
		m := &opencodeOpts{verb: verb, format: new(string), fs: opencodeMenuFlags(verb)}
		m.bind()
		return helptext.FlagsOf(m.fs, values)
	}
	return helptext.FlagsOf((&catalogOpts{name: name, verb: verb}).bind(), values)
}

// catalogPage is `<agent> catalog -h`.
func catalogPage(name string) helptext.Page {
	s, _ := agent.LookupSchema(name)
	page := helptext.Page{Usage: "claude-code " + name + " catalog [list|check|sync] [flags]", Hint: s.CLI.CatalogHint,
		About: "On a Databricks profile (--profile, else " + name + "'s pin, else the session) bare catalog is list; " +
			"otherwise " + name + "'s own model catalog, or a note that it has none. TESTED is the last check on " +
			"that profile (ok, fail, transient), blank when never probed; every agent shares one check cache. " +
			"claude-code " + name + " catalog <command> -h lists each one's flags.",
		Commands: [][2]string{
			{"list", "READY chat endpoints " + name + " can use, with TESTED"},
			{"check", "probe them; --all: every chat endpoint; or name ids"},
			{"sync", "discover endpoints; --apply seals the snapshot"},
		}}
	switch {
	case name == "opencode":
		page.Usage = "claude-code opencode catalog [<command>] [flags]"
		page.About = "On a Databricks profile bare catalog is list. add, remove, import and reset edit the " +
			"menu; claude-code opencode catalog <command> -h lists each one's flags."
		page.Commands = [][2]string{{"list", "the model menu; --live adds served/missing and last check"},
			{"add", "add or update an entry: id[=Label]"},
			{"remove", "drop an entry from the menu"},
			{"import", "replace the menu from a file; --apply writes"},
			{"reset", "fall back to the built-in menu"},
			{"check", "probe the menu; --all: every chat endpoint; or name ids"},
			{"sync", "discover endpoints; --apply seals, adds to the menu"}}
	case len(s.CLI.Dialects) == 0:
		page.About = name + " has no Databricks route: bare catalog is " + name + "'s own list; list, check and " +
			"sync only print a note."
		page.Commands = [][2]string{{"list", "print a note: no Databricks route"}, {"check", "print a note: no Databricks route"},
			{"sync", "print a note: no Databricks route"}}
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
		return runOpencodeMenu(append([]string{verb}, args...)), true
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
		return o.runOwnLogin()
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
		return o.done(o.sync(p))
	case o.name == "opencode":
		return runOpencodeMenu(append([]string{"list"}, args...))
	}
	return o.done(listCatalog(o.name, p, o.format))
}

// runOwnLogin is the catalog of an agent with no Databricks route: its own listing, or a no-op verb.
func (o *catalogOpts) runOwnLogin() int {
	if o.verb == "" {
		return ask.Catalog(o.name, o.format, cmdModels)
	}
	fmt.Printf("%s catalog %s: %s has no Databricks route (it runs on its own login); nothing to %s\n",
		o.name, o.verb, o.name, o.verb)
	return 0
}

func (o *catalogOpts) done(err error) int {
	if err != nil {
		return helptext.Fail("%s catalog %s: %v", o.name, cmp.Or(o.verb, "list"), err)
	}
	return 0
}

// syncMode is the discovery mode for a sync: --refresh always lists live.
func syncMode(refresh bool) configure.ModelMode {
	if refresh {
		return configure.RefreshModels
	}
	return configure.VerifiedModels
}

// discover runs the unified model discovery for name on profile.
func discover(name, profile string, mode configure.ModelMode) (configure.ModelResult, error) {
	return configure.DiscoverModels(context.Background(), configure.ModelQuery{Agent: name, Profile: profile, Mode: mode})
}

func listCatalog(name string, p *providers.Provider, format string) error {
	res, err := discover(name, p.Name, configure.RecallModels)
	if err != nil {
		return err
	}
	var names []string
	for _, m := range res.Models {
		names = append(names, m.ID)
	}
	return catalog.WriteList(os.Stdout, p.Name, catalog.Rows(p.Name, names), format)
}

func (o *catalogOpts) check(p *providers.Provider, dialects, ids []string) int {
	if o.all && len(ids) > 0 {
		return helptext.FailUsage("%s catalog check: --all and explicit ids are exclusive", o.name)
	}
	res, err := discover(o.name, p.Name, configure.RecallModels)
	if err != nil {
		return o.done(err)
	}
	results, cacheErr, err := catalog.CheckUsable(p, res.Raw, dialects, ids, o.all, o.timeout)
	if err != nil {
		return o.done(err)
	}
	if cacheErr != nil {
		fmt.Fprintf(os.Stderr, "%s catalog check: cache not written: %v\n", o.name, cacheErr)
	}
	return o.done(catalog.WriteCheck(os.Stdout, results, o.format))
}

// sync discovers p's endpoints and, with --apply, seals the snapshot; no menu or other state is written.
func (o *catalogOpts) sync(p *providers.Provider) error {
	if o.apply && o.dryRun {
		return errors.New("--apply and --dry-run are mutually exclusive")
	}
	res, err := discover(o.name, p.Name, syncMode(o.refresh))
	if err != nil {
		return err
	}
	snapshot := *res.Snapshot
	for _, line := range catalog.SyncSummaryLines(snapshot, false) {
		fmt.Println(line)
	}
	fmt.Printf("%s catalog sync: %d listed, %d usable by %s\n", o.name, len(snapshot.Models), len(res.Models), o.name)
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

// writeOpencodeMenu stores next; launches rebuild their config from the menu, so nothing else is refreshed.
func writeOpencodeMenu(w io.Writer, next []providers.OpencodeModel, msg string) error {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return err
	}
	fmt.Fprintf(w, "opencode catalog: %s; relaunch `claude-code opencode` to apply\n", msg)
	return nil
}

func listOpencodeMenu(w io.Writer, live bool, provider, format string) error {
	var res configure.ModelResult
	if live {
		var err error
		if res, err = discover("opencode", provider, configure.RecallModels); err != nil {
			return err
		}
	}
	rows := catalog.MenuRows(res.Provider, res.Raw)
	if format == helptext.JSON {
		return helptext.WriteJSON(w, rows)
	}
	labW := catalog.ColWidth("LABEL", len(rows), func(i int) string { return rows[i].Label })
	idW := catalog.ColWidth("ID", len(rows), func(i int) string { return rows[i].ID })
	head := fmt.Sprintf("%-*s  %-*s  %-9s  %s", idW, "ID", labW, "LABEL", "RESPONSES", "SOURCE")
	if live {
		head += "  LIVE                  CHECK"
	}
	fmt.Fprintln(w, head)
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-9v  %s", idW, r.ID, labW, r.Label, r.Responses, r.Source)
		if live {
			line += fmt.Sprintf("  %-20s  %s", strings.TrimSpace(r.Live+" "+r.Ready), r.Check)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

func addOpencodeMenu(w io.Writer, arg string, responses, responsesSet, force bool, provider string) error {
	id, next, err := catalog.MenuAddServed(arg, responses, responsesSet, force,
		func() (*providers.Provider, []providers.ModelInfo, error) {
			res, err := discover("opencode", provider, configure.RecallModels)
			return res.Provider, res.Raw, err
		})
	if err != nil {
		return err
	}
	return writeOpencodeMenu(w, next, "added "+id)
}

func importOpencodeMenu(w io.Writer, file string, apply bool) error {
	next, changes, err := catalog.MenuImportDiff(file)
	if err != nil {
		return err
	}
	for _, c := range changes {
		line := fmt.Sprintf("%s %s  %s", c.Op, c.Model.ID, c.Model.Label)
		if c.Op == "~" {
			line += fmt.Sprintf("  (was %q responses=%v; now responses=%v)", c.Was.Label, c.Was.Responses, c.Model.Responses)
		}
		fmt.Fprintln(w, line)
	}
	if len(changes) == 0 {
		fmt.Fprintln(w, "opencode catalog: no change")
		return nil
	}
	if !apply {
		fmt.Fprintf(w, "opencode catalog: dry run, %d change(s); pass --apply to write\n", len(changes))
		return nil
	}
	return writeOpencodeMenu(w, next, fmt.Sprintf("imported %d model(s)", len(next)))
}

func checkOpencodeMenu(w, errw io.Writer, ids []string, all bool, provider string, timeout time.Duration, format string) error {
	res, err := discover("opencode", provider, configure.RecallModels)
	if err != nil {
		return err
	}
	results, cacheErr, err := catalog.MenuCheck(res.Provider, res.Raw, ids, all, timeout)
	if err != nil {
		return err
	}
	if cacheErr != nil {
		fmt.Fprintf(errw, "opencode catalog check: cache not written: %v\n", cacheErr)
	}
	return catalog.WriteCheck(w, results, format)
}

// printSyncPlan writes the sync summary, the planned changes and the verify hint.
func printSyncPlan(w io.Writer, pl catalog.SyncPlan, profile string, verify bool) {
	for _, line := range catalog.SyncSummaryLines(pl.Snapshot, verify) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "opencode catalog sync: %d listed, %d eligible, %d additions, %d failed additions skipped, %d retained, %d total\n",
		len(pl.Snapshot.Models), len(pl.IDs), len(pl.Changes), pl.Skipped, len(pl.Menu), len(pl.Next))
	for _, c := range pl.Changes {
		fmt.Fprintf(w, "%s %s  %s  responses=%v\n", c.Op, c.Model.ID, c.Model.Label, c.Model.Responses)
	}
	fmt.Fprintln(w, "Shared menu: OpenCode uses all entries; Codex needs Responses + bundled metadata.")
	fmt.Fprintln(w, "Discovery is not inference proof; use --verify or the existing check command:")
	fmt.Fprintf(w, "  claude-code opencode catalog check --all --profile %s\n", profile)
}

func syncOpencodeMenu(w io.Writer, profile string, refresh bool, opts catalog.SyncOptions) error {
	res, err := discover("opencode", profile, syncMode(refresh))
	if err != nil {
		return err
	}
	pl, err := catalog.MenuSync(res.Provider, *res.Snapshot, opts)
	if err != nil {
		return err
	}
	printSyncPlan(w, pl, profile, opts.Verify)
	if !opts.Apply {
		fmt.Fprintln(w, "opencode catalog: dry run; no discovery/menu writes; pass --apply to write")
		return nil
	}
	sealed, err := pl.Seal()
	if err != nil {
		return err
	}
	if sealed {
		fmt.Fprintln(w, "opencode catalog: discovery snapshot sealed in vault")
	}
	if len(pl.Changes) == 0 {
		fmt.Fprintln(w, "opencode catalog: no menu change")
		return nil
	}
	if err := writeOpencodeMenu(w, pl.Next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(pl.Changes))); err != nil {
		return fmt.Errorf("cannot persist the shared model menu (check vault access): %w", err)
	}
	return nil
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

// opencodeOpts are the flag values of one `opencode catalog` verb.
type opencodeOpts struct {
	verb                                                   string
	live, force, apply, responses, dryRun, refresh, verify bool
	all                                                    bool
	provider                                               string
	timeout                                                time.Duration
	format                                                 *string
	fs                                                     *flag.FlagSet
}

func (m *opencodeOpts) fail(f string, a ...any) int {
	return helptext.Fail("opencode catalog "+m.verb+": "+f, a...)
}

func (m *opencodeOpts) done(err error) int {
	if err != nil {
		return m.fail("%v", err)
	}
	return 0
}

// bind registers the flags the verb takes: the shared ones first, then its own.
func (m *opencodeOpts) bind() {
	fs, verb := m.fs, m.verb
	if verb == "list" || verb == "check" {
		m.format = helptext.FormatFlag(fs)
	}
	if verb == "list" || verb == "check" || verb == "add" {
		fs.StringVar(&m.provider, "profile", "", "workspace profile (default: opencode's record profile)")
	}
	m.bindVerb()
}

func (m *opencodeOpts) bindVerb() {
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

// runOpencodeMenu is `claude-code opencode catalog list|add|remove|import|reset|check|sync`.
func runOpencodeMenu(args []string) int {
	verb, rest := args[0], args[1:]
	m := &opencodeOpts{verb: verb, timeout: 30 * time.Second, format: new(string), fs: opencodeMenuFlags(verb)}
	m.bind()
	pos, err := helptext.ParseArgs(m.fs, rest)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if verb == "list" || verb == "check" {
		if err := helptext.CheckFormat(*m.format); err != nil {
			return helptext.BadFormat("opencode catalog "+verb, err)
		}
	}
	return m.run(pos)
}

func (m *opencodeOpts) run(pos []string) int {
	switch m.verb {
	case "sync":
		return m.sync(pos)
	case "list":
		if len(pos) > 0 {
			return helptext.FailUsage("opencode catalog list: unexpected argument %q", pos[0])
		}
		return m.done(listOpencodeMenu(os.Stdout, m.live, m.provider, *m.format))
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
		return m.done(importOpencodeMenu(os.Stdout, pos[0], m.apply))
	}
	if len(pos) != 0 {
		return m.fail("takes no arguments")
	}
	return m.done(writeOpencodeMenu(os.Stdout, nil, "reset to the built-in menu"))
}

func (m *opencodeOpts) sync(pos []string) int {
	if err := opencodeSyncArgs(pos, m.apply, m.dryRun, m.provider); err != nil {
		return m.done(err)
	}
	return m.done(syncOpencodeMenu(os.Stdout, m.provider, m.refresh, catalog.SyncOptions{Apply: m.apply, Verify: m.verify}))
}

func (m *opencodeOpts) check(pos []string) int {
	if m.all && len(pos) > 0 {
		return m.fail("--all and explicit ids are exclusive")
	}
	return m.done(checkOpencodeMenu(os.Stdout, os.Stderr, pos, m.all, m.provider, m.timeout, *m.format))
}

func (m *opencodeOpts) add(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog add <id>[=Label]")
	}
	set := false
	m.fs.Visit(func(f *flag.Flag) { set = set || f.Name == "responses" })
	return m.done(addOpencodeMenu(os.Stdout, pos[0], m.responses, set, m.force, m.provider))
}

func (m *opencodeOpts) remove(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog remove <id>")
	}
	next, ok := catalog.MenuRemove(providers.OpencodeMenu(), pos[0])
	if !ok {
		return m.fail("%s is not in the menu", catalog.ID(pos[0]))
	}
	return m.done(writeOpencodeMenu(os.Stdout, next, "removed "+catalog.ID(pos[0])))
}
