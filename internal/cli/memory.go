package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/launch"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/paths"
)

// memorySub is one sub-verb's synopsis args and blurb; public ones are the memory page's COMMANDS rows.
type memorySub struct{ name, args, about string }

var memorySubs = []memorySub{
	{"recall", "<terms>", "rank bank blocks for terms; --load appends the top bodies"},
	{"remember", "<fact>", "append a fact to the bank (--type picks the file)"},
	{"init", "", "create the repo's bank"},
	{"review", "", "doctor + stats for the banks; --candidates ranks promotions"},
	{"promote", "<id> --to <target>", "move or surface one block (home|rules|skill|claude-md)"},
	{"update", "", "harvest a finished session (--transcript); bare prints the procedure"},
	{"dream", "", "consolidate the bank when due (--force, --dry-run)"},
}

var memoryPage = helptext.Page{
	Usage: "claude-code memory <command> [flags] [args]",
	About: "Per-repo and home memory banks: session-start injects them, the Stop/SessionEnd\n" +
		"harvest writes them. Recall and save facts, then promote the ones that hold up.",
	Commands: memoryCommandRows(),
	Hint: "plumbing: search, show, index, path, digest, fmt, watch, timeline, archive,\n" +
		"          migrate, alias — dispatchable, off the menu above\n" +
		"claude-code memory <command> -h lists that command's flags.",
}

// memoryCommandRows lists the public sub-verbs as the page's COMMANDS rows; args live on each -h page.
func memoryCommandRows() [][2]string {
	var rows [][2]string
	for _, m := range memorySubs {
		rows = append(rows, [2]string{m.name, m.about})
	}
	return rows
}

// memoryPlumbing is the off-menu sub-verbs' {args, blurb} for their own -h pages.
var memoryPlumbing = map[string][2]string{
	"search":   {"<terms>", "rank blocks for terms, with scores"},
	"show":     {"<id>", "print one block (id from memory search or a MEMORY.md region)"},
	"index":    {"", "settle the bank: rotate, promote and archive over caps"},
	"path":     {"", "print the bank directory"},
	"digest":   {"", "write the bank digest"},
	"fmt":      {"", "wrap the bank files"},
	"timeline": {"", "recent bank entries and sessionHistory"},
}

// memorySubPage is sub's help page: synopsis from its args plus its one-line blurb.
func memorySubPage(sub string) helptext.Page {
	m := memoryPlumbing[sub]
	for _, s := range memorySubs {
		if s.name == sub {
			m = [2]string{s.args, s.about}
		}
	}
	return helptext.Page{Usage: memorySynopsis(sub, m[0]), About: m[1]}
}

// memorySynopsis is `claude-code memory <sub> [flags] <args>`, the usage line of sub's page and its errors.
func memorySynopsis(sub, args string) string {
	if strings.HasPrefix(args, "<id> --to") { // promote: the id and --to target lead
		return "claude-code memory " + sub + " " + args + " [flags]"
	}
	return strings.TrimSpace("claude-code memory " + sub + " [flags] " + args)
}

// memoryAgent runs background memory jobs on the Copilot CLI (claude-opus-5.5, medium), falling back to Claude.
func memoryAgent() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		r, err := agent.Lookup("copilot")
		if err != nil {
			return "", err
		}
		req := agent.Request{Prompt: prompt, Model: "claude-opus-5.5",
			Effort: "medium", Tools: true, WorkDir: workdir}
		notify := func(m string) { fmt.Fprintln(os.Stderr, m) }
		res, err := agent.RunWithFallback(ctx, r, agent.FallbackChain(r, agent.FallbackAll), req, os.Stdout, notify)
		who := res.Agent
		if res.Fallback != "" {
			who += "<-" + res.Fallback
		}
		return who, err
	}
}

// memoryFlagSet builds the per-subcommand flag.FlagSet shared by every handler below.
func memoryFlagSet(sub string) *flag.FlagSet {
	return helptext.NewFlagSet("memory "+sub, memorySubPage(sub))
}

// memoryRepoFlagSet adds --repo; only handlers that resolve a bank root take it, so it is never silently ignored.
func memoryRepoFlagSet(sub string) (*flag.FlagSet, *string) {
	fs := memoryFlagSet(sub)
	return fs, fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
}

func cmdMemoryInit(args []string) int {
	fs, repo := memoryRepoFlagSet("init")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	root := memoryRoot(*repo)
	dir, err := memory.Init(root)
	if err != nil {
		return fail("memory init: %v", err)
	}
	fmt.Printf("bank:    %s\nhome:    %s\nsession-start injects it; `claude-code memory update` (or the background harvest) writes it.\n", dir, memory.HomeBankDir())
	if memory.IsProjectContextTemplate(root) {
		ref, err := memory.Reference("init")
		if err != nil {
			return fail("memory init: %v", err)
		}
		fmt.Println()
		fmt.Println(ref)
	}
	return 0
}

// recallLoadCap bounds `memory recall --load N`'s total appended bytes across every loaded block.
const recallLoadCap = 4096

func cmdMemoryRecall(args []string) int {
	fs, repo := memoryRepoFlagSet("recall")
	k := fs.Int("k", 3, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	load := fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	format := outfmt.Flag(fs)
	terms, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		fail("memory recall: %v", err)
		return 2
	}
	if len(terms) == 0 {
		return fail("memory recall: no query (usage: %s)", memorySubPage("recall").Usage)
	}
	hits := searchHits(*repo, *all, strings.Join(terms, " "), *k)
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, hits)
		return 0
	}
	if len(hits) == 0 {
		fmt.Println("no matching blocks (banks empty or query all stopwords)")
		return 0
	}
	for _, h := range hits {
		fmt.Printf("%s — %s\n", h.ID, h.Title())
	}
	if *load > 0 {
		recallLoad(hits, *load)
	}
	return 0
}

// recallLoad appends the top n hits' full block text (ShowBlock), capped at recallLoadCap bytes total.
func recallLoad(hits []memory.SearchHit, n int) {
	if n > len(hits) {
		n = len(hits)
	}
	budget := recallLoadCap
	for _, h := range hits[:n] {
		if budget <= 0 {
			continue
		}
		_, block, err := memory.ShowBlock(h.ID)
		if err != nil {
			continue
		}
		block = strings.TrimSpace(block)
		if len(block) > budget {
			block = block[:budget]
		}
		fmt.Printf("\n<!-- %s -->\n%s\n", h.ID, block)
		budget -= len(block)
	}
}

func cmdMemoryRemember(args []string) int {
	fs, repo := memoryRepoFlagSet("remember")
	kind := fs.String("type", "feedback", "project->decisionLog.md; user/feedback/reference->conventions.md; "+
		"reads like a rule? use memory promote --to rules instead")
	terms, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(terms) == 0 {
		return fail("memory remember: no fact text (usage: %s)", memorySubPage("remember").Usage)
	}
	root := memoryRoot(*repo)
	id, err := memory.Remember(root, *kind, strings.Join(terms, " "), time.Now())
	if err != nil {
		return fail("memory remember: %v", err)
	}
	fmt.Printf("remembered [%s] -> %s\n", id, memory.Dir(root))
	return 0
}

// searchHits ranks blocks for q: every store with all, else the named store or checkout (default: cwd's).
func searchHits(repo string, all bool, q string, k int) []memory.SearchHit {
	switch {
	case all:
		return memory.SearchAll(q, k)
	case repo != "": // a store name from the catalog, else a checkout path
		root := memoryRoot(repo)
		if e, ok := memory.RepoByName(repo); ok && e.Origin.Origin != "" {
			root = e.Origin.Origin
		}
		return memory.SearchBanks(root, q, k)
	default:
		return memory.SearchBanks(memoryRoot(""), q, k)
	}
}

func cmdMemorySearch(args []string) int {
	fs, repo := memoryRepoFlagSet("search")
	k := fs.Int("k", 8, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	format := outfmt.Flag(fs)
	terms, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := outfmt.Check(*format); err != nil {
		fail("memory search: %v", err)
		return 2
	}
	if len(terms) == 0 {
		return fail("memory search: no query (usage: %s)", memorySubPage("search").Usage)
	}
	hits := searchHits(*repo, *all, strings.Join(terms, " "), *k)
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, hits)
		return 0
	}
	if len(hits) == 0 {
		fmt.Println("no matching blocks (banks empty or query all stopwords)")
	}
	for _, h := range hits {
		id := ""
		if h.ID != "" {
			id = " [" + h.ID + "]"
		}
		fmt.Printf("%5.2f  %-7s %-18s %s%s\n", h.Score, h.Bank, h.File, h.Title(), id)
	}
	return 0
}

func cmdMemoryShow(args []string) int {
	fs := memoryFlagSet("show")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 1 {
		fail("memory show: give one block id (from `memory search` or a MEMORY.md region)")
		return 2
	}
	path, block, err := memory.ShowBlock(fs.Arg(0))
	if err != nil {
		return fail("memory show %s: %v", fs.Arg(0), err)
	}
	fmt.Printf("<!-- %s -->\n%s\n", path, strings.TrimSpace(block))
	return 0
}

func cmdMemoryIndex(args []string) int {
	fs, repo := memoryRepoFlagSet("index")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	root := memoryRoot(*repo)
	s, err := memory.SettleBank(root, time.Now())
	if err != nil {
		return fail("memory index: %v", err)
	}
	fmt.Printf("indexed %s", memory.Dir(root))
	if s.Rotated > 0 {
		fmt.Printf(" (rotated %d sessionHistory entries to archive/)", s.Rotated)
	}
	if s.Promoted > 0 {
		fmt.Printf(" (promoted %d block(s) to home)", s.Promoted)
	}
	if s.Archived > 0 {
		fmt.Printf(" (archived %d block(s))", s.Archived)
	}
	fmt.Println()
	return 0
}

func cmdMemoryPath(args []string) int {
	fs, repo := memoryRepoFlagSet("path")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	fmt.Println(memory.Dir(memoryRoot(*repo)))
	return 0
}

func cmdMemoryUpdate(args []string) int {
	fs := memoryFlagSet("update")
	transcript := fs.String("transcript", "", "finished session's transcript")
	cwd := fs.String("cwd", "", "project root whose bank to harvest (default: cwd)")
	session := fs.String("session", "", "finished session's id (outcome recorded under state/harvest/memory)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *transcript == "" { // a live session: print the narrative procedure instead of refusing outright
		ref, err := memory.Reference("update")
		if err != nil {
			return fail("memory update: %v", err)
		}
		fmt.Println(ref)
		return 0
	}
	root := *cwd
	if root == "" {
		root, _ = os.Getwd()
	}
	claude, _ := launch.ClaudeBinary()
	if err := memory.Update(memory.UpdateOptions{Claude: claude, Run: memoryAgent(), Transcript: *transcript, CWD: root, Session: *session, Out: os.Stdout}); err != nil {
		return fail("memory update: %v", err)
	}
	return 0
}

func cmdMemoryDream(args []string) int {
	fs := memoryFlagSet("dream")
	cwd := fs.String("cwd", "", "project root whose bank to consolidate (default: cwd)")
	force := fs.Bool("force", false, "run even when not due")
	dryRun := fs.Bool("dry-run", false, "deterministic preview: caps, promotable/archivable blocks; no writes")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	root := *cwd
	if root == "" {
		root, _ = os.Getwd()
	}
	if *dryRun {
		fmt.Print(memory.PreviewDream(root, time.Now()).Report())
		return 0
	}
	if !*force && !memory.DreamDue(root, time.Now()) {
		fmt.Println("memory dream: not due")
		return 0
	}
	if err := memory.Dream(memory.DreamOptions{CWD: root, Run: memoryAgent(), Out: os.Stdout}); err != nil {
		return fail("memory dream: %v", err)
	}
	return 0
}

func cmdMemoryDigest(args []string) int {
	fs, repo := memoryRepoFlagSet("digest")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	claude, _ := launch.ClaudeBinary()
	path, err := memory.Digest(memory.Dir(memoryRoot(*repo)), claude)
	if err != nil {
		return fail("memory digest: %v", err)
	}
	fmt.Println("wrote " + path)
	return 0
}

func cmdMemoryWatch(args []string) int {
	return hooks.IdleHarvestWatch(args, os.Stderr) // spawned detached by the Stop gate; its own flag set
}

func cmdMemoryFmt(args []string) int {
	fs := memoryFlagSet("fmt")
	check := fs.Bool("check", false, "report would-change files; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	changed, err := memory.FormatBank(*check)
	if err != nil {
		return fail("memory fmt: %v", err)
	}
	for _, f := range changed {
		fmt.Println(f)
	}
	if *check {
		if len(changed) > 0 {
			fmt.Printf("%d file(s) would change (run `claude-code memory fmt`)\n", len(changed))
			return 1
		}
		fmt.Println("memory fmt --check: clean")
		return 0
	}
	fmt.Printf("wrapped %d file(s)\n", len(changed))
	return 0
}

// cmdMemoryReview folds the old `doctor` + `stats` verbs: same engine calls, same output blocks.
func cmdMemoryReview(args []string) int {
	fs, repo := memoryRepoFlagSet("review")
	staleDays := fs.Int("stale-days", memory.DefaultStaleDays, "flag a bank untouched for this many days")
	brief := fs.Bool("brief", false, "print only the problem count, not each line")
	notice := fs.Bool("notice", false, "refresh the cached session-start notice and print nothing")
	candidates := fs.Bool("candidates", false, "print today's stats, then the ranking procedure")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *notice {
		if err := memory.WriteDoctorNotice(memoryRoot(*repo), *staleDays, time.Now()); err != nil {
			return fail("memory review: %v", err)
		}
		return 0
	}
	problems := memory.Doctor(memoryRoot(*repo), *staleDays, time.Now())
	switch {
	case len(problems) == 0:
		fmt.Println("memory doctor: no problems found")
	case *brief:
		fmt.Printf("memory doctor: %d problem(s) found\n", len(problems))
	default:
		for _, p := range problems {
			fmt.Println("- " + p)
		}
	}
	fmt.Print(memory.CollectBankStats().Report())
	if *candidates {
		ref, err := memory.Reference("review")
		if err != nil {
			return fail("memory review: %v", err)
		}
		fmt.Println()
		fmt.Println(ref)
	}
	return 0
}

func cmdMemoryTimeline(args []string) int {
	fs, repo := memoryRepoFlagSet("timeline")
	n := fs.Int("n", 20, "max entries")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	entries, sessions := memory.Timeline(memoryRoot(*repo), *n)
	for _, e := range entries {
		fmt.Printf("%s  %-8s %s\n", e.Time.Format("2006-01-02 15:04"), e.Kind, e.Line)
	}
	if len(sessions) > 0 {
		fmt.Println("-- sessionHistory --")
		for _, s := range sessions {
			fmt.Println("- " + s)
		}
	}
	return 0
}

// cmdMemoryPromote moves/surfaces one block by id: "home" is a deterministic move, settled both
// sides; "rules"/"skill"/"claude-md" print the block + target for a human/agent to apply by hand
// (model residue) — Go never writes those targets. `memory index` already settles caps on its own.
func cmdMemoryPromote(args []string) int {
	fs, repo := memoryRepoFlagSet("promote")
	to := fs.String("to", "", "promotion target: home, rules, skill, or claude-md")
	dryRun := fs.Bool("dry-run", false, "print the move without writing")
	pos, err := outfmt.Parse(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || *to == "" {
		fail("memory promote: usage: %s", memorySubPage("promote").Usage)
		return 2
	}
	out, err := memory.Promote(memoryRoot(*repo), pos[0], *to, *dryRun, time.Now())
	if err != nil {
		return fail("memory promote: %v", err)
	}
	fmt.Print(out)
	return 0
}

// memorySubcommands follows the top-level verb table in internal/cli/cli.go: public verbs are
// memoryPage's COMMANDS rows, private ones dispatch here but only appear in its trailing "plumbing:" line.
var memorySubcommands = map[string]func([]string) int{
	// public
	"recall":   cmdMemoryRecall,
	"remember": cmdMemoryRemember,
	"init":     cmdMemoryInit,
	"review":   cmdMemoryReview,
	"promote":  cmdMemoryPromote,
	"update":   cmdMemoryUpdate,
	"dream":    cmdMemoryDream,
	// private: plumbing
	"search":   cmdMemorySearch,
	"show":     cmdMemoryShow,
	"index":    cmdMemoryIndex,
	"path":     cmdMemoryPath,
	"digest":   cmdMemoryDigest,
	"fmt":      cmdMemoryFmt,
	"watch":    cmdMemoryWatch,
	"timeline": cmdMemoryTimeline,
	"archive":  cmdMemoryArchive,
	"migrate":  cmdMemoryMigrate,
	"alias":    cmdMemoryAlias,
}

func cmdMemory(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		memoryPage.Fprint(os.Stderr)
		return 0
	}
	sub := args[0]
	switch sub {
	case "index", "promote", "update", "dream", "fmt", "remember": // settle/harvest writers: a harvest receipt stays pending; SessionStart surfaces it
		if g := memory.StartupGuard(); g.Paused {
			return fail("memory %s: %s", sub, g.Notice)
		}
	}
	h, ok := memorySubcommands[sub]
	if !ok {
		return fail("memory: unknown subcommand %q (claude-code memory -h lists them)", sub)
	}
	return h(args[1:])
}

// memoryRoot resolves --repo → $CLAUDE_PROJECT_DIR (only when it is a repo or the config dir) → git toplevel → cwd.
// A launcher may stamp CLAUDE_PROJECT_DIR with a non-repo parent (e.g. $HOME); honoring that attached every
// bank verb to a junk root instead of the repo the user is standing in.
func memoryRoot(repo string) string {
	if repo != "" {
		abs, _ := filepath.Abs(repo)
		return abs
	}
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" && (paths.SamePath(d, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(d), paths.ConfigDir()) || paths.SamePath(memory.GitToplevel(d), d)) {
		return d
	}
	cwd, _ := os.Getwd()
	// Inside the store its own toplevel is the store root; keep cwd so memory/<repo>/ resolves to that repo.
	if top := memory.GitToplevel(cwd); top != "" && !paths.SamePath(top, memory.StoreDir()) {
		return top
	}
	return cwd
}
