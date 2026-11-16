package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agentconfig"
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

// cmdAgentsResolve is `agents resolve <agent> ...`: read-only, prints the overlay a typed model string becomes.
func cmdAgentsResolve(args []string) int {
	fs := agentsFlags("resolve")
	model := fs.String("model", "", "typed model string, as --model takes it")
	profile := fs.String("profile", "", "profile (a registry row, or auto for the Databricks pool)")
	overlay := fs.String("overlay", "", "overlay document: file, inline JSON or -")
	all := fs.Bool("all-profiles", false, "resolve on every profile")
	live := fs.Bool("live", false, "with --all-profiles: check credential, listing, READY (network)")
	format := fs.String("format", "text", "text or json")
	words, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(words) != 1 || helptext.CheckFormat(*format) != nil || *live && !*all {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	q, err := resolveQuery(ctx, words[0], *model, *profile, *overlay)
	if err != nil {
		return helptext.FailUsage("agents resolve: %v", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("agents resolve: %v", err)
	}
	if *all {
		return printRows(resolveAll(ctx, reg, q, *live), *format)
	}
	r, err := agent.ResolveModel(reg, q, time.Now())
	if errors.Is(err, agent.ErrModelRoute) {
		return helptext.FailUsage("agents resolve: %v", err)
	} else if err != nil {
		return helptext.Fail("agents resolve: %v", err)
	}
	return printRoute(r, *format)
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

func printRoute(r agent.ModelRoute, format string) int {
	if format == helptext.JSON {
		b, _ := json.MarshalIndent(map[string]any{"overlay": r.Overlay(), "route": r}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(routeLine(r))
	return 0
}

// routeLine is the one-line summary: agent · account · model, the effort, the source and any note.
func routeLine(r agent.ModelRoute) string {
	line := fmt.Sprintf("%s · %s · %s", r.Agent, r.Account, r.Model)
	if r.Effort != "" {
		line += " effort " + r.Effort
	}
	line += " (" + r.Source + ")"
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
		r, err := agent.ResolveModel(reg, q, time.Now())
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
		}
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
	if s, ok := router.LoadServed(p.Name); !ok || !s.Fresh(now) {
		infos, err := providers.ListModels(ctx, p)
		if err != nil {
			return "listing refresh: " + firstLine(err.Error())
		}
		if err := router.WarmServed(p.Name, infos, now); err != nil {
			return "listing save: " + err.Error()
		}
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
