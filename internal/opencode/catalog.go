package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// opencodeProvider resolves the workspace profile: --profile, else the opencode pin, else the session profile.
func opencodeProvider(name string) (*providers.Provider, error) {
	p, err := catalog.Profile("opencode", name)
	if err == nil && p == nil {
		err = errors.New("no profile pinned for opencode; pass --profile")
	}
	return p, err
}

// opencodeServed lists the workspace's endpoints by name.
func opencodeServed(name string) (map[string]providers.ModelInfo, *providers.Provider, error) {
	p, err := opencodeProvider(name)
	if err != nil {
		return nil, nil, err
	}
	served, err := catalog.Served(p)
	return served, p, err
}

// opencodeSyncModels resolves the profile; discovery validates identity before reading the sealed cache.
func opencodeSyncModels(profile string, refresh bool) (*providers.Provider, serving.Snapshot, error) {
	p, err := opencodeProvider(profile)
	if errors.Is(err, catalog.ErrGateway) {
		return nil, serving.Snapshot{}, err
	}
	if err != nil {
		return nil, serving.Snapshot{}, errors.New("cannot resolve profile; check the provider configuration")
	}
	snapshot, err := serving.Discover(context.Background(), p, refresh)
	return p, snapshot, err
}

// opencodeSyncIDs selects unique, losslessly spellable READY chat endpoint ids in deterministic order.
func opencodeSyncIDs(infos []providers.ModelInfo) []string {
	var ids []string
	for _, info := range infos {
		if info.Gateway || info.Ready != "READY" || info.Task != "llm/v1/chat" ||
			!strings.HasPrefix(info.ID, models.DatabricksPrefix) {
			continue
		}
		id := catalog.ID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func opencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []serving.CheckResult) ([]providers.OpencodeModel, int) {
	failed := make(map[string]bool)
	for _, c := range checks {
		failed[c.ID] = c.Status == "fail" || c.Status == "missing"
	}
	next := slices.Clone(menu)
	skipped := 0
	for _, id := range ids {
		if !slices.ContainsFunc(menu, func(m providers.OpencodeModel) bool { return m.ID == id }) {
			if failed[id] {
				skipped++
				continue
			}
			// menuAdd canonicalizes its input, so pass the lossless wire name rather than canonicalizing the id twice.
			next = menuAdd(next, models.Wire("databricks-serving", id), "", nil)
		}
	}
	return next, skipped
}

func opencodeSyncVerify(p *providers.Provider, snapshot *serving.Snapshot, ids []string, menu []providers.OpencodeModel) error {
	responses := make(map[string]bool, len(menu))
	for _, m := range menu {
		responses[m.ID] = m.Responses
	}
	targets := make([]serving.CheckTarget, 0, len(ids))
	for _, id := range ids {
		r, exists := responses[id]
		if !exists {
			r = models.ResponsesOnly(id)
		}
		targets = append(targets, serving.CheckTarget{ID: id, Wire: models.Wire("databricks-serving", id), Responses: r})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checks, err := serving.Probe(ctx, p, targets,
		serving.CheckOptions{ClientRoutes: true, Concurrency: 4, Timeout: 30 * time.Second})
	if err != nil {
		return errors.New("serving verification did not complete; no discovery/menu writes")
	}
	snapshot.Checks = serving.SanitizeChecks(checks)
	snapshot.CheckedAt = time.Now().UTC()
	return nil
}

// defaultMenuLabel is the label an added id gets when none is given.
func defaultMenuLabel(id string) string {
	var words []string
	prevDigits := false
	for w := range strings.SplitSeq(id, "-") {
		digits := w != "" && strings.Trim(w, "0123456789") == ""
		switch {
		case digits && prevDigits: // version runs read as one number: 5-5 → 5.5
			words[len(words)-1] += "." + w
		case menuAcronyms[w]:
			words = append(words, strings.ToUpper(w))
		case w != "":
			words = append(words, strings.ToUpper(w[:1])+w[1:])
		}
		prevDigits = digits
	}
	return strings.Join(words, " ")
}

// menuAcronyms are id words a label spells in capitals.
var menuAcronyms = map[string]bool{"glm": true, "gpt": true, "oss": true, "bge": true, "gte": true}

// menuAdd returns menu with id added, or updated in place when present; catalog Responses-only ids default to Responses.
// responses is nil when the caller did not say. A blank label keeps the existing one, else the derived default.
func menuAdd(menu []providers.OpencodeModel, id, label string, responses *bool) []providers.OpencodeModel {
	id = catalog.ID(id)
	out := slices.Clone(menu)
	i := slices.IndexFunc(out, func(m providers.OpencodeModel) bool { return m.ID == id })
	cur := providers.OpencodeModel{ID: id, Responses: models.ResponsesOnly(id)}
	if i >= 0 {
		cur = out[i]
	}
	if label != "" {
		cur.Label = label
	} else if cur.Label == "" {
		cur.Label = defaultMenuLabel(id)
	}
	if responses != nil {
		cur.Responses = *responses
	}
	if i >= 0 {
		out[i] = cur
		return out
	}
	return append(out, cur)
}

// menuRemove returns menu without id; ok is false when it was not there.
func menuRemove(menu []providers.OpencodeModel, id string) (out []providers.OpencodeModel, ok bool) {
	id = catalog.ID(id)
	out = slices.DeleteFunc(slices.Clone(menu), func(m providers.OpencodeModel) bool { return m.ID == id })
	return out, len(out) != len(menu)
}

// parseMenuImport reads an opencode.json (its databricks provider "models" maps) or plain `id[,Label][,responses]` lines.
func parseMenuImport(data []byte) ([]providers.OpencodeModel, error) {
	if t := strings.TrimSpace(string(data)); strings.HasPrefix(t, "{") {
		return parseOpencodeJSON([]byte(t))
	}
	var out []providers.OpencodeModel
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m, err := parseMenuLine(line, n)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return dedupeMenu(out), sc.Err()
}

func parseMenuLine(line string, n int) (providers.OpencodeModel, error) {
	f := strings.Split(line, ",")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	id := catalog.ID(f[0])
	if id == "" {
		return providers.OpencodeModel{}, fmt.Errorf("line %d: empty id", n)
	}
	m := providers.OpencodeModel{ID: id, Responses: models.ResponsesOnly(id)}
	if len(f) > 1 && f[1] != "" && f[1] != "responses" {
		m.Label = f[1]
	}
	if slices.Contains(f[1:], "responses") {
		m.Responses = true
	}
	if m.Label == "" {
		m.Label = defaultMenuLabel(id)
	}
	return m, nil
}

// parseOpencodeJSON takes every databricks provider (by id or profile name); the provider id sets each model's routing.
func parseOpencodeJSON(data []byte) ([]providers.OpencodeModel, error) {
	var doc struct {
		Provider map[string]struct {
			NPM    string `json:"npm"`
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("opencode.json: %w", err)
	}
	var out []providers.OpencodeModel
	profiles := databricksProfileNames()
	for _, pid := range slices.Sorted(maps.Keys(doc.Provider)) {
		if !strings.Contains(pid, "databricks") && !namesProfile(pid, profiles) {
			continue
		}
		prov := doc.Provider[pid]
		for _, wire := range slices.Sorted(maps.Keys(prov.Models)) {
			id := catalog.ID(wire)
			label := strings.TrimSpace(strings.SplitN(prov.Models[wire].Name, " · ", 2)[0])
			if label == "" {
				label = defaultMenuLabel(id)
			}
			out = append(out, providers.OpencodeModel{ID: id, Label: label, Responses: providerResponses(pid, prov.NPM)})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("opencode.json: no databricks provider with a models map")
	}
	return dedupeMenu(out), nil
}

// providerResponses reads the route from the provider's npm package (@ai-sdk/openai is Responses), else the "-gpt" suffix.
func providerResponses(pid, npm string) bool {
	if npm != "" {
		return npm == "@ai-sdk/openai"
	}
	return strings.HasSuffix(pid, "-gpt")
}

// databricksProfileNames are the registry's databricks profiles; a registry that does not load has none.
func databricksProfileNames() []string {
	reg, _ := providers.Load()
	if reg == nil {
		return nil
	}
	return providers.DatabricksProfiles(reg)
}

// namesProfile reports whether a profile appears in pid as whole "-"-delimited words (profiles may contain hyphens).
func namesProfile(pid string, profiles []string) bool {
	seg := "-" + pid + "-"
	return slices.ContainsFunc(profiles, func(p string) bool { return strings.Contains(seg, "-"+p+"-") })
}

// dedupeMenu keeps the last entry per id, in first-seen order.
func dedupeMenu(in []providers.OpencodeModel) []providers.OpencodeModel {
	var out []providers.OpencodeModel
	for _, m := range in {
		if i := slices.IndexFunc(out, func(o providers.OpencodeModel) bool { return o.ID == m.ID }); i >= 0 {
			out[i] = m
			continue
		}
		out = append(out, m)
	}
	return out
}

// MenuChange is one line of an import diff: Op is "+", "-" or "~".
type MenuChange struct {
	Op    string                   `json:"op"`
	Model providers.OpencodeModel  `json:"model"`
	Was   *providers.OpencodeModel `json:"was,omitempty"`
}

// diffMenu lists what turns old into next: additions, removals, then label/responses changes.
func diffMenu(old, next []providers.OpencodeModel) []MenuChange {
	var out []MenuChange
	find := func(l []providers.OpencodeModel, id string) (providers.OpencodeModel, bool) {
		i := slices.IndexFunc(l, func(m providers.OpencodeModel) bool { return m.ID == id })
		if i < 0 {
			return providers.OpencodeModel{}, false
		}
		return l[i], true
	}
	for _, m := range next {
		if _, ok := find(old, m.ID); !ok {
			out = append(out, MenuChange{Op: "+", Model: m})
		}
	}
	for _, m := range old {
		if _, ok := find(next, m.ID); !ok {
			out = append(out, MenuChange{Op: "-", Model: m})
		}
	}
	for _, m := range next {
		if was, ok := find(old, m.ID); ok && was != m {
			w := was
			out = append(out, MenuChange{Op: "~", Model: m, Was: &w})
		}
	}
	return out
}

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

// menuRows lists the shared menu; live marks each entry served or missing on provider's workspace.
func menuRows(live bool, provider string) ([]MenuRow, error) {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	var cache map[string]serving.CheckResult
	if live {
		var err error
		if served, p, err = opencodeServed(provider); err != nil {
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

// menuAddServed is the menu with arg (`id[=Label]`) added; unless force, the workspace must serve the id.
func menuAddServed(arg string, responses, responsesSet, force bool, provider string) (id string, next []providers.OpencodeModel, err error) {
	id, label, _ := strings.Cut(arg, "=")
	id = catalog.ID(id)
	if id == "" {
		return "", nil, errors.New("empty model id")
	}
	if !force {
		served, p, err := opencodeServed(provider)
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
	return id, menuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r), nil
}

// menuImportDiff parses file and returns the menu it describes plus what changes against the current one.
func menuImportDiff(file string) (next []providers.OpencodeModel, changes []MenuChange, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	next, err = parseMenuImport(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", file, err)
	}
	return next, diffMenu(providers.OpencodeMenu(), next), nil
}

// menuCheck probes the chosen endpoints and caches the results; cacheErr is a cache write failure, not a probe one.
func menuCheck(ids []string, all bool, provider string, timeout time.Duration) (results []serving.CheckResult, cacheErr, err error) {
	served, p, err := opencodeServed(provider)
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
	p, snapshot, err := opencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return SyncPlan{}, err
	}
	ids := opencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := opencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return SyncPlan{}, err
		}
	}
	next, skipped := opencodeSyncMerge(menu, ids, snapshot.Checks)
	return SyncPlan{Provider: p, Snapshot: snapshot, IDs: ids, Menu: menu, Next: next, Skipped: skipped,
		Changes: diffMenu(menu, next), Verified: opts.Verify}, nil
}

// Seal stores the discovery snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func (pl SyncPlan) Seal() (sealed bool, err error) {
	return catalog.Seal(pl.Provider, pl.Snapshot, pl.Verified)
}

// writeMenu stores next; launches rebuild their config from the menu, so nothing else is refreshed.
func writeMenu(w io.Writer, next []providers.OpencodeModel, msg string) error {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return err
	}
	fmt.Fprintf(w, "opencode catalog: %s; relaunch `claude-code opencode` to apply\n", msg)
	return nil
}

func ListMenu(w io.Writer, live bool, provider, format string) error {
	rows, err := menuRows(live, provider)
	if err != nil {
		return err
	}
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

func AddMenu(w io.Writer, arg string, responses, responsesSet, force bool, provider string) error {
	id, next, err := menuAddServed(arg, responses, responsesSet, force, provider)
	if err != nil {
		return err
	}
	return writeMenu(w, next, "added "+id)
}

func ImportMenu(w io.Writer, file string, apply bool) error {
	next, changes, err := menuImportDiff(file)
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
	return writeMenu(w, next, fmt.Sprintf("imported %d model(s)", len(next)))
}

func CheckMenu(w, errw io.Writer, ids []string, all bool, provider string, timeout time.Duration, format string) error {
	results, cacheErr, err := menuCheck(ids, all, provider, timeout)
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
	if err := writeMenu(w, pl.Next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(pl.Changes))); err != nil {
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
	return m.done(writeMenu(os.Stdout, nil, "reset to the built-in menu"))
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
	next, ok := menuRemove(providers.OpencodeMenu(), pos[0])
	if !ok {
		return m.fail("%s is not in the menu", catalog.ID(pos[0]))
	}
	return m.done(writeMenu(os.Stdout, next, "removed "+catalog.ID(pos[0])))
}
