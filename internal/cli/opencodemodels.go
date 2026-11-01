package cli

import (
	"errors"
	"flag"
	"fmt"
	"github.com/edotau/claude-code/internal/opencode"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// opencodeMenuVerbs are the `opencode catalog` subverbs this file owns; bare `catalog` stays the CLI's own catalog.
var opencodeMenuVerbs = []string{"list", "add", "remove", "import", "reset", "check", "sync"}

func isOpencodeMenuVerb(name string, args []string) bool {
	return name == "opencode" && len(args) > 0 && slices.Contains(opencodeMenuVerbs, args[0])
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

// cmdOpencodeMenu is `claude-code opencode catalog list|add|remove|import|reset|check|sync`.
func cmdOpencodeMenu(args []string) int {
	verb, rest := args[0], args[1:]
	fs := opencodeMenuFlags(verb)
	var live, force, apply, responses, dryRun, refresh, verify bool
	var provider string
	var all bool
	timeout := 30 * time.Second
	format := new(string)
	if verb == "list" || verb == "check" {
		format = helptext.FormatFlag(fs)
	}
	switch verb {
	case "list":
		fs.BoolVar(&live, "live", false, "mark each entry served or missing on the workspace")
		fs.StringVar(&provider, "profile", "", "workspace profile (default: opencode's record profile)")
	case "check":
		fs.BoolVar(&all, "all", false, "probe every chat endpoint, not just the menu")
		fs.StringVar(&provider, "profile", "", "workspace profile (default: opencode's record profile)")
		fs.DurationVar(&timeout, "timeout", timeout, "per-request timeout")
	case "add":
		fs.BoolVar(&responses, "responses", false, "serve through the Responses API")
		fs.BoolVar(&force, "force", false, "skip the workspace check")
		fs.StringVar(&provider, "profile", "", "workspace profile (default: opencode's record profile)")
	case "import":
		fs.BoolVar(&apply, "apply", false, "write the change (default: print the diff)")
	case "sync":
		fs.StringVar(&provider, "profile", "", "Databricks serving profile (required)")
		fs.BoolVar(&apply, "apply", false, "seal discovery and persist menu additions")
		fs.BoolVar(&dryRun, "dry-run", false, "preview additions without writing (default)")
		fs.BoolVar(&refresh, "refresh", false, "rediscover instead of using cached models")
		fs.BoolVar(&verify, "verify", false, "probe eligible models; exclude failed additions")
	}
	pos, err := helptext.ParseArgs(fs, rest)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if verb == "list" || verb == "check" {
		if err := helptext.CheckFormat(*format); err != nil {
			return badFormat("opencode catalog "+verb, err)
		}
	}
	fail := func(f string, a ...any) int { return fail("opencode catalog "+verb+": "+f, a...) }
	done := func(err error) int {
		if err != nil {
			return fail("%v", err)
		}
		return 0
	}
	switch verb {
	case "sync":
		if err := opencodeSyncArgs(pos, apply, dryRun, provider); err != nil {
			return done(err)
		}
		return done(opencodeMenuSync(provider, opencode.SyncOptions{Apply: apply, Refresh: refresh, Verify: verify}))
	case "list":
		return done(opencodeMenuList(live, provider, *format))
	case "check":
		if all && len(pos) > 0 {
			return fail("--all and explicit ids are exclusive")
		}
		return done(opencodeMenuCheck(pos, all, provider, timeout, *format))
	case "add":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode catalog add <id>[=Label]")
		}
		set := false
		fs.Visit(func(f *flag.Flag) { set = set || f.Name == "responses" })
		return done(opencodeMenuAdd(pos[0], responses, set, force, provider))
	case "remove":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode catalog remove <id>")
		}
		next, ok := opencode.MenuRemove(providers.OpencodeMenu(), pos[0])
		if !ok {
			return fail("%s is not in the menu", opencode.MenuID(pos[0]))
		}
		return done(opencodeMenuWrite(next, "removed "+opencode.MenuID(pos[0])))
	case "import":
		if len(pos) != 1 {
			return fail("usage: claude-code opencode catalog import <file> [--apply]")
		}
		return done(opencodeMenuImport(pos[0], apply))
	}
	if len(pos) != 0 {
		return fail("takes no arguments")
	}
	return done(opencodeMenuWrite(nil, "reset to the built-in menu"))
}

// opencodeMenuWrite stores next. opencode's databricks config is built from the menu at every launch (and
// codex's catalog likewise), so there is no rendered file to refresh; the next launch picks the change up.
func opencodeMenuWrite(next []providers.OpencodeModel, msg string) error {
	if err := providers.SetOpencodeMenu(next); err != nil {
		return err
	}
	fmt.Printf("opencode catalog: %s; relaunch `claude-code opencode` to apply\n", msg)
	return nil
}

func opencodeMenuList(live bool, provider, format string) error {
	rows, err := opencode.MenuRows(live, provider)
	if err != nil {
		return err
	}
	if format == helptext.JSON {
		_ = helptext.WriteJSON(os.Stdout, rows)
		return nil
	}
	idW, labW := len("ID"), len("LABEL")
	for _, r := range rows {
		idW, labW = max(idW, len(r.ID)), max(labW, len(r.Label))
	}
	head := fmt.Sprintf("%-*s  %-*s  %-9s  %s", idW, "ID", labW, "LABEL", "RESPONSES", "SOURCE")
	if live {
		head += "  LIVE                  CHECK"
	}
	fmt.Println(head)
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %-*s  %-9v  %s", idW, r.ID, labW, r.Label, r.Responses, r.Source)
		if live {
			line += fmt.Sprintf("  %-20s  %s", strings.TrimSpace(r.Live+" "+r.Ready), r.Check)
		}
		fmt.Println(line)
	}
	return nil
}

func opencodeMenuAdd(arg string, responses, responsesSet, force bool, provider string) error {
	id, next, err := opencode.MenuAddServed(arg, responses, responsesSet, force, provider)
	if err != nil {
		return err
	}
	return opencodeMenuWrite(next, "added "+id)
}

func opencodeMenuImport(file string, apply bool) error {
	next, changes, err := opencode.MenuImportDiff(file)
	if err != nil {
		return err
	}
	for _, c := range changes {
		line := fmt.Sprintf("%s %s  %s", c.Op, c.Model.ID, c.Model.Label)
		if c.Op == "~" {
			line += fmt.Sprintf("  (was %q responses=%v; now responses=%v)", c.Was.Label, c.Was.Responses, c.Model.Responses)
		}
		fmt.Println(line)
	}
	if len(changes) == 0 {
		fmt.Println("opencode catalog: no change")
		return nil
	}
	if !apply {
		fmt.Printf("opencode catalog: dry run, %d change(s); pass --apply to write\n", len(changes))
		return nil
	}
	return opencodeMenuWrite(next, fmt.Sprintf("imported %d model(s)", len(next)))
}

func opencodeMenuCheck(ids []string, all bool, provider string, timeout time.Duration, format string) error {
	results, cacheErr, err := opencode.MenuCheck(ids, all, provider, timeout)
	if err != nil {
		return err
	}
	if cacheErr != nil {
		fmt.Fprintf(os.Stderr, "opencode catalog check: cache not written: %v\n", cacheErr)
	}
	if format == helptext.JSON {
		_ = helptext.WriteJSON(os.Stdout, results)
		return nil
	}
	idW := len("ID")
	for _, r := range results {
		idW = max(idW, len(r.ID))
	}
	fmt.Printf("%-*s  %-7s  %-4s  %-10s  %s\n", idW, "ID", "STATUS", "HTTP", "LATENCY", "TOOLS")
	for _, r := range results {
		code, lat, tools := "-", "-", "-"
		if r.HTTP > 0 {
			code = strconv.Itoa(r.HTTP)
		}
		if r.Status != "missing" {
			lat = fmt.Sprintf("%dms", r.LatencyMS)
		}
		if r.Tools != "" {
			tools = r.Tools
		}
		fmt.Printf("%-*s  %-7s  %-4s  %-10s  %s", idW, r.ID, r.Status, code, lat, tools)
		if r.Status == "fail" && r.Error != "" {
			fmt.Print("  " + r.Error)
		}
		fmt.Println()
	}
	return nil
}

func opencodeMenuSync(profile string, opts opencode.SyncOptions) error {
	pl, err := opencode.MenuSync(profile, opts)
	if err != nil {
		return err
	}
	for _, line := range opencode.SyncSummaryLines(pl.Snapshot, opts.Verify) {
		fmt.Println(line)
	}
	fmt.Printf("opencode catalog sync: %d listed, %d eligible, %d additions, %d failed additions skipped, %d retained, %d total\n",
		len(pl.Snapshot.Models), len(pl.IDs), len(pl.Changes), pl.Skipped, len(pl.Menu), len(pl.Next))
	for _, c := range pl.Changes {
		fmt.Printf("%s %s  %s  responses=%v\n", c.Op, c.Model.ID, c.Model.Label, c.Model.Responses)
	}
	fmt.Println("Shared menu: OpenCode uses all entries; Codex needs Responses + bundled metadata.")
	fmt.Println("Discovery is not inference proof; use --verify or the existing check command:")
	fmt.Printf("  claude-code opencode catalog check --all --profile %s\n", profile)
	if !opts.Apply {
		fmt.Println("opencode catalog: dry run; no discovery/menu writes; pass --apply to write")
		return nil
	}
	sealed, err := pl.Seal()
	if err != nil {
		return err
	}
	if sealed {
		fmt.Println("opencode catalog: discovery snapshot sealed in vault")
	}
	if len(pl.Changes) == 0 {
		fmt.Println("opencode catalog: no menu change")
		return nil
	}
	if opencodeMenuWrite(pl.Next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(pl.Changes))) != nil {
		return errors.New("cannot persist the shared model menu; check vault access")
	}
	return nil
}
