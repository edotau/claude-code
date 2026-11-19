package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// resolveRow is one profile's outcome for --all-profiles: the route when it resolved, else why not.
type resolveRow struct {
	Profile string            `json:"profile"`
	Pass    bool              `json:"pass"`
	Reason  string            `json:"reason,omitempty"`
	Route   *agent.ModelRoute `json:"route,omitempty"`
}

// gatewayResolvePage is `gateway resolve`: read-only, no side effects.
var gatewayResolvePage = helptext.Page{
	Usage: "claude-code gateway resolve <agent> [flags]",
	About: "Print the overlay and wire id <agent> runs a model string as (read-only). " +
		"--all-profiles resolves on every profile; --live also checks credential, listing and READY. " +
		"all --model auto --all-profiles prints every rotation row x agent x tier pool.",
	Hint: "Exit 0, 1 no profile passed, 2 usage or a model the agent cannot run.",
}

// cmdGatewayResolve is `gateway resolve <agent> ...`: read-only, prints the overlay a typed model string becomes.
func cmdGatewayResolve(args []string) int {
	fs := helptext.NewFlagSet("gateway resolve", gatewayResolvePage)
	model := fs.String("model", "", "typed model string, as --model takes it")
	profile := fs.String("profile", "", "profile (a registry row, or auto for the Databricks pool)")
	overlay := fs.String("overlay", "", "overlay document: file, inline JSON or -")
	fallback := fs.String("fallback", "", "fallback mode ("+strings.Join(router.FallbackModes, "|")+"); none refuses a model the listing lacks")
	all := fs.Bool("all-profiles", false, "resolve on every profile")
	live := fs.Bool("live", false, "with --all-profiles: check credential, listing, READY (network)")
	format := fs.String("format", "text", "text or json")
	taskFile := fs.String("task", "", "the brief the run gets (- reads stdin): auto and an untyped model size it")
	tools := fs.Bool("tools", false, "the run calls tools: auto skips a model whose probe refused them")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if !resolveArgsOK(words, *format, *live, *all) {
		fs.Usage()
		return 2
	}
	if err := checkFallback(*fallback); err != nil {
		return helptext.FailUsage("gateway resolve: %v", err)
	}
	ctx := context.Background()
	task, err := agent.ReadBrief(*taskFile)
	if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	if words[0] == poolAll {
		return cmdPoolTable(ctx, poolUsable(*all, *live, *model, *profile, *overlay), task, *tools, *fallback, *format)
	}
	q, err := resolveQuery(ctx, words[0], *model, *profile, *overlay)
	if err != nil {
		return helptext.FailUsage("gateway resolve: %v", err)
	}
	q.Task, q.Tools, q.Fallback = task, *tools, cmp.Or(*fallback, q.Fallback)
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	if *all {
		return printRows(resolveAll(ctx, reg, q, *live), *format)
	}
	return resolveOne(ctx, reg, q, task, *format)
}

// resolveArgsOK is the verb's shape: one agent word, a known format, --live only with --all-profiles.
func resolveArgsOK(words []string, format string, live, all bool) bool {
	return len(words) == 1 && helptext.CheckFormat(format) == nil && (!live || all)
}

// resolveOne prints the first hop's route; an unrunnable model is a usage error.
func resolveOne(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, task, format string) int {
	r, choice, err := resolveFirstHop(ctx, reg, q, task)
	if errors.Is(err, agent.ErrModelRoute) {
		return helptext.FailUsage("gateway resolve: %v", err)
	} else if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	return printRoute(withGoogleQuota(ctx, r, os.Stderr), choice, format)
}

// checkFallback rejects a --fallback mode the router does not know; empty means unset.
func checkFallback(mode string) error {
	if mode == "" || slices.Contains(router.FallbackModes, mode) {
		return nil
	}
	return fmt.Errorf("--fallback %q unknown (%s)", mode, strings.Join(router.FallbackModes, "|"))
}

// googleQuotaCheck is the agy /usage seam; tests assert a non-Google route never calls it.
var googleQuotaCheck = gemini.Check

// withGoogleQuota notes a Google route's pool percent and hold, warning on warnTo; a failed read never fails resolve.
func withGoogleQuota(ctx context.Context, r agent.ModelRoute, warnTo io.Writer) agent.ModelRoute {
	if !router.PerModelQuota(r.Account) {
		return r
	}
	q, err := googleQuotaCheck(ctx, r.Model, time.Now())
	note := q.Note()
	switch {
	case err != nil:
		note = "usage unavailable"
	case q.HoldErr != nil:
		note += "; hold not saved"
	}
	r.Note = strings.Join(slices.DeleteFunc([]string{r.Note, note}, func(s string) bool { return s == "" }), "; ")
	if w := q.Warning(); w != "" {
		fmt.Fprintln(warnTo, w)
	}
	return r
}

// googleLabelWarning prints a gemini dispatch's low-quota warning to w; a route that cannot resolve prints nothing.
func googleLabelWarning(ctx context.Context, w io.Writer, name, model, profile, task string) {
	q, err := resolveQuery(ctx, name, model, profile, "")
	if err != nil {
		return
	}
	reg, err := providers.Load()
	if err != nil {
		return
	}
	if r, _, err := resolveFirstHop(ctx, reg, q, task); err == nil {
		withGoogleQuota(ctx, r, w)
	}
}

// resolveRunQuery is a seam: tests stub it so resolve never reads the live route table.
var resolveRunQuery = agent.RunQuery

// resolveFirstHop resolves q as the run's first hop does: an untyped external agent takes the ladder, like the label.
func resolveFirstHop(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, task string,
) (agent.ModelRoute, *agent.AutoChoice, error) {
	ctx = agent.PeekContext(ctx)
	hop, note, err := resolveRunQuery(ctx, reg, q, router.RuleClassifier{}.Alias(task))
	if err != nil {
		return agent.ModelRoute{}, nil, err
	}
	r, choice, err := agent.ExplainRun(reg, agent.QueryFor(ctx, hop), time.Now())
	r.Note = strings.Join(slices.DeleteFunc([]string{note, r.Note}, func(s string) bool { return s == "" }), "; ")
	return r, choice, err
}

// resolveQuery folds the overlay and flags (flags win) into the agent's query.
func resolveQuery(ctx context.Context, name, model, profile, overlay string) (agent.ModelQuery, error) {
	o, err := agentconfig.LoadOverlayContext(ctx, overlay, os.Stdin)
	if err != nil {
		return agent.ModelQuery{}, err
	}
	q := agent.ModelQuery{Agent: name, Model: model, Profile: profile, Provider: o.Provider, Account: o.Account,
		Effort: o.Effort, Identity: o.Identity, Fallback: o.Fallback}
	if q.Model == "" {
		q.Model = o.Model
	}
	if q.Profile == "" {
		q.Profile = o.Profile
	}
	if q.Profile != "" {
		q.Provider = ""
	}
	return agent.QueryFor(ctx, q), nil
}

// autoTop is how many ranked candidates --format json shows for an auto pick.
const autoTop = 5

// printRoute prints the route line, then auto's reason line; json carries auto's top ranked candidates.
func printRoute(r agent.ModelRoute, choice *agent.AutoChoice, format string) int {
	if format == helptext.JSON {
		doc := map[string]any{"overlay": r.Overlay(), "route": r}
		if choice != nil {
			c := *choice
			c.Ranked = c.Ranked[:min(len(c.Ranked), autoTop)]
			auto := map[string]any{}
			b, _ := json.Marshal(c)
			_ = json.Unmarshal(b, &auto)
			if pool := poolMembers(r, choice); pool != nil {
				auto["pool"] = pool
			}
			doc["auto"] = auto
		}
		b, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if choice == nil {
		fmt.Println(r.Line())
		return 0
	}
	reason := choice.Reason()
	before, after, _ := strings.Cut(r.Note, reason)
	r.Note = strings.Join(slices.DeleteFunc([]string{strings.TrimSuffix(before, "; "), strings.TrimPrefix(after, "; ")},
		func(s string) bool { return s == "" }), "; ")
	fmt.Println(r.Line())
	fmt.Println(reason)
	for _, m := range poolMembers(r, choice) {
		fmt.Println(poolLine(m))
	}
	return 0
}

// poolMembers is the rotation pool a rotation pick drew from; nil for any other pick.
func poolMembers(r agent.ModelRoute, choice *agent.AutoChoice) []agent.PoolMember {
	if choice.Criterion != "rotation" {
		return nil
	}
	return agent.RotationMembers(r.Agent, r.Profile, choice.Tier, time.Now())
}

// poolLine is one pool member: a marker, the model, then its hold or last pick; at most 80 columns.
func poolLine(m agent.PoolMember) string {
	mark, note := " ", "never picked"
	switch {
	case m.Held:
		mark, note = "x", "held until "+m.HeldUntil.Local().Format("Jan 2 15:04")
	case m.Next:
		mark = ">"
	}
	if !m.Held && m.LastPicked != nil {
		note = "picked " + m.LastPicked.Local().Format("Jan 2 15:04")
	}
	line := fmt.Sprintf("  %s %s  %s", mark, m.Model, note)
	if len(line) > 80 {
		line = line[:79] + "…"
	}
	return line
}

// resolveAll resolves q on every profile, optionally live-checking each that resolved.
func resolveAll(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, live bool) []resolveRow {
	q = agent.QueryFor(agent.PeekContext(ctx), q)
	spec, err := providers.SplitModelSpec(reg, q.Model)
	if err == nil {
		q.Model = spec.Model
	}
	var rows []resolveRow
	for _, p := range allRows(reg) {
		q.Profile, q.Provider = p.Name, ""
		row := resolveRow{Profile: p.Name}
		r, err := agent.ResolveRun(reg, q, time.Now())
		if err != nil {
			row.Reason = strings.TrimPrefix(err.Error(), agent.ErrModelRoute.Error()+": ")
			rows = append(rows, row)
			continue
		}
		row.Route, row.Pass = &r, true
		if live {
			if why := liveCheck(ctx, p, r); why != "" {
				row.Pass, row.Reason = false, why
			}
		} else if r.Served == agent.ServedNo {
			row.Pass, row.Reason = false, r.Model+" is not listed"
		} // an unverified route says so on its own line: Line prints served: unverified
		rows = append(rows, row)
	}
	return rows
}

// allRows lists every registry row grouped by provider type, each in declared order.
func allRows(reg *providers.Registry) []*providers.Provider {
	var types []string
	for _, n := range reg.Names() {
		if t := reg.Providers[n].Provider; !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	var out []*providers.Provider
	for _, t := range types {
		for _, n := range reg.ProfilesOf(t) {
			out = append(out, reg.Providers[n])
		}
	}
	return out
}

// liveCheck is "" when p's credential mints headless, its listing is fresh and holds the model READY.
func liveCheck(ctx context.Context, p *providers.Provider, r agent.ModelRoute) string {
	ctx, cancel := context.WithTimeout(providers.WithIdentity(ctx, providers.IdentityHeadless), 45*time.Second)
	defer cancel()
	if _, err := providers.Credential(ctx, p); err != nil && !errors.Is(err, providers.ErrPassthrough) {
		return "credential: " + firstLine(err.Error())
	}
	if p.Preset != providers.PresetDatabricks {
		return ""
	}
	now := time.Now()
	if msg := refreshServed(ctx, p, now); msg != "" {
		return msg
	}
	switch listed, ready, fresh := router.ServedListed(p, r.Model, now); {
	case !fresh:
		return "listing still stale after refresh"
	case !listed:
		return r.Model + " is not listed"
	case !ready:
		return r.Model + " endpoint is not READY"
	}
	return ""
}

// refreshServed is "" when p's served listing is fresh, or was refreshed and saved.
func refreshServed(ctx context.Context, p *providers.Provider, now time.Time) string {
	if s, ok := router.LoadServed(p.Name); ok && s.Fresh(now) {
		return ""
	}
	infos, err := providers.ListModels(ctx, p)
	if err != nil {
		return "listing refresh: " + firstLine(err.Error())
	}
	if err := router.WarmServed(p.Name, infos, now); err != nil {
		return "listing save: " + err.Error()
	}
	return ""
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// printRows prints one PASS/FAIL row per profile (or a JSON array).
func printRows(rows []resolveRow, format string) int {
	if format == helptext.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, r := range rows {
			if r.Route != nil {
				fmt.Printf("%-4s %-12s %s\n", map[bool]string{true: "PASS", false: "FAIL"}[r.Pass], r.Profile, r.Route.Line())
			} else {
				fmt.Printf("FAIL %-12s %s\n", r.Profile, r.Reason)
			}
			if r.Route != nil && r.Reason != "" {
				fmt.Printf("     %-12s %s\n", "", r.Reason)
			}
		}
	}
	for _, r := range rows {
		if r.Pass {
			return 0
		}
	}
	return 1
}

// poolAll is the agent word for `gateway resolve all`: every rotation row x agent x tier, read-only.
const poolAll = "all"

// poolAgents are the five agents the pool table walks.
var poolAgents = append([]string{agentid.Claude}, agent.DefaultAgents...)

// poolTiers are the auto tiers in print order, keyed by the alias TaskTier returns.
var poolTiers = []struct{ name, alias string }{{"fast", models.Haiku}, {"standard", models.Sonnet}, {"deep", models.Opus}}

// poolRow is one table row; json reuses the auto block's pool member fields.
type poolRow struct {
	Row        string             `json:"row"`
	Agent      string             `json:"agent"`
	Tier       string             `json:"tier"`
	Size       int                `json:"size"`
	Next       string             `json:"next"`
	Held       int                `json:"held"`
	LastPicked *time.Time         `json:"last_picked,omitempty"`
	Note       string             `json:"note,omitempty"`
	Pool       []agent.PoolMember `json:"pool"`
}

func poolUsable(all, live bool, model, profile, overlay string) bool {
	return all && !live && model == agent.AutoModel && profile == "" && overlay == ""
}

// cmdPoolTable prints the pool table; it only peeks, so no rotation history moves.
func cmdPoolTable(ctx context.Context, usable bool, task string, tools bool, fallback, format string) int {
	if !usable {
		return helptext.FailUsage("gateway resolve: all takes --model auto --all-profiles; no --live, --profile, --overlay")
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	alias, _ := agent.TaskTier(task) // "" for no brief: every tier
	var tiers []string
	for _, t := range poolTiers {
		if alias == "" || t.alias == alias {
			tiers = append(tiers, t.name)
		}
	}
	rows := poolRows(ctx, reg, tiers, tools, fallback, time.Now())
	if format == helptext.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	renderPoolTable(os.Stdout, rows, helptext.TermWidth(os.Stdout), time.Now())
	return 0
}

// poolRows resolves every rotation row x agent x tier through the single-agent path, sorted agent, row, tier.
func poolRows(ctx context.Context, reg *providers.Registry, tiers []string, tools bool, fallback string, now time.Time) []poolRow {
	var out []poolRow
	for _, p := range allRows(reg) {
		if !router.RotationRow(p) {
			continue
		}
		for _, name := range poolAgents {
			if !agent.VaultAgentIsClaude(name) && !agent.CanServe(name, p) {
				continue
			}
			for _, tier := range tiers {
				if row, ok := poolCell(ctx, reg, name, p, tier, tools, fallback, now); ok {
					out = append(out, row)
				}
			}
		}
	}
	slices.SortStableFunc(out, func(a, b poolRow) int {
		return cmp.Or(strings.Compare(a.Agent, b.Agent), strings.Compare(a.Row, b.Row), tierRank(a.Tier)-tierRank(b.Tier))
	})
	return out
}

func tierRank(tier string) int {
	return slices.IndexFunc(poolTiers, func(t struct{ name, alias string }) bool { return t.name == tier })
}

// poolCell is name's auto pick on p at tier; false when p cannot run name at all (claude on a login row).
func poolCell(ctx context.Context, reg *providers.Registry, name string, p *providers.Provider, tier string,
	tools bool, fallback string, now time.Time) (poolRow, bool) {
	row := poolRow{Row: router.AccountOf(p), Agent: name, Tier: tier}
	q, err := resolveQuery(ctx, name, agent.AutoModel, p.Name, "")
	if err != nil {
		row.Note = "cannot run"
		return row, true
	}
	q.Task, q.Tools, q.Fallback = "Tier: "+tier, tools, cmp.Or(fallback, q.Fallback)
	r, choice, err := resolveFirstHop(ctx, reg, q, q.Task)
	switch {
	case errors.Is(err, agent.ErrPoolExhausted):
		row.Note, row.Pool = "exhausted", agent.RotationMembers(name, p.Name, tier, now)
	case err != nil && agent.VaultAgentIsClaude(name) && errors.Is(err, agent.ErrModelRoute):
		return row, false
	case err != nil:
		row.Note = "cannot run"
	case choice != nil:
		row.Next, row.Pool = r.Model, poolMembers(r, choice)
		if choice.Criterion == "rotation" && len(row.Pool) == 0 { // tierRotation stepped: the row serves nothing here
			row.Note, row.Pool = "stepped to standard", agent.RotationMembers(name, p.Name, "standard", now)
		}
	default:
		row.Next = r.Model
	}
	return tallyPool(row, agent.VaultAgentIsClaude(name)), true
}

// tallyPool fills the held count, newest pick and size from the pool, and why an empty pool is empty.
func tallyPool(row poolRow, claude bool) poolRow {
	for _, m := range row.Pool {
		if m.Held {
			row.Held++
		}
		if m.LastPicked != nil && (row.LastPicked == nil || m.LastPicked.After(*row.LastPicked)) {
			row.LastPicked = m.LastPicked
		}
	}
	row.Size = len(row.Pool)
	if row.Size == 0 && row.Note == "" {
		row.Note = map[bool]string{true: "does not rotate", false: "no fresh listing"}[claude]
	}
	return row
}

// renderPoolTable prints the table; width > 0 (a terminal) clips NOTE first, then NEXT, so no line passes width.
func renderPoolTable(w io.Writer, rows []poolRow, width int, now time.Time) {
	head := [8]string{"ROW", "AGENT", "TIER", "POOL", "NEXT", "HELD", "LAST", "NOTE"}
	cells := [][8]string{head}
	for _, r := range rows {
		last := "-"
		if r.LastPicked != nil {
			last = agent.ShortDur(now.Sub(*r.LastPicked))
		}
		cells = append(cells, [8]string{r.Row, r.Agent, r.Tier, strconv.Itoa(r.Size), r.Next, strconv.Itoa(r.Held), last, r.Note})
	}
	var widths [8]int
	for _, c := range cells {
		for i, s := range c {
			widths[i] = max(widths[i], textutil.DisplayWidth(s))
		}
	}
	if width > 0 {
		fixed := 7 + widths[0] + widths[1] + widths[2] + widths[3] + widths[5] + widths[6] // 7 column gaps
		widths[7] = max(min(widths[7], width-fixed-widths[4]), 0)
		widths[4] = max(min(widths[4], width-fixed-widths[7]), 0)
	}
	for _, c := range cells {
		var line [8]string
		for i, s := range c {
			line[i] = textutil.PadCells(textutil.TruncateCells(s, widths[i], "…"), widths[i], i == 3 || i == 5)
		}
		fmt.Fprintln(w, strings.TrimRight(strings.Join(line[:], " "), " "))
	}
}
