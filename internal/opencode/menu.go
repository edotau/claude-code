package opencode

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// MenuRow is one `opencode catalog list` line; Live, Ready and Check are set only for a live listing.
type MenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Check     string `json:"check,omitempty"`
}

// MenuRows lists the shared menu; live marks each entry served or missing on provider's workspace.
func MenuRows(live bool, provider string) ([]MenuRow, error) {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	var cache map[string]serving.CheckResult
	if live {
		var err error
		if served, p, err = OpencodeServed(provider); err != nil {
			return nil, err
		}
		cache, _ = catalog.LoadCheckCache(p.Name)
	}
	var rows []MenuRow
	for _, m := range providers.OpencodeMenu() {
		r := MenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := catalog.ServedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
			r.Check = catalog.CheckCell(cache, m.ID)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// MenuAddServed is the menu with arg (`id[=Label]`) added; unless force, the workspace must serve the id.
func MenuAddServed(arg string, responses, responsesSet, force bool, provider string) (id string, next []providers.OpencodeModel, err error) {
	id, label, _ := strings.Cut(arg, "=")
	id = catalog.ID(id)
	if id == "" {
		return "", nil, errors.New("empty model id")
	}
	if !force {
		served, p, err := OpencodeServed(provider)
		if err != nil {
			return "", nil, fmt.Errorf("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := catalog.ServedInfo(served, p, id); !ok {
			return "", nil, fmt.Errorf("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return id, MenuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r), nil
}

// MenuImportDiff parses file and returns the menu it describes plus what changes against the current one.
func MenuImportDiff(file string) (next []providers.OpencodeModel, changes []MenuChange, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	next, err = ParseMenuImport(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", file, err)
	}
	return next, DiffMenu(providers.OpencodeMenu(), next), nil
}

// MenuCheck probes the chosen endpoints and caches the results; cacheErr is a cache write failure, not a probe one.
func MenuCheck(ids []string, all bool, provider string, timeout time.Duration) (results []serving.CheckResult, cacheErr, err error) {
	served, p, err := OpencodeServed(provider)
	if err != nil {
		return nil, nil, err
	}
	var menu []string
	for _, m := range providers.OpencodeMenu() {
		menu = append(menu, m.ID)
	}
	targets := catalog.Targets(ids, all, served, p, menu)
	return catalog.Check(p, targets, serving.CheckOptions{Concurrency: 4, Timeout: timeout})
}

// SyncOptions are `opencode catalog sync`'s switches.
type SyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// SyncPlan is a discovery run merged into the shared menu; nothing is written until Seal and the caller's menu write.
type SyncPlan struct {
	Provider *providers.Provider
	Snapshot serving.Snapshot
	IDs      []string // eligible serving ids
	Menu     []providers.OpencodeModel
	Next     []providers.OpencodeModel
	Skipped  int // additions dropped because their probe failed
	Changes  []MenuChange
	Verified bool
}

// MenuSync discovers profile's serving models and merges absent ids in; existing order, labels and Responses stay.
func MenuSync(profile string, opts SyncOptions) (SyncPlan, error) {
	p, snapshot, err := OpencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return SyncPlan{}, err
	}
	ids := OpencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := OpencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return SyncPlan{}, err
		}
	}
	next, skipped := OpencodeSyncMerge(menu, ids, snapshot.Checks)
	return SyncPlan{Provider: p, Snapshot: snapshot, IDs: ids, Menu: menu, Next: next, Skipped: skipped,
		Changes: DiffMenu(menu, next), Verified: opts.Verify}, nil
}

// Seal stores the discovery snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func (pl SyncPlan) Seal() (sealed bool, err error) {
	return catalog.Seal(pl.Provider, pl.Snapshot, pl.Verified)
}

// WriteMenu stores next; launches rebuild their config from the menu, so nothing else is refreshed.
func WriteMenu(w io.Writer, next []providers.OpencodeModel, msg string) error {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return err
	}
	fmt.Fprintf(w, "opencode catalog: %s; relaunch `claude-code opencode` to apply\n", msg)
	return nil
}

func ListMenu(w io.Writer, live bool, provider, format string) error {
	rows, err := MenuRows(live, provider)
	if err != nil {
		return err
	}
	if format == helptext.JSON {
		return helptext.WriteJSON(w, rows)
	}
	labW := len("LABEL")
	for _, r := range rows {
		labW = max(labW, len(r.Label))
	}
	idW := catalog.IDWidth(len(rows), func(i int) string { return rows[i].ID })
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

func AddMenu(w io.Writer, arg string, responses, responsesSet, force bool, provider string) error {
	id, next, err := MenuAddServed(arg, responses, responsesSet, force, provider)
	if err != nil {
		return err
	}
	return WriteMenu(w, next, "added "+id)
}

func ImportMenu(w io.Writer, file string, apply bool) error {
	next, changes, err := MenuImportDiff(file)
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
	return WriteMenu(w, next, fmt.Sprintf("imported %d model(s)", len(next)))
}

func CheckMenu(w, errw io.Writer, ids []string, all bool, provider string, timeout time.Duration, format string) error {
	results, cacheErr, err := MenuCheck(ids, all, provider, timeout)
	if err != nil {
		return err
	}
	if cacheErr != nil {
		fmt.Fprintf(errw, "opencode catalog check: cache not written: %v\n", cacheErr)
	}
	return catalog.WriteCheck(w, results, format)
}

func SyncMenu(w io.Writer, profile string, opts SyncOptions) error {
	pl, err := MenuSync(profile, opts)
	if err != nil {
		return err
	}
	for _, line := range catalog.SyncSummaryLines(pl.Snapshot, opts.Verify) {
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
	if err := WriteMenu(w, pl.Next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(pl.Changes))); err != nil {
		return fmt.Errorf("cannot persist the shared model menu (check vault access): %w", err)
	}
	return nil
}

// menuVerbs are the `opencode catalog` subverbs RunMenu handles.
var menuVerbs = []string{"list", "add", "remove", "import", "reset", "check", "sync"}

func menuPage(verb string) helptext.Page {
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

func menuFlags(verb string) *flag.FlagSet {
	return helptext.NewFlagSet("opencode catalog "+verb, menuPage(verb))
}

// syncArgs validates `opencode catalog sync`'s arguments.
func syncArgs(pos []string, apply, dryRun bool, provider string) error {
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

func (m *menuOpts) fail(f string, a ...any) int {
	return helptext.Fail("opencode catalog "+m.verb+": "+f, a...)
}

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

// RunMenu is `claude-code opencode catalog list|add|remove|import|reset|check|sync`.
func RunMenu(args []string) int {
	verb, rest := args[0], args[1:]
	m := &menuOpts{verb: verb, timeout: 30 * time.Second, format: new(string), fs: menuFlags(verb)}
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

func (m *menuOpts) run(pos []string) int {
	switch m.verb {
	case "sync":
		return m.sync(pos)
	case "list":
		if len(pos) > 0 {
			return helptext.FailUsage("opencode catalog list: unexpected argument %q", pos[0])
		}
		return m.done(ListMenu(os.Stdout, m.live, m.provider, *m.format))
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
		return m.done(ImportMenu(os.Stdout, pos[0], m.apply))
	}
	if len(pos) != 0 {
		return m.fail("takes no arguments")
	}
	return m.done(WriteMenu(os.Stdout, nil, "reset to the built-in menu"))
}

func (m *menuOpts) sync(pos []string) int {
	if err := syncArgs(pos, m.apply, m.dryRun, m.provider); err != nil {
		return m.done(err)
	}
	return m.done(SyncMenu(os.Stdout, m.provider, SyncOptions{Apply: m.apply, Refresh: m.refresh, Verify: m.verify}))
}

func (m *menuOpts) check(pos []string) int {
	if m.all && len(pos) > 0 {
		return m.fail("--all and explicit ids are exclusive")
	}
	return m.done(CheckMenu(os.Stdout, os.Stderr, pos, m.all, m.provider, m.timeout, *m.format))
}

func (m *menuOpts) add(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog add <id>[=Label]")
	}
	set := false
	m.fs.Visit(func(f *flag.Flag) { set = set || f.Name == "responses" })
	return m.done(AddMenu(os.Stdout, pos[0], m.responses, set, m.force, m.provider))
}

func (m *menuOpts) remove(pos []string) int {
	if len(pos) != 1 {
		return m.fail("usage: claude-code opencode catalog remove <id>")
	}
	next, ok := MenuRemove(providers.OpencodeMenu(), pos[0])
	if !ok {
		return m.fail("%s is not in the menu", catalog.ID(pos[0]))
	}
	return m.done(WriteMenu(os.Stdout, next, "removed "+catalog.ID(pos[0])))
}
