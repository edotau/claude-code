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
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/gemini"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
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
		"--all-profiles resolves on every profile; --live also checks credential, listing and READY.",
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
	if len(words) != 1 || helptext.CheckFormat(*format) != nil || *live && !*all {
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

// resolveOne prints the first hop's route; an unrunnable model is a usage error.
func resolveOne(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, task, format string) int {
	r, choice, err := resolveFirstHop(ctx, reg, q, task)
	if errors.Is(err, agent.ErrModelRoute) {
		return helptext.FailUsage("gateway resolve: %v", err)
	} else if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	if p := reg.Row(r.Profile); p != nil {
		target := *p
		target.Identity = cmp.Or(r.Identity, p.Identity)
		if f := agent.CheckToken(&target, time.Now(), agent.DefaultToolsTimeout); f != nil {
			r.Note = strings.TrimPrefix(r.Note+"; "+f.String(), "; ")
		}
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
			doc["auto"] = c
		}
		b, _ := json.MarshalIndent(doc, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	if choice == nil {
		fmt.Println(routeLine(r))
		return 0
	}
	reason := choice.Reason()
	before, after, _ := strings.Cut(r.Note, reason)
	r.Note = strings.Join(slices.DeleteFunc([]string{strings.TrimSuffix(before, "; "), strings.TrimPrefix(after, "; ")},
		func(s string) bool { return s == "" }), "; ")
	fmt.Println(routeLine(r))
	fmt.Println(reason)
	return 0
}

// routeLine is the one-line summary: agent · account · model, the effort, the source and any note.
func routeLine(r agent.ModelRoute) string {
	line := fmt.Sprintf("%s · %s · %s", r.Agent, r.Account, r.Model)
	if r.Effort != "" {
		line += " effort " + r.Effort
	}
	line += " (" + r.Source + ")"
	if r.Served != "" {
		line += " served: " + r.Served
	}
	if r.Note != "" {
		line += " note: " + r.Note
	}
	return line
}

// resolveAll resolves q on every profile, optionally live-checking each that resolved.
func resolveAll(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, live bool) []resolveRow {
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
		} // an unverified route says so on its own line: routeLine prints served: unverified
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
				fmt.Printf("%-4s %-12s %s\n", map[bool]string{true: "PASS", false: "FAIL"}[r.Pass], r.Profile, routeLine(*r.Route))
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
