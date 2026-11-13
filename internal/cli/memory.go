package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// memorySub is one sub-verb's synopsis args and blurb; public ones are the memory page's COMMANDS rows.
type memorySub struct{ name, args, about string }

var memorySubs = []memorySub{
	{"recall", "<terms>", "rank bank blocks for terms; --load appends the top bodies"},
	{"remember", "<fact>", "append a fact to the bank (--type picks the file)"},
	{"init", "", "create the repo's bank"},
	{"review", "", "doctor + bank stats; --candidates adds the promotion procedure"},
	{"promote", "<id> --to <target>", "move one block to home, or print it for rules|skill"},
	{"update", "", "save the live session or --transcript; else print the procedure"},
	{"dream", "", "consolidate the bank when due (--force, --dry-run)"},
}

var memoryPage = helptext.Page{
	Usage: "claude-code memory <command> [flags] [args]",
	About: "Per-repo and home memory banks: session-start injects them, the Stop/SessionEnd\n" +
		"update writes them. Recall and save facts, then promote the ones that hold up.",
	Commands: memoryCommandRows(),
	Hint: "plumbing: search, show, index, path, report, digest, fmt, watch,\n" +
		"          catchup, timeline, alias, import — dispatchable, off the menu above\n" +
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
	"index":    {"", "settle the bank: rotate and archive over caps; refresh the embedding cache"},
	"path":     {"", "print the bank directory"},
	"report":   {"--label L [--file F | --stdin]", "append a report and its decisions to the bank reports/"},
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

// memoryFlagSet builds the per-subcommand flag.FlagSet shared by every handler below.
func memoryFlagSet(sub string) *flag.FlagSet {
	return helptext.NewFlagSet("memory "+sub, memorySubPage(sub))
}

// memoryRepoFlagSet adds --repo; only handlers that resolve a bank root take it, so it is never silently ignored.
func memoryRepoFlagSet(sub string) (*flag.FlagSet, *string) {
	fs := memoryFlagSet(sub)
	return fs, fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
}

// memoryParseNoArgs parses args and refuses a positional: it stops flag parsing, so later flags would be ignored.
func memoryParseNoArgs(fs *flag.FlagSet, sub string, args []string) (code int, done bool) {
	if code, done := helptext.Parse(fs, args); done {
		return code, true
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory %s: unexpected argument %q", sub, fs.Arg(0)), true
	}
	return 0, false
}

func cmdMemoryUpdate(args []string) int {
	fs := memoryFlagSet("update")
	transcript := fs.String("transcript", "", "finished session's transcript")
	cwd := fs.String("cwd", "", "bank's project root (default: session's, else cwd)")
	session := fs.String("session", "", "finished session's id; result: update-result-<id>")
	dryRun := fs.Bool("dry-run", false, "print the resolved transcript and session; update nothing")
	historical := fs.Bool("historical", false, "older session: history and decisions only")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory update: unexpected argument %q; name a session with --session <id>", fs.Arg(0))
	}
	err := memory.UpdateSession(memory.UpdateSessionOptions{Transcript: *transcript, CWD: *cwd, Session: *session,
		DryRun: *dryRun, Historical: *historical, Run: ask.MemoryRunner(), Out: os.Stdout})
	if err != nil {
		return helptext.Fail("memory update: %v", err)
	}
	return 0
}

func cmdMemoryDream(args []string) int {
	fs := memoryFlagSet("dream")
	cwd := fs.String("cwd", "", "project root whose bank to consolidate (default: cwd)")
	force := fs.Bool("force", false, "run even when not due")
	dryRun := fs.Bool("dry-run", false, "deterministic preview: caps, archivable blocks; no writes")
	if code, done := memoryParseNoArgs(fs, "dream", args); done {
		return code
	}
	if err := memory.RunDream(memory.DreamRunOptions{CWD: *cwd, Force: *force, DryRun: *dryRun, Run: ask.MemoryRunner(), Out: os.Stdout}); err != nil {
		return helptext.Fail("memory dream: %v", err)
	}
	return 0
}

func cmdMemoryDigest(args []string) int {
	fs, repo := memoryRepoFlagSet("digest")
	if code, done := memoryParseNoArgs(fs, "digest", args); done {
		return code
	}
	claudeBin, binErr := claude.ClaudeBinary()
	if binErr != nil {
		return helptext.Fail("memory digest: %v", binErr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	path, err := memory.DigestRepo(ctx, *repo, claudeBin)
	if err != nil {
		return helptext.Fail("memory digest: %v", err)
	}
	fmt.Println("wrote " + path)
	return 0
}

func cmdMemoryWatch(args []string) int {
	fs := helptext.NewFlagSet("memory watch", helptext.Page{Usage: "claude-code memory watch [flags]",
		About: "Detached per-session watcher the Stop gate spawns; updates the bank once the session goes idle."})
	session := fs.String("session", "", "session id (names the stamp)")
	tr := fs.String("transcript", "", "the session's transcript path")
	cwd := fs.String("cwd", "", "project root whose bank to update")
	idle := fs.Duration("idle", memory.UpdateIdleAfter, "quiet period before the final update")
	owner := fs.Int("owner", 0, "session process pid; the watcher exits once it is gone (0 = unbounded)")
	if code, done := memoryParseNoArgs(fs, "watch", args); done {
		return code
	}
	return memory.IdleUpdateWatch(memory.WatchOptions{Session: *session, Transcript: *tr, CWD: *cwd,
		Idle: *idle, Owner: *owner, Stderr: os.Stderr})
}

func cmdMemoryCatchup(args []string) int {
	fs := helptext.NewFlagSet("memory catchup", helptext.Page{Usage: "claude-code memory catchup [flags]",
		About: "Session-start sweep: saves finished sessions whose memory update was missed."})
	dry := fs.Bool("dry-run", false, "list the sessions it would save; write nothing")
	skip := fs.String("skip", "", "session id to leave alone (the one that started the sweep)")
	if code, done := memoryParseNoArgs(fs, "catchup", args); done {
		return code
	}
	return memory.CatchUpSweep(memory.SweepOptions{DryRun: *dry, Skip: *skip, Stdout: os.Stdout, Stderr: os.Stderr})
}

// memorySubcommands dispatches both public verbs and the off-menu plumbing verbs.
func memorySubcommands() map[string]func([]string) int {
	return map[string]func([]string) int{
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
		"report":   cmdMemoryReport,
		"digest":   cmdMemoryDigest,
		"fmt":      cmdMemoryFmt,
		"watch":    cmdMemoryWatch,
		"catchup":  cmdMemoryCatchup,
		"timeline": cmdMemoryTimeline,
		"alias":    cmdMemoryAlias,
		"import":   cmdMemoryImport,
	}
}

// cmdMemory dispatches the memory verb tree.
func cmdMemory(args []string) int {
	if len(args) == 0 || isHelpVerb(args[0]) {
		memoryPage.Fprint(os.Stderr)
		return 0
	}
	sub := args[0]
	h, ok := memorySubcommands()[sub]
	if !ok {
		return helptext.Fail("memory: unknown subcommand %q (claude-code memory -h lists them)", sub)
	}
	return h(args[1:])
}

// memorySpec describes memory subcommands and their completion domains.
func memorySpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"recall": {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.BoolFlag("all"), helptext.Plain("k"),
			helptext.Plain("load"), helptext.BoolFlag("explain"), helptext.CompletionFormatFlag()}},
		"remember": {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.ValuedFlag("type", memoryRememberTypes)}},
		"init":     {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys)}},
		"review": {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.Plain("stale-days"), helptext.BoolFlag("notice"),
			helptext.BoolFlag("brief"), helptext.BoolFlag("candidates")}},
		"promote": {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys),
			helptext.ValuedFlag("to", func() []string { return memory.PromoteTargets }), helptext.BoolFlag("dry-run")}},
		"update": {Flags: []helptext.Flag{helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("session"),
			helptext.BoolFlag("dry-run"), helptext.BoolFlag("historical")}},
		"dream":    {Flags: []helptext.Flag{helptext.Plain("cwd"), helptext.BoolFlag("force"), helptext.BoolFlag("dry-run")}},
		"search":   {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.BoolFlag("all"), helptext.Plain("k"), helptext.CompletionFormatFlag()}},
		"show":     {},
		"digest":   {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys)}},
		"index":    {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.BoolFlag("reembed"), helptext.BoolFlag("mint-only")}},
		"path":     {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys)}},
		"report":   {Flags: []helptext.Flag{helptext.Plain("label"), helptext.Plain("file"), helptext.BoolFlag("stdin"), helptext.CompletionFormatFlag(), helptext.ValuedFlag("repo", memory.RepoKeys)}},
		"fmt":      {Flags: []helptext.Flag{helptext.BoolFlag("check")}},
		"catchup":  {Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.Plain("skip")}},
		"watch":    {Flags: []helptext.Flag{helptext.Plain("session"), helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("owner"), helptext.Plain("idle")}},
		"timeline": {Flags: []helptext.Flag{helptext.ValuedFlag("repo", memory.RepoKeys), helptext.Plain("n")}},
		"alias":    {Flags: []helptext.Flag{helptext.Plain("repo"), helptext.ValuedFlag("into", memory.RepoKeys), helptext.BoolFlag("dry-run")}},
		"import": {Flags: []helptext.Flag{helptext.Plain("from"), helptext.Plain("host"), helptext.ValuedFlag("repo", memory.RepoKeys),
			helptext.BoolFlag("all"), helptext.BoolFlag("dry-run"), helptext.BoolFlag("topics")}},
	}}
}

// memoryRememberTypes lists memory.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return slices.Sorted(maps.Keys(memory.RememberTypes)) }

func cmdMemoryInit(args []string) int {
	fs, repo := memoryRepoFlagSet("init")
	if code, done := memoryParseNoArgs(fs, "init", args); done {
		return code
	}
	root, err := memory.ResolveRepo(*repo)
	if err != nil {
		return helptext.FailUsage("memory init: %v", err)
	}
	if err := memory.InitBank(os.Stdout, root); err != nil {
		return helptext.Fail("memory init: %v", err)
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
		return helptext.Fail("memory remember: no fact text (usage: %s)", memorySubPage("remember").Usage)
	}
	root, err := memory.ResolveRepo(*repo)
	if err != nil {
		return helptext.FailUsage("memory remember: %v", err)
	}
	id, err := memory.Remember(root, *kind, strings.Join(terms, " "), time.Now())
	if err != nil {
		return helptext.Fail("memory remember: %v", err)
	}
	fmt.Printf("remembered [%s] -> %s\n", id, memory.Dir(root))
	return 0
}

// memoryQuery is the flags recall and search share: --repo, -k, --all and --format, plus the query terms.
type memoryQuery struct {
	fs         *flag.FlagSet
	repo       *string
	k          *int
	all        *bool
	format     *string
	sub, terms string
	termCount  int
}

func newMemoryQuery(sub string, k int) *memoryQuery {
	fs, repo := memoryRepoFlagSet(sub)
	return &memoryQuery{fs: fs, repo: repo, sub: sub, k: fs.Int("k", k, "maximum hits"),
		all: fs.Bool("all", false, "search the home tier and every repo store"), format: helptext.FormatFlag(fs)}
}

// parse reads args into q; done means the verb returns code (help, a usage error or no query).
func (q *memoryQuery) parse(args []string) (code int, done bool) {
	terms, err := helptext.ParseArgs(q.fs, args)
	if err != nil {
		return helptext.ExitCode(err), true
	}
	if err := helptext.CheckFormat(*q.format); err != nil {
		return helptext.BadFormat("memory "+q.sub, err), true
	}
	if len(terms) == 0 {
		return helptext.Fail("memory %s: no query (usage: %s)", q.sub, memorySubPage(q.sub).Usage), true
	}
	q.terms, q.termCount = strings.Join(terms, " "), len(terms)
	return 0, false
}

func (q *memoryQuery) json() bool { return *q.format == helptext.JSON }

func cmdMemorySearch(args []string) int {
	q := newMemoryQuery("search", 8)
	if code, done := q.parse(args); done {
		return code
	}
	res, err := memory.Search(memory.SearchOptions{Repo: *q.repo, All: *q.all, Query: q.terms, K: *q.k,
		Expand: q.termCount == 1 && !q.json()})
	if err != nil {
		return helptext.FailUsage("memory search: %v", err)
	}
	if res.Block != "" {
		printMemoryBlock(res.Path, res.Block)
		return 0
	}
	if q.json() {
		return helptext.WriteJSONOrFail("memory search", res.Hits)
	}
	printMemorySearchHits(res.Hits)
	return 0
}

func printMemorySearchHits(hits []memory.SearchHit) {
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
}

func cmdMemoryShow(args []string) int {
	fs := memoryFlagSet("show")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 1 {
		return helptext.FailUsage("memory show: give one block id (from `memory search` or a MEMORY.md region)")
	}
	path, block, err := memory.Show(fs.Arg(0))
	if err != nil {
		return helptext.Fail("memory show %s: %v", fs.Arg(0), err)
	}
	printMemoryBlock(path, block)
	return 0
}

// printMemoryBlock prints one block under its source path, as search's id expansion and show both do.
func printMemoryBlock(path, block string) {
	fmt.Printf("<!-- %s -->\n%s\n", path, strings.TrimSpace(block))
}

func cmdMemoryIndex(args []string) int {
	fs, repo := memoryRepoFlagSet("index")
	reembed := fs.Bool("reembed", false, "re-embed all entries; last 8 min of the lease or expired")
	mintOnly := fs.Bool("mint-only", false, "refresh embedding cache without settling bank contents")
	if code, done := memoryParseNoArgs(fs, "index", args); done {
		return code
	}
	if err := memory.Index(os.Stdout, os.Stderr, memory.IndexOptions{Repo: *repo, Reembed: *reembed, MintOnly: *mintOnly}); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

func cmdMemoryPath(args []string) int {
	fs, repo := memoryRepoFlagSet("path")
	if code, done := memoryParseNoArgs(fs, "path", args); done {
		return code
	}
	root, err := memory.ResolveRepo(*repo)
	if err != nil {
		return helptext.FailUsage("memory path: %v", err)
	}
	fmt.Println(memory.Dir(root))
	return 0
}

// cmdMemoryReport appends a JSON report and its decisions to the repo's bank reports/ and prints both paths.
func cmdMemoryReport(args []string) int {
	fs, repo := memoryRepoFlagSet("report")
	label := fs.String("label", "", "report name: reports/<date>-<label>.md (sanitized to [A-Za-z0-9._-])")
	file := fs.String("file", "", "JSON input file: {\"report\": markdown, \"decisions\": [...]}")
	stdin := fs.Bool("stdin", false, "read the JSON input from stdin")
	format := helptext.FormatFlag(fs)
	if code, done := memoryParseNoArgs(fs, "report", args); done {
		return code
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("memory report", err)
	}
	if (*file == "") == !*stdin {
		return helptext.FailUsage("memory report: give exactly one of --file or --stdin")
	}
	in, err := memory.ReadReportInput(*file, *stdin)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	out, err := memory.ReportRepo(*repo, *label, in)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	return printReportPaths(out, *format)
}

// printReportPaths prints the written paths one per line, or as {"paths": [...]} with --format json.
func printReportPaths(out []string, format string) int {
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("memory report", map[string][]string{"paths": out})
	}
	for _, p := range out {
		if _, err := fmt.Fprintln(os.Stdout, p); err != nil {
			return helptext.Fail("memory report: %v", err)
		}
	}
	return 0
}

func cmdMemoryFmt(args []string) int {
	fs := memoryFlagSet("fmt")
	check := fs.Bool("check", false, "report would-change files; write nothing")
	if code, done := memoryParseNoArgs(fs, "fmt", args); done {
		return code
	}
	dirty, err := memory.FormatReport(os.Stdout, *check)
	if err != nil {
		return helptext.Fail("memory fmt: %v", err)
	}
	if dirty {
		return 1
	}
	return 0
}

// cmdMemoryReview runs the doctor and stats engine calls and prints both output blocks.
func cmdMemoryReview(args []string) int {
	fs, repo := memoryRepoFlagSet("review")
	staleDays := fs.Int("stale-days", memory.DefaultStaleDays, "flag a bank untouched for this many days")
	brief := fs.Bool("brief", false, "print only the problem count, not each line")
	notice := fs.Bool("notice", false, "refresh the cached session-start notice and print nothing")
	candidates := fs.Bool("candidates", false, "also print the promotion-ranking procedure")
	if code, done := memoryParseNoArgs(fs, "review", args); done {
		return code
	}
	err := memory.ReviewReport(os.Stdout, memory.ReviewOptions{Repo: *repo, StaleDays: *staleDays, Brief: *brief,
		Notice: *notice, Candidates: *candidates})
	if err != nil {
		return helptext.Fail("memory review: %v", err)
	}
	return 0
}

func cmdMemoryTimeline(args []string) int {
	fs, repo := memoryRepoFlagSet("timeline")
	n := fs.Int("n", 20, "max entries")
	if code, done := memoryParseNoArgs(fs, "timeline", args); done {
		return code
	}
	if err := memory.TimelineReport(os.Stdout, *repo, *n); err != nil {
		return helptext.Fail("memory timeline: %v", err)
	}
	return 0
}

// cmdMemoryPromote moves a block to home, or prints a block and target for a human to apply (never writes them).
func cmdMemoryPromote(args []string) int {
	fs, repo := memoryRepoFlagSet("promote")
	to := fs.String("to", "", "promotion target: home, rules or skill (claude-md: dream only)")
	dryRun := fs.Bool("dry-run", false, "print the move without writing")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || *to == "" {
		return helptext.FailUsage("memory promote: usage: %s", memorySubPage("promote").Usage)
	}
	root, err := memory.ResolveRepo(*repo)
	if err != nil {
		return helptext.FailUsage("memory promote: %v", err)
	}
	out, err := memory.Promote(root, pos[0], *to, *dryRun, time.Now())
	if err != nil {
		return helptext.Fail("memory promote: %v", err)
	}
	fmt.Print(out)
	return 0
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
		return helptext.Fail("memory alias: --repo and --into are required (usage: %s)", memoryAliasPage.Usage)
	}
	res, err := memory.AliasRepo(memory.AliasOptions{Repo: *repo, Into: *into, DryRun: *dryRun, Now: time.Now()})
	if err != nil {
		return helptext.Fail("memory alias: %v", err)
	}
	printAliasPlan(*dryRun, res.Lines)
	return 0
}

func printAliasPlan(dryRun bool, lines []string) {
	verb := "applied"
	if dryRun {
		verb = "plan (dry run, nothing written)"
	}
	fmt.Println(verb + ":")
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}

var memoryImportPage = helptext.Page{
	Usage: "claude-code memory import (--from DIR | --host HOST) [flags]",
	About: "Merge another bank's logs (a memory root or one store) into this one; nothing\n" +
		"local is overwritten, the source is never changed. --topics adds the other files.\n" +
		"git -C <memory> log is the undo.",
}

// cmdMemoryImport is `memory import`: --dry-run prints the plan and writes nothing.
func cmdMemoryImport(args []string) int {
	fs := helptext.NewFlagSet("memory import", memoryImportPage)
	from := fs.String("from", "", "memory root or store dir to merge in")
	host := fs.String("host", "", "tailnet host whose memory root to fetch over ssh")
	repo := fs.String("repo", "", "store key or checkout to import into (default: this repo)")
	all := fs.Bool("all", false, "every store present on both sides, home included")
	dryRun := fs.Bool("dry-run", false, "print the plan; write nothing")
	topics := fs.Bool("topics", false, "also topic files, backups and MEMORY.md pointers")
	if code, done := memoryParseNoArgs(fs, "import", args); done {
		return code
	}
	if (*from == "") == (*host == "") {
		return helptext.FailUsage("memory import: give exactly one of --from or --host (usage: %s)", memoryImportPage.Usage)
	}
	dir, label, err := memoryImportSource(*from, *host)
	if err != nil {
		return helptext.Fail("memory import: %v", err)
	}
	if *host != "" {
		defer func() { _ = os.RemoveAll(dir) }() // the fetched copy
	}
	res, err := memory.ImportBank(memory.ImportOptions{From: dir, Repo: *repo, Label: label, All: *all, DryRun: *dryRun, Topics: *topics})
	if err != nil {
		return memoryImportFail(err)
	}
	fmt.Println(strings.Join(append(res.Lines, res.Summary), "\n"))
	return 0
}

// memoryImportFail is exit 2 for a bad --from or flag combination, else exit 1.
func memoryImportFail(err error) int {
	if _, bad := errors.AsType[*memory.ImportInputError](err); bad {
		return helptext.FailUsage("memory import: %v", err)
	}
	return helptext.Fail("memory import: %v", err)
}

// memoryImportSource is the dir to import from and its commit label: --from as given, else the --host fetch.
func memoryImportSource(from, host string) (dir, label string, err error) {
	if host == "" {
		return from, from, nil
	}
	if dir, err = fetchMemoryRoot(host); err != nil {
		_ = os.RemoveAll(dir) // a failed fetch leaves no copy behind
		err = fmt.Errorf("fetch from %s: %w", host, err)
	}
	return dir, host, err
}

// fetchMemoryRoot streams host's memory root as a tar into a fresh private temp dir; the caller removes it.
func fetchMemoryRoot(host string) (string, error) {
	script := `cd "${CLAUDE_CONFIG_DIR:-$HOME/.claude}/memory" && COPYFILE_DISABLE=1 tar -cf - --exclude=.git --exclude='*.lock' --exclude=.postupdate .`
	argv, err := remote.StreamArgs(host, script)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "memory-import-")
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return dir, err
	}
	if err := cmd.Start(); err != nil {
		return dir, err
	}
	skipped, xerr := memory.ExtractStoreTar(out, dir)
	if xerr != nil {
		_ = cmd.Process.Kill()
	}
	werr := cmd.Wait()
	for _, s := range skipped {
		fmt.Println("fetch " + s)
	}
	if werr != nil {
		return dir, werr
	}
	return dir, xerr
}

// memoryRecallExplain prints the hook's recall selection; --explain refuses the flags that need real hits.
func memoryRecallExplain(repo, query string, conflict bool) int {
	if conflict {
		return helptext.Fail("memory recall: --explain does not support --all, --format json, or --load")
	}
	if err := memory.RecallExplain(os.Stdout, repo, query); err != nil {
		return helptext.FailUsage("memory recall: %v", err)
	}
	return 0
}

func cmdMemoryRecall(args []string) int {
	q := newMemoryQuery("recall", 3)
	load := q.fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	explain := q.fs.Bool("explain", false, "explain hook recall (not with --all, --load or json)")
	if code, done := q.parse(args); done {
		return code
	}
	if *explain {
		return memoryRecallExplain(*q.repo, q.terms, *q.all || q.json() || *load != 0)
	}
	hits, _, err := memory.SearchRepo(*q.repo, *q.all, q.terms, *q.k)
	if err != nil {
		return helptext.FailUsage("memory recall: %v", err)
	}
	return memoryRecallEmit(hits, *q.format, *load)
}

// memoryRecallEmit writes hits as JSON, or as the text report with an optional body load.
func memoryRecallEmit(hits []memory.SearchHit, format string, load int) int {
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("memory recall", hits)
	}
	memory.RecallReport(os.Stdout, os.Stderr, hits, load)
	return 0
}
