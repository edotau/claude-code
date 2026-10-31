package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
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
		if len(pos) != 0 {
			return fail("takes no positional arguments")
		}
		if apply && dryRun {
			return fail("--apply and --dry-run are mutually exclusive")
		}
		if strings.TrimSpace(provider) == "" {
			return fail("--profile is required")
		}
		return done(opencodeMenuSync(provider, opencodeSyncOptions{Apply: apply, Refresh: refresh, Verify: verify}))
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
		next, ok := agent.MenuRemove(providers.OpencodeMenu(), pos[0])
		if !ok {
			return fail("%s is not in the menu", agent.MenuID(pos[0]))
		}
		return done(opencodeMenuWrite(next, "removed "+agent.MenuID(pos[0])))
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

type ocMenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Check     string `json:"check,omitempty"`
}

func opencodeMenuList(live bool, provider, format string) error {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	if live {
		var err error
		if served, p, err = agent.OpencodeServed(provider); err != nil {
			return err
		}
	}
	var cache map[string]agent.CheckResult
	if live {
		cache, _ = agent.LoadCheckCache(p.Name)
	}
	var rows []ocMenuRow
	for _, m := range providers.OpencodeMenu() {
		r := ocMenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := agent.ServedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
			r.Check = checkCell(cache, m.ID)
		}
		rows = append(rows, r)
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
	id, label, _ := strings.Cut(arg, "=")
	id = agent.MenuID(id)
	if id == "" {
		return errors.New("empty model id")
	}
	if !force {
		served, p, err := agent.OpencodeServed(provider)
		if err != nil {
			return fmt.Errorf("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := agent.ServedInfo(served, p, id); !ok {
			return fmt.Errorf("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return opencodeMenuWrite(agent.MenuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r),
		"added "+id)
}

func opencodeMenuImport(file string, apply bool) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	next, err := agent.ParseMenuImport(data)
	if err != nil {
		return fmt.Errorf("%s: %v", file, err)
	}
	changes := agent.DiffMenu(providers.OpencodeMenu(), next)
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

// checkCell renders a cached probe as "ok tools", "ok no-tools", "fail 400", "missing" or "-".
func checkCell(cache map[string]agent.CheckResult, id string) string {
	r, ok := cache[id]
	switch {
	case !ok:
		return "-"
	case r.Status == "ok" && r.Tools == "no":
		return "ok no-tools"
	case r.Status == "ok":
		return "ok tools"
	case r.Status == "fail" && r.HTTP > 0:
		return fmt.Sprintf("fail %d", r.HTTP)
	}
	return r.Status
}

func opencodeMenuCheck(ids []string, all bool, provider string, timeout time.Duration, format string) error {
	served, p, err := agent.OpencodeServed(provider)
	if err != nil {
		return err
	}
	targets := agent.OpencodeCheckTargets(ids, all, served, p)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err := agent.ProbeEndpoints(ctx, p, targets, agent.CheckOptions{Concurrency: 4, Timeout: timeout})
	if err != nil && results == nil {
		return err
	}
	agent.SortCheckResults(results)
	if err := agent.SaveCheckCache(agent.CheckCache{CheckedAt: time.Now().UTC(), Provider: p.Name, Results: results}); err != nil {
		fmt.Fprintf(os.Stderr, "opencode catalog check: cache not written: %v\n", err)
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

type opencodeSyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// opencodeMenuSync only appends absent ids; existing order, labels and explicit Responses flags are untouched.
func opencodeMenuSync(profile string, opts opencodeSyncOptions) error {
	p, snapshot, err := agent.OpencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return err
	}
	ids := agent.OpencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := agent.OpencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return err
		}
	}
	opencodeSyncSummary(snapshot, opts.Verify)
	next, skipped := agent.OpencodeSyncMerge(menu, ids, snapshot.Checks)
	changes := agent.DiffMenu(menu, next)
	fmt.Printf("opencode catalog sync: %d listed, %d eligible, %d additions, %d failed additions skipped, %d retained, %d total\n",
		len(snapshot.Models), len(ids), len(changes), skipped, len(menu), len(next))
	for _, c := range changes {
		fmt.Printf("%s %s  %s  responses=%v\n", c.Op, c.Model.ID, c.Model.Label, c.Model.Responses)
	}
	fmt.Println("Shared menu: OpenCode uses all entries; Codex needs Responses + bundled metadata.")
	fmt.Println("Discovery is not inference proof; use --verify or the existing check command:")
	fmt.Printf("  claude-code opencode catalog check --all --profile %s\n", profile)
	if !opts.Apply {
		fmt.Println("opencode catalog: dry run; no discovery/menu writes; pass --apply to write")
		return nil
	}
	if !snapshot.CacheHit || opts.Verify {
		if err := agent.SaveServingDiscovery(p, snapshot); err != nil {
			return err
		}
		fmt.Println("opencode catalog: discovery snapshot sealed in vault")
	}
	if len(changes) == 0 {
		fmt.Println("opencode catalog: no menu change")
		return nil
	}
	if opencodeMenuWrite(next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(changes))) != nil {
		return errors.New("cannot persist the shared model menu; check vault access")
	}
	return nil
}

func opencodeSyncSummary(snapshot agent.ServingDiscovery, verified bool) {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	fmt.Printf("Valid identity (not endpoint permissions); %s, discovered %s\n", source, snapshot.DiscoveredAt.Format(time.RFC3339))
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		fmt.Println("Verification: listing only; models are discovered/unverified")
		return
	}
	fmt.Printf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted\n",
		label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools)
	fmt.Println("Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}
