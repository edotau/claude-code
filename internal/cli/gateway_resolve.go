package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/gateway/resolve"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

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
	profile := fs.String("profile", "", "registry row; auto is legacy: use --provider databricks")
	provider := fs.String("provider", "", "provider (databricks selects the pool)")
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
	if !resolveArgsOK(words, *format, *live, *all, route.Route{Provider: *provider, Profile: *profile}) {
		fs.Usage()
		return 2
	}
	c, err := providers.CastShared(*model)
	if err == nil {
		_, *profile, err = c.Merge(*provider, *profile)
	}
	if err = cmp.Or(err, checkFallback(*fallback)); err != nil {
		return helptext.FailUsage("gateway resolve: %v", err)
	}
	if *provider == providers.GroupDatabricks && c.Profile == "" {
		*profile = agent.AutoModel
	}
	*model = c.Model
	ctx := context.Background()
	task, err := agent.ReadBrief(*taskFile)
	if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	if words[0] == poolAll {
		return cmdPoolTable(ctx, poolUsable(*all, *live, *model, *profile, *overlay), task, *tools, *fallback, *format)
	}
	q, err := resolve.Query(ctx, words[0], *model, *profile, *overlay)
	if err != nil {
		return helptext.FailUsage("gateway resolve: %v", err)
	}
	q.Task, q.Tools, q.Fallback = task, *tools, cmp.Or(*fallback, q.Fallback)
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	if *all {
		return printRows(resolve.All(ctx, reg, q, *live), *format)
	}
	return resolveOne(ctx, reg, q, task, *format)
}

// resolveArgsOK is the verb's shape: one agent word, a known format, --live only with --all-profiles.
func resolveArgsOK(words []string, format string, live, all bool, rt route.Route) bool {
	return len(words) == 1 && helptext.CheckFormat(format) == nil && (!live || all) &&
		(rt.Provider == "" || rt.Provider == providers.GroupDatabricks && (rt.Profile == "" || rt.Profile == agent.AutoModel))
}

// resolveOne prints the first hop's route; an unrunnable model is a usage error.
func resolveOne(ctx context.Context, reg *providers.Registry, q agent.ModelQuery, task, format string) int {
	r, choice, err := resolve.FirstHop(ctx, reg, q, task)
	if errors.Is(err, agent.ErrModelRoute) {
		return helptext.FailUsage("gateway resolve: %v", err)
	} else if err != nil {
		return helptext.Fail("gateway resolve: %v", err)
	}
	return printRoute(resolve.WithGoogleQuota(ctx, r, os.Stderr), choice, format)
}

// checkFallback rejects a --fallback mode the router does not know; empty means unset.
func checkFallback(mode string) error {
	if mode == "" || slices.Contains(router.FallbackModes, mode) {
		return nil
	}
	return fmt.Errorf("--fallback %q unknown (%s)", mode, strings.Join(router.FallbackModes, "|"))
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
			if pool := resolve.PoolMembers(r, choice); pool != nil {
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
	for _, m := range resolve.PoolMembers(r, choice) {
		fmt.Println(resolve.PoolLine(m))
	}
	return 0
}

// printRows prints one PASS/FAIL row per profile (or a JSON array).
func printRows(rows []resolve.ProfileRow, format string) int {
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
	rows := resolve.PoolRows(ctx, reg, resolve.Tiers(task), tools, fallback, time.Now())
	if format == helptext.JSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	resolve.RenderPoolTable(os.Stdout, rows, helptext.TermWidth(os.Stdout), time.Now())
	return 0
}
