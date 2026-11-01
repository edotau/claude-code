package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// memorySub is one sub-verb's synopsis args and blurb; public ones are the memory page's COMMANDS rows.
type memorySub struct{ name, args, about string }

var memorySubs = []memorySub{
	{"recall", "<terms>", "rank bank blocks for terms; --load appends the top bodies"},
	{"remember", "<fact>", "append a fact to the bank (--type picks the file)"},
	{"init", "", "create the repo's bank"},
	{"review", "", "doctor + stats for the banks; --candidates ranks promotions"},
	{"verify", "", "check bank claims against local evidence; never write or fetch"},
	{"promote", "<id> --to <target>", "move or surface one block (home|rules|skill|claude-md)"},
	{"update", "", "save the live session or --transcript; else print the procedure"},
	{"dream", "", "consolidate the bank when due (--force, --dry-run)"},
}

var memoryPage = helptext.Page{
	Usage: "claude-code memory <command> [flags] [args]",
	About: "Per-repo and home memory banks: session-start injects them, the Stop/SessionEnd\n" +
		"update writes them. Recall and save facts, then promote the ones that hold up.",
	Commands: memoryCommandRows(),
	Hint: "plumbing: search, show, index, path, digest, fmt, watch, timeline, archive,\n" +
		"          alias — dispatchable, off the menu above\n" +
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

// memoryAgent runs background memory jobs on Claude as the logged-in user, on their pin (medium effort).
func memoryAgent() memory.AgentFunc {
	return func(ctx context.Context, prompt, workdir string) (string, error) {
		client := newClient()
		client.Refresh = nil
		client.Notify = func(m string) { fmt.Fprintln(os.Stderr, m) }
		res, err := client.Run(ctx, agent.RunOptions{Agent: "claude", Prompt: prompt,
			Effort: "medium", Tools: true, WorkDir: workdir}, agent.NewTextSink(os.Stdout))
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
	if err := memory.InitBank(os.Stdout, memstore.Root(*repo)); err != nil {
		return fail("memory init: %v", err)
	}
	return 0
}

func cmdMemoryRemember(args []string) int {
	fs, repo := memoryRepoFlagSet("remember")
	kind := fs.String("type", "feedback", "project->decisionLog.md; user/feedback/reference->conventions.md; "+
		"reads like a rule? use memory promote --to rules instead")
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(terms) == 0 {
		return fail("memory remember: no fact text (usage: %s)", memorySubPage("remember").Usage)
	}
	root := memstore.Root(*repo)
	id, err := bank.Remember(root, *kind, strings.Join(terms, " "), time.Now())
	if err != nil {
		return fail("memory remember: %v", err)
	}
	fmt.Printf("remembered [%s] -> %s\n", id, memstore.Dir(root))
	return 0
}

func cmdMemorySearch(args []string) int {
	fs, repo := memoryRepoFlagSet("search")
	k := fs.Int("k", 8, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	format := helptext.FormatFlag(fs)
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return badFormat("memory search", err)
	}
	if len(terms) == 0 {
		return fail("memory search: no query (usage: %s)", memorySubPage("search").Usage)
	}
	res := memory.Search(memory.SearchOptions{Repo: *repo, All: *all, Query: strings.Join(terms, " "), K: *k,
		Expand: len(terms) == 1 && *format != helptext.JSON})
	if res.Block != "" {
		fmt.Printf("<!-- %s -->\n%s\n", res.Path, strings.TrimSpace(res.Block))
		return 0
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, res.Hits); err != nil {
			return fail("memory search: %v", err)
		}
		return 0
	}
	if len(res.Hits) == 0 {
		fmt.Println("no matching blocks (banks empty or query all stopwords)")
	}
	for _, h := range res.Hits {
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
		return failUsage("memory show: give one block id (from `memory search` or a MEMORY.md region)")
	}
	path, block, err := memory.Show(fs.Arg(0))
	if err != nil {
		return fail("memory show %s: %v", fs.Arg(0), err)
	}
	fmt.Printf("<!-- %s -->\n%s\n", path, strings.TrimSpace(block))
	return 0
}

func cmdMemoryIndex(args []string) int {
	fs, repo := memoryRepoFlagSet("index")
	reembed := fs.Bool("reembed", false, "re-embed all entries; last 8 min of the lease or expired")
	mintOnly := fs.Bool("mint-only", false, "refresh embedding cache without settling bank contents")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := memory.Index(os.Stdout, os.Stderr, memory.IndexOptions{Repo: *repo, Reembed: *reembed, MintOnly: *mintOnly}); err != nil {
		return fail("%v", err)
	}
	return 0
}

func cmdMemoryPath(args []string) int {
	fs, repo := memoryRepoFlagSet("path")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	fmt.Println(memstore.Dir(memstore.Root(*repo)))
	return 0
}

func cmdMemoryUpdate(args []string) int {
	fs := memoryFlagSet("update")
	transcript := fs.String("transcript", "", "finished session's transcript")
	cwd := fs.String("cwd", "", "bank's project root (default: session's, else cwd)")
	session := fs.String("session", "", "finished session's id; result: update-result-<id>")
	dryRun := fs.Bool("dry-run", false, "print the resolved transcript and session; update nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return failUsage("memory update: unexpected argument %q; name a session with --session <id>", fs.Arg(0))
	}
	err := memory.UpdateSession(memory.UpdateSessionOptions{Transcript: *transcript, CWD: *cwd, Session: *session,
		DryRun: *dryRun, ClaudeBin: claude.ClaudeBinary, Run: memoryAgent(), Out: os.Stdout, Err: os.Stderr})
	if err != nil {
		return fail("memory update: %v", err)
	}
	return 0
}

func cmdMemoryVerify(args []string) int {
	fs := memoryFlagSet("verify")
	cwd := fs.String("cwd", "", "project root whose bank to verify (default: cwd)")
	archives := fs.Bool("archives", false, "include archived bank blocks and native files")
	all := fs.Bool("all-stores", false, "verify every store, including home")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return failUsage("memory verify: unexpected argument %q", fs.Arg(0))
	}
	if err := memory.Verify(os.Stdout, memory.VerifyOptions{CWD: *cwd, Archives: *archives,
		AllStores: *all, HelpVerb: memoryVerifyHelpVerb}); err != nil {
		return fail("memory verify: %v", err)
	}
	return 0
}

var memoryVerifyHelpVerbs = map[string]bool{}

func init() {
	for _, command := range commands() {
		memoryVerifyHelpVerbs[command.name] = true
	}
}

// memoryVerifyHelpVerb excludes launchers that forward -h after credential resolution or other side effects.
func memoryVerifyHelpVerb(verb string) bool {
	switch verb {
	case "claude", "opus", "sonnet", "haiku", "fable", "science", "wl-paste":
		return false
	}
	return memoryVerifyHelpVerbs[verb]
}

func cmdMemoryDream(args []string) int {
	fs := memoryFlagSet("dream")
	cwd := fs.String("cwd", "", "project root whose bank to consolidate (default: cwd)")
	force := fs.Bool("force", false, "run even when not due")
	dryRun := fs.Bool("dry-run", false, "deterministic preview: caps, promotable/archivable blocks; no writes")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := memory.RunDream(memory.DreamRunOptions{CWD: *cwd, Force: *force, DryRun: *dryRun, Run: memoryAgent(), Out: os.Stdout}); err != nil {
		return fail("memory dream: %v", err)
	}
	return 0
}

func cmdMemoryDigest(args []string) int {
	fs, repo := memoryRepoFlagSet("digest")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	claudeBin, _ := claude.ClaudeBinary()
	path, err := memory.DigestRepo(*repo, claudeBin)
	if err != nil {
		return fail("memory digest: %v", err)
	}
	fmt.Println("wrote " + path)
	return 0
}

func cmdMemoryWatch(args []string) int {
	return hooks.IdleUpdateWatch(args, os.Stderr) // spawned detached by the Stop gate; its own flag set
}

func cmdMemoryFmt(args []string) int {
	fs := memoryFlagSet("fmt")
	check := fs.Bool("check", false, "report would-change files; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	dirty, err := memory.FormatReport(os.Stdout, *check)
	if err != nil {
		return fail("memory fmt: %v", err)
	}
	if dirty {
		return 1
	}
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
	err := memory.ReviewReport(os.Stdout, memory.ReviewOptions{Repo: *repo, StaleDays: *staleDays, Brief: *brief,
		Notice: *notice, Candidates: *candidates})
	if err != nil {
		return fail("memory review: %v", err)
	}
	return 0
}

func cmdMemoryTimeline(args []string) int {
	fs, repo := memoryRepoFlagSet("timeline")
	n := fs.Int("n", 20, "max entries")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	memory.TimelineReport(os.Stdout, *repo, *n)
	return 0
}

// cmdMemoryPromote moves/surfaces one block by id: "home" is a deterministic move, settled both
// sides; "rules"/"skill"/"claude-md" print the block + target for a human/agent to apply by hand
// (model residue) — Go never writes those targets. `memory index` already settles caps on its own.
func cmdMemoryPromote(args []string) int {
	fs, repo := memoryRepoFlagSet("promote")
	to := fs.String("to", "", "promotion target: home, rules, skill, or claude-md")
	dryRun := fs.Bool("dry-run", false, "print the move without writing")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || *to == "" {
		fail("memory promote: usage: %s", memorySubPage("promote").Usage)
		return 2
	}
	out, err := memory.Promote(memstore.Root(*repo), pos[0], *to, *dryRun, time.Now())
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
	"verify":   cmdMemoryVerify,
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
	"alias":    cmdMemoryAlias,
}

func cmdMemory(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		memoryPage.Fprint(os.Stderr)
		return 0
	}
	sub := args[0]
	h, ok := memorySubcommands[sub]
	if !ok {
		return fail("memory: unknown subcommand %q (claude-code memory -h lists them)", sub)
	}
	return h(args[1:])
}

var memoryAliasPage = helptext.Page{
	Usage: "claude-code memory alias --repo CHECKOUT --into KEY [flags]",
	About: "Record CHECKOUT as an alias of store KEY and fold any separate store into KEY.",
}

// cmdMemoryAlias is `memory alias`: --dry-run prints the plan and writes nothing.
func cmdMemoryAlias(args []string) int {
	fs := helptext.NewFlagSet("memory alias", memoryAliasPage)
	repo := fs.String("repo", "", "second checkout of the repo to record as an alias")
	into := fs.String("into", "", "store key that absorbs it (see the catalog)")
	dryRun := fs.Bool("dry-run", false, "print the plan; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *repo == "" || *into == "" || fs.NArg() > 0 {
		return fail("memory alias: --repo and --into are required (usage: %s)", memoryAliasPage.Usage)
	}
	res, err := memory.AliasRepo(memory.AliasOptions{Repo: *repo, Into: *into, DryRun: *dryRun, Now: time.Now()})
	if err != nil {
		return fail("memory alias: %v", err)
	}
	verb := "applied"
	if *dryRun {
		verb = "plan (dry run, nothing written)"
	}
	fmt.Println(verb + ":")
	for _, l := range res.Lines {
		fmt.Println("  " + l)
	}
	return 0
}
