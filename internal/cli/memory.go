package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/network/remote"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// memorySub is one sub-verb's synopsis args and blurb; public ones are the memory page's COMMANDS rows.
type memorySub struct{ name, args, about string }

var memorySubs = []memorySub{
	{"recall", "<terms>", "rank bank blocks for terms; --load appends the top bodies"},
	{"remember", "<fact>", "append a fact to the bank (--type picks the file)"},
	{"init", "", "create the bank"},
	{"review", "", "doctor + bank stats; --timeline lists recent entries"},
	{"promote", "<id> --to <target>", "print one block and its target for rules|skill|claude-md"},
	{"update", "", "save the live session; --catchup saves missed sessions"},
	{"dream", "", "consolidate the bank when due (--force, --dry-run)"},
	{"index", "", "settle the bank, rewrite MEMORY.md and index.md, file loose notes"},
	{"migrate", "[--dry-run]", "merge old per-repo bank files into the one bank (--dry-run previews)"},
}

var memoryPage = helptext.Page{
	Usage: "claude-code memory <command> [flags] [args]",
	About: "One memory bank for every workspace, organised in folders: session-start\n" +
		"injects it, the Stop/SessionEnd update writes it. Recall and save facts, then\n" +
		"promote the ones that hold up.",
	Commands: memoryCommandRows(),
	Hint: "plumbing: search, show, path, report, fmt, watch,\n" +
		"          import, sandbox (off menu)\n" +
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
	"search":  {"<terms>", "rank blocks for terms, with scores"},
	"show":    {"<id>", "print one block (id from memory search or a MEMORY.md region)"},
	"path":    {"[SUB] [NAME]", "print the store root or a SUB path: handoffs|reports|knowledge|worktrees"},
	"report":  {"--label L [--file F | --stdin]", "append a report and its decisions to reports/"},
	"fmt":     {"", "wrap the bank files"},
	"sandbox": {"", "create a disposable memory config"},
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
	if strings.HasPrefix(args, "<id> --to") || strings.HasPrefix(args, "<old-id> --with") {
		return "claude-code memory " + sub + " " + args + " [flags]"
	}
	return strings.TrimSpace("claude-code memory " + sub + " [flags] " + args)
}

// memoryFlagSet builds the per-subcommand flag.FlagSet shared by every handler below.
func memoryFlagSet(sub string) *flag.FlagSet {
	return helptext.NewFlagSet("memory "+sub, memorySubPage(sub))
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
	dryRun := fs.Bool("dry-run", false, "preview the session or catch-up candidates; update nothing")
	catchup := fs.Bool("catchup", false, "save finished sessions whose memory update was missed")
	skip := fs.String("skip", "", "catch-up session id to leave alone")
	historical := fs.Bool("historical", false, "older session: history and decisions only")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory update: unexpected argument %q; name a session with --session <id>", fs.Arg(0))
	}
	if *catchup {
		return memory.CatchUpSweep(memory.SweepOptions{DryRun: *dryRun, Skip: *skip, Stdout: os.Stdout, Stderr: os.Stderr})
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
		"search":  cmdMemorySearch,
		"show":    cmdMemoryShow,
		"index":   cmdMemoryIndex,
		"path":    cmdMemoryPath,
		"report":  cmdMemoryReport,
		"fmt":     cmdMemoryFmt,
		"watch":   cmdMemoryWatch,
		"import":  cmdMemoryImport,
		"sandbox": cmdMemorySandbox,
		"migrate": cmdMemoryMigrate,
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
		"recall": {Flags: []helptext.Flag{helptext.Plain("k"),
			helptext.Plain("load"), helptext.BoolFlag("explain"), helptext.CompletionFormatFlag()}},
		"remember": {Flags: []helptext.Flag{helptext.ValuedFlag("type", memoryRememberTypes)}},
		"init":     {},
		"review": {Flags: []helptext.Flag{helptext.Plain("stale-days"), helptext.BoolFlag("notice"),
			helptext.BoolFlag("brief"), helptext.BoolFlag("candidates"), helptext.BoolFlag("timeline"), helptext.Plain("n")}},
		"promote": {Flags: []helptext.Flag{
			helptext.ValuedFlag("to", func() []string { return memory.PromoteTargets })}},
		"update": {Flags: []helptext.Flag{helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("session"),
			helptext.BoolFlag("dry-run"), helptext.BoolFlag("historical"), helptext.BoolFlag("catchup"), helptext.Plain("skip")}},
		"dream":   {Flags: []helptext.Flag{helptext.Plain("cwd"), helptext.BoolFlag("force"), helptext.BoolFlag("dry-run")}},
		"search":  {Flags: []helptext.Flag{helptext.Plain("k"), helptext.CompletionFormatFlag()}},
		"show":    {},
		"index":   {Flags: []helptext.Flag{helptext.BoolFlag("reembed"), helptext.BoolFlag("mint-only")}},
		"path":    {},
		"report":  {Flags: []helptext.Flag{helptext.Plain("label"), helptext.Plain("file"), helptext.BoolFlag("stdin"), helptext.CompletionFormatFlag()}},
		"fmt":     {Flags: []helptext.Flag{helptext.BoolFlag("check"), helptext.BoolFlag("own")}},
		"watch":   {Flags: []helptext.Flag{helptext.Plain("session"), helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("owner"), helptext.Plain("idle")}},
		"sandbox": {Flags: []helptext.Flag{helptext.BoolFlag("empty"), helptext.Plain("format")}},
		"migrate": {Flags: []helptext.Flag{helptext.BoolFlag("dry-run")}},
		"import": {Flags: []helptext.Flag{helptext.Plain("from"), helptext.Plain("host"),
			helptext.BoolFlag("dry-run"), helptext.BoolFlag("topics")}},
	}}
}

// memoryRememberTypes lists memory.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return slices.Sorted(maps.Keys(memory.RememberTypes)) }

func cmdMemoryInit(args []string) int {
	fs := memoryFlagSet("init")
	if code, done := memoryParseNoArgs(fs, "init", args); done {
		return code
	}
	if err := memory.InitBank(os.Stdout); err != nil {
		return helptext.Fail("memory init: %v", err)
	}
	return 0
}

func cmdMemoryRemember(args []string) int {
	fs := memoryFlagSet("remember")
	kind := fs.String("type", "feedback", "project->decisionLog.md; user/feedback/reference->conventions.md; "+
		"reads like a rule? use memory promote --to rules instead")
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(terms) == 0 {
		return helptext.Fail("memory remember: no fact text (usage: %s)", memorySubPage("remember").Usage)
	}
	id, err := memory.Remember(*kind, strings.Join(terms, " "), time.Now())
	if err != nil {
		return helptext.Fail("memory remember: %v", err)
	}
	fmt.Printf("remembered [%s] -> %s\n", id, memory.HomeBank().SubDir(""))
	return 0
}

// memoryQuery is the flags recall and search share: -k and --format, plus the query terms.
type memoryQuery struct {
	fs         *flag.FlagSet
	k          *int
	format     *string
	sub, terms string
	termCount  int
}

func newMemoryQuery(sub string, k int) *memoryQuery {
	fs := memoryFlagSet(sub)
	return &memoryQuery{fs: fs, sub: sub, k: fs.Int("k", k, "maximum hits"), format: helptext.FormatFlag(fs)}
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
	res := memory.Search(memory.SearchOptions{Query: q.terms, K: *q.k, Expand: q.termCount == 1 && !q.json()})
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
		if by := h.ReplacedBy(); by != "" {
			id += " replaced by " + by
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
	fs := memoryFlagSet("index")
	reembed := fs.Bool("reembed", false, "re-embed all entries; last 8 min of the lease or expired")
	mintOnly := fs.Bool("mint-only", false, "refresh embedding cache without settling bank contents")
	if code, done := memoryParseNoArgs(fs, "index", args); done {
		return code
	}
	if err := memory.Index(os.Stdout, os.Stderr, memory.IndexOptions{Reembed: *reembed, MintOnly: *mintOnly}); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

// memoryPathSubs are the sub dirs `memory path` resolves; SUB NAME names one file in the bank's.
var memoryPathSubs = []string{"handoffs", "reports", "knowledge", "worktrees"}

func cmdMemoryPath(args []string) int {
	fs := memoryFlagSet("path")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) > 2 {
		return helptext.FailUsage("memory path: unexpected argument %q", pos[2])
	}
	if len(pos) > 0 && !slices.Contains(memoryPathSubs, pos[0]) {
		return helptext.FailUsage("memory path: unexpected argument %q (SUB is one of %s)", pos[0], strings.Join(memoryPathSubs, ", "))
	}
	b := memory.HomeBank()
	switch len(pos) {
	case 0:
		fmt.Println(b.SubDir(""))
	case 1:
		fmt.Println(b.SubDir(pos[0]))
	default:
		fmt.Println(b.Sub(pos[0], memoryPathName(pos[0], pos[1])))
	}
	return 0
}

// memoryPathName is the store file name for NAME in sub: date-first for dated subs, prefix-free, .md by default.
func memoryPathName(sub, name string) string {
	if sub == "knowledge" {
		name, _ = memory.StripRepoPrefix(name)
	} else if sub != "" {
		name = memory.DatedName(name, time.Now())
	}
	if filepath.Ext(name) == "" {
		name += ".md"
	}
	return name
}

// cmdMemoryReport appends a JSON report and its decisions to the bank's reports/ and prints both paths.
func cmdMemoryReport(args []string) int {
	fs := memoryFlagSet("report")
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
	out, err := memory.Report(*label, in)
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
	own := fs.Bool("own", false, "wrap only the harness and home banks; run from the live config dir")
	if code, done := memoryParseNoArgs(fs, "fmt", args); done {
		return code
	}
	var dirty bool
	var err error
	if *own && *check {
		return helptext.FailUsage("memory fmt: --check and --own cannot combine")
	}
	if *own {
		if cwd, _ := os.Getwd(); !paths.SamePath(cwd, paths.ConfigDir()) {
			return helptext.Fail("memory fmt --own: %s is not the live config dir (%s)", cwd, paths.ConfigDir())
		}
		if err = memory.FormatOwn(os.Stdout); err != nil {
			return helptext.Fail("memory fmt: %v", err)
		}
		return 0
	}
	if *check {
		dirty, err = memory.FormatCheck(os.Stdout)
	} else {
		dirty, err = memory.FormatReport(os.Stdout, false)
	}
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
	fs := memoryFlagSet("review")
	staleDays := fs.Int("stale-days", memory.DefaultStaleDays, "flag a bank untouched for this many days")
	brief := fs.Bool("brief", false, "print only the problem count, not each line")
	notice := fs.Bool("notice", false, "refresh the cached session-start notice and print nothing")
	candidates := fs.Bool("candidates", false, "also print the promotion-ranking procedure")
	timeline := fs.Bool("timeline", false, "print recent bank entries and sessionHistory")
	n := fs.Int("n", 20, "max timeline entries")
	if code, done := memoryParseNoArgs(fs, "review", args); done {
		return code
	}
	if *timeline {
		if err := memory.TimelineReport(os.Stdout, *n); err != nil {
			return helptext.Fail("memory review: %v", err)
		}
		return 0
	}
	err := memory.ReviewReport(os.Stdout, memory.ReviewOptions{StaleDays: *staleDays, Brief: *brief,
		Notice: *notice, Candidates: *candidates})
	if err != nil {
		return helptext.Fail("memory review: %v", err)
	}
	return 0
}

// cmdMemoryPromote prints a block and its target for a human to apply (never writes them).
func cmdMemoryPromote(args []string) int {
	fs := memoryFlagSet("promote")
	to := fs.String("to", "", "promotion target: rules or skill (claude-md: dream only)")
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || *to == "" {
		return helptext.FailUsage("memory promote: usage: %s", memorySubPage("promote").Usage)
	}
	out, err := memory.Promote(pos[0], *to)
	if err != nil {
		return helptext.Fail("memory promote: %v", err)
	}
	fmt.Print(out)
	return 0
}

var memoryImportPage = helptext.Page{
	Usage: "claude-code memory import (--from DIR | --host HOST) [flags]",
	About: "Merge another bank's logs (a memory root or one store) into this one; nothing\n" +
		"local is overwritten, the source is never changed. --topics adds the other files.\n" +
		"git -C <memory> log is the undo.",
}

// cmdMemoryMigrate is `memory migrate`: --dry-run lists the plan and blockers and writes nothing.
func cmdMemoryMigrate(args []string) int {
	fs := memoryFlagSet("migrate")
	dryRun := fs.Bool("dry-run", false, "print the plan and blockers; write nothing")
	if code, done := memoryParseNoArgs(fs, "migrate", args); done {
		return code
	}
	if err := memory.Migrate(memory.MigrateOptions{DryRun: *dryRun, Out: os.Stdout}); err != nil {
		return helptext.Fail("memory migrate: %v", err)
	}
	if *dryRun {
		return 0
	}
	if err := memory.Index(os.Stdout, os.Stderr, memory.IndexOptions{}); err != nil { // writes the layout into index.md
		return helptext.Fail("memory migrate: index: %v", err)
	}
	return 0
}

// cmdMemoryImport is `memory import`: --dry-run prints the plan and writes nothing.
func cmdMemoryImport(args []string) int {
	fs := helptext.NewFlagSet("memory import", memoryImportPage)
	from := fs.String("from", "", "memory root or store dir to merge in")
	host := fs.String("host", "", "tailnet host whose memory root to fetch over ssh")
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
		defer func() { _ = os.RemoveAll(dir) }()
	}
	res, err := memory.ImportBank(memory.ImportOptions{From: dir, Label: label, DryRun: *dryRun, Topics: *topics})
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
		_ = os.RemoveAll(dir)
		err = fmt.Errorf("fetch from %s: %w", host, err)
	}
	return dir, host, err
}

// memoryFetchTimeout bounds the whole `memory import --host` transfer; a store is tens of MB, so this is generous.
const memoryFetchTimeout = 10 * time.Minute

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
	ctx, cancel := context.WithTimeout(context.Background(), memoryFetchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.OwnGroup(cmd) // the deadline kills ssh's whole group, so a hung host cannot block the import
	cmd.WaitDelay = proc.ExecWaitDelay
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
func memoryRecallExplain(query string, conflict bool) int {
	if conflict {
		return helptext.Fail("memory recall: --explain does not support --format json or --load")
	}
	memory.RecallExplain(os.Stdout, query)
	return 0
}

func cmdMemoryRecall(args []string) int {
	q := newMemoryQuery("recall", 3)
	load := q.fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	explain := q.fs.Bool("explain", false, "explain hook recall (not with --load or json)")
	if code, done := q.parse(args); done {
		return code
	}
	if *explain {
		return memoryRecallExplain(q.terms, q.json() || *load != 0)
	}
	return memoryRecallEmit(memory.RecallBank(q.terms, *q.k), *q.format, *load)
}

// memoryRecallEmit writes hits as JSON, or as the text report with an optional body load.
func memoryRecallEmit(hits []memory.SearchHit, format string, load int) int {
	if format == helptext.JSON {
		return helptext.WriteJSONOrFail("memory recall", hits)
	}
	memory.RecallReport(os.Stdout, os.Stderr, hits, load)
	return 0
}

func cmdMemorySandbox(args []string) int {
	fs := memoryFlagSet("sandbox")
	empty := fs.Bool("empty", false, "seed empty stores instead of copying content")
	format := fs.String("format", "text", "output format: text|json")
	if code, done := memoryParseNoArgs(fs, "sandbox", args); done {
		return code
	}
	if *format != "text" && *format != "json" {
		return helptext.FailUsage("memory sandbox: format must be text or json")
	}
	res, err := memory.Sandbox(memory.SandboxOptions{Empty: *empty})
	if err != nil {
		if _, ok := errors.AsType[*memory.ImportInputError](err); ok {
			return helptext.FailUsage("memory sandbox: %v", err)
		}
		return helptext.Fail("memory sandbox: %v", err)
	}
	return printMemorySandbox(res, *format)
}

func printMemorySandbox(res memory.SandboxResult, format string) int {
	fmt.Fprintln(os.Stderr, "memory sandbox: vault holds origin metadata only; embeddings off (no profiles or cache)")
	if format == "json" {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			return helptext.Fail("memory sandbox: %v", err)
		}
		return 0
	}
	var stores []string
	for _, s := range res.Stores {
		stores = append(stores, fmt.Sprintf("%s (%d files)", s.Key, s.Files))
	}
	_, err := fmt.Fprintf(os.Stdout, "sandbox  %s\nexport   CLAUDE_CONFIG_DIR=%s\nstores   %s\nremove with: rm -rf %s\n",
		res.Dir, res.Dir, strings.Join(stores, ", "), res.Dir)
	if err != nil {
		return helptext.Fail("memory sandbox: %v", err)
	}
	return 0
}
