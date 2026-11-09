package providerscmd

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/copilot"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

var modelsDiscoverPage = helptext.Page{
	Usage: "claude-code models discover [flags]",
	About: "Live-discover models for one profile, or every registry profile (sorted).\nPreview by default; --apply appends sealed, profile-scoped selectable entries.\nNever removes models, changes pins/routes, or treats listing as inference proof.",
	Hint:  "Vendor-owned native catalogs are not rewritten. /discover-models defaults to --apply.",
}

func modelsDiscover(args []string) int {
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
		return helptext.BadFormat("models discover", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("models discover: cannot load provider registry")
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

func printDiscoverResults(results []copilot.DiscoverResult, apply bool, format string, code int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, struct {
			Apply   bool                     `json:"apply"`
			Results []copilot.DiscoverResult `json:"results"`
		}{apply, results}); err != nil {
			return helptext.Fail("models discover: cannot write JSON")
		}
		return code
	}
	if err := printDiscoverText(results); err != nil {
		return helptext.Fail("models discover: cannot write text output")
	}
	return code
}

func discoverPickerProfiles(reg *providers.Registry, names []string, apply bool) ([]copilot.DiscoverResult, int) {
	results := make([]copilot.DiscoverResult, 0, len(names))
	code, completed := 0, 0
	for _, name := range names {
		p, _ := reg.Get(name)
		r := copilot.DiscoverPickerProfile(p, apply)
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

// printDiscoverText renders the results into one buffer and writes it once, so a stdout write error is returned.
func printDiscoverText(results []copilot.DiscoverResult) error {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "%s/%s: %s source=%s discovered=%d additions=%d\n", r.Provider, r.Profile, r.Status, r.Source, r.Discovered, len(r.Additions))
		for _, m := range r.Additions {
			fmt.Fprintf(&b, "  + %s\n", m.ID)
		}
		if len(r.Destinations) > 0 {
			fmt.Fprintf(&b, "  destinations: %v\n", r.Destinations)
		}
		if r.Note != "" {
			fmt.Fprintf(&b, "  %s\n", r.Note)
		}
	}
	b.WriteString("Saved entries are append-only choices, not current availability or inference proof. Native vendor catalogs are unchanged.\n")
	_, err := io.WriteString(os.Stdout, b.String())
	return err
}
