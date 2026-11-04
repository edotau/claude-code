package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var modelsDiscoverPage = helptext.Page{
	Usage: "claude-code models discover [flags]",
	About: "Live-discover models for one profile, or every registry profile (sorted).\nPreview by default; --apply appends sealed, profile-scoped selectable entries.\nNever removes models, changes pins/routes, or treats listing as inference proof.",
	Hint:  "Vendor-owned native catalogs are not rewritten. /discover-models defaults to --apply.",
}

type discoverResult struct {
	Provider     string                `json:"provider"`
	Profile      string                `json:"profile"`
	Source       string                `json:"source"`
	Discovered   int                   `json:"discovered"`
	Additions    []providers.ModelInfo `json:"additions"`
	Destinations []string              `json:"destinations"`
	Status       string                `json:"status"`
	Note         string                `json:"note,omitempty"`
}

func cmdModelsDiscover(args []string) int {
	fs := helptext.NewFlagSet("models discover", modelsDiscoverPage)
	profile := fs.String("profile", "", "discover only this registry profile")
	apply := fs.Bool("apply", false, "append discovered additions to saved runtime pickers")
	dry := fs.Bool("dry-run", false, "preview only (the default); conflicts with --apply")
	format := helptext.FormatFlag(fs)
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 0 || (*apply && *dry) {
		fmt.Fprintln(os.Stderr, "claude-code: models discover: unexpected argument or conflicting --apply/--dry-run")
		return 2
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("models discover", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("models discover: cannot load provider registry")
	}
	names := reg.Names()
	slices.Sort(names)
	if *profile != "" {
		if _, err := reg.Get(*profile); err != nil {
			fmt.Fprintln(os.Stderr, "claude-code: models discover: unknown profile")
			return 2
		}
		names = []string{*profile}
	}
	results, code := discoverPickerProfiles(reg, names, *apply)
	return printDiscoverResults(results, *apply, *format, code)
}

func printDiscoverResults(results []discoverResult, apply bool, format string, code int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool             `json:"apply"`
			Results []discoverResult `json:"results"`
		}{apply, results}); err != nil {
			return fail("models discover: cannot write JSON")
		}
	} else {
		printDiscoverText(results)
	}
	return code
}

func discoverPickerProfiles(reg *providers.Registry, names []string, apply bool) ([]discoverResult, int) {
	results := make([]discoverResult, 0, len(names))
	code, completed := 0, 0
	for _, name := range names {
		p, _ := reg.Get(name)
		r := discoverPickerProfile(p, apply)
		if r.Status == "error" {
			code = 1
		}
		if r.Status == "applied" || r.Status == "preview" {
			completed++
		}
		results = append(results, r)
	}
	if completed == 0 {
		code = 1
	}
	return results, code
}

func printDiscoverText(results []discoverResult) {
	for _, r := range results {
		fmt.Printf("%s/%s: %s source=%s discovered=%d additions=%d\n", r.Provider, r.Profile, r.Status, r.Source, r.Discovered, len(r.Additions))
		for _, m := range r.Additions {
			fmt.Printf("  + %s\n", m.ID)
		}
		if len(r.Destinations) > 0 {
			fmt.Printf("  destinations: %v\n", r.Destinations)
		}
		if r.Note != "" {
			fmt.Printf("  %s\n", r.Note)
		}
	}
	fmt.Println("Saved entries are append-only choices, not current availability or inference proof. Native vendor catalogs are unchanged.")
}

func discoverPickerProfile(p *providers.Provider, apply bool) discoverResult {
	r := discoverResult{Provider: p.Provider, Profile: p.Name, Additions: []providers.ModelInfo{}, Destinations: []string{}, Status: "skipped"}
	source, skip := copilot.DiscoverySource(p)
	r.Source = source
	if skip != "" {
		r.Note = skip
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	live, err := copilot.DiscoverProfileModels(ctx, p)
	if err != nil {
		r.Status = "error"
		r.Note = err.Error() + " (no fallback saved)"
		return r
	}
	r.Discovered = len(live)
	r.Destinations = []string{"configure model picker", "agent model picker", "OpenCode harness-generated catalog (next routed launch)"}
	if p.Preset == providers.PresetDatabricks {
		r.Destinations = append(r.Destinations, "Codex generated catalog (Responses models with bundled metadata only)")
	}
	var added []providers.ModelInfo
	if apply {
		added, err = providers.AppendPickerModels(p, live)
	} else {
		added, err = providers.PreviewPickerModels(p, live)
	}
	if err != nil {
		r.Status = "error"
		r.Note = "saved picker additions unavailable or invalid; nothing overwritten: " + err.Error()
		return r
	}
	if len(added) > 0 {
		r.Additions = added
	}
	r.Status = "preview"
	if apply {
		r.Status = "applied"
	}
	r.Note = "Listing is not inference proof; existing selections and native vendor catalogs are unchanged."
	if p.Provider == "github" {
		r.Destinations = []string{"sealed profile history only (Copilot pickers retain live policy filtering)"}
	}
	return r
}
