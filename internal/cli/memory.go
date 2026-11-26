package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/shadow"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// memorySub is one sub-verb's synopsis args and blurb; public ones are the memory page's COMMANDS rows.
type memorySub struct{ name, args, about string }

var memorySubs = []memorySub{
	{"search", "<terms>", "rank bank blocks for terms; --open N appends bodies, --id one block"},
	{"remember", "<fact>", "append a fact to the bank (--type picks the file)"},
	{"init", "", "create the bank"},
	{"review", "", "doctor + bank stats + hook signals; --timeline lists entries"},
	{"promote", "<id> --to <target>", "print one block and its target for rules|skill|claude-md"},
	{"update", "", "save the live session; --catchup saves missed sessions"},
	{"dream", "", "consolidate the bank when due (--force, --dry-run)"},
	{"index", "", "settle the bank, regenerate MEMORY.md, append to index.md"},
}

var memoryPage = helptext.Page{
	Usage: "claude-code memory <command> [flags] [args]",
	About: "One memory bank for every workspace, organised in folders: session-start\n" +
		"injects it, the Stop/SessionEnd update writes it. Recall and save facts, then\n" +
		"promote the ones that hold up.",
	Commands: memoryCommandRows(),
	Hint: "plumbing: path, report, fmt, watch,\n" +
		"          import, sandbox, upgrade, extract, shadow, store (off menu)\n" +
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
	"path":    {"[SUB] [NAME]", "print the store root or a SUB path: handoffs|reports|knowledge|worktrees"},
	"report":  {"--label L [--file F | --stdin]", "write a report to a new reports/<date>-<label>[-N].md and append its decisions"},
	"fmt":     {"", "wrap the bank files"},
	"sandbox": {"", "create a disposable memory config"},
	"upgrade": {"[--dry-run] [--to N] [--model M] [--no-resolve]", "bring every memory file to the current store version, one commit per step"},
	"extract": {"", "classify files (--dry-run) or purge proven-dead files (--purge); no model"},
	"shadow":  {"--rebuild", "rebuild state/memory.db from the Markdown store and check every render is byte-equal"},
	"store":   {"[--truth files|db]", "show which store is the source of truth, or switch it"},
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
	catchup := fs.Bool("catchup", false, "save missed sessions, then run queued memory jobs")
	skip := fs.String("skip", "", "catch-up session id to leave alone")
	historical := fs.Bool("historical", false, "older session: history and decisions only")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory update: unexpected argument %q; name a session with --session <id>", fs.Arg(0))
	}
	if *catchup {
		if *dryRun {
			return memory.CatchUpSweep(memory.SweepOptions{DryRun: true, Skip: *skip, Stdout: os.Stdout, Stderr: os.Stderr})
		}
		now := time.Now()
		dailyLearn(now, os.Stderr)
		code := memory.CatchUpSweep(memory.SweepOptions{Skip: *skip, Stdout: os.Stdout, Stderr: os.Stderr})
		if memory.SyncIfDue(now, now, *cwd) {
			fmt.Fprintln(os.Stderr, "memory sync: started")
		}
		return code
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
	trigger := fs.String("trigger", "manual", "what started the run: session-start|schedule|manual")
	if code, done := memoryParseNoArgs(fs, "dream", args); done {
		return code
	}
	switch *trigger {
	case memory.DreamTriggerManual, memory.DreamTriggerSessionStart, memory.DreamTriggerSchedule:
	default:
		return helptext.FailUsage("memory dream: --trigger must be session-start, schedule or manual")
	}
	if err := memory.RunDream(memory.DreamRunOptions{CWD: *cwd, Force: *force, DryRun: *dryRun, Run: ask.MemoryRunner(), Out: os.Stdout, Trigger: *trigger}); err != nil {
		return helptext.Fail("memory dream: %v", err)
	}
	return 0
}

// cmdMemoryUpgrade exits 4 when a file was left untouched.
func cmdMemoryUpgrade(args []string) int {
	fs := memoryFlagSet("upgrade")
	dryRun := fs.Bool("dry-run", false, "print the per-file summary; write nothing")
	to := fs.Int("to", 0, "stop at this store version (default: the version this binary writes)")
	noResolve := fs.Bool("no-resolve", false, "leave residue listed; skip the one code-mode agent")
	model := fs.String("model", "", "residue agent model: opus (default), astra or a catalog id")
	if code, done := memoryParseNoArgs(fs, "upgrade", args); done {
		return code
	}
	ask.RegisterCodeMode() // the residue agent needs the seam; upgrade builds no MemoryRunner
	if _, err := memory.FallbackModel(*model); err != nil {
		return helptext.FailUsage("memory upgrade: --model: %v", err)
	}
	rep, err := memory.MemoryUpgrade(memory.UpgradeOptions{DryRun: *dryRun, To: *to, Model: *model, NoResolve: *noResolve})
	if err != nil {
		return helptext.Fail("memory upgrade: %v", err)
	}
	rep.Print(os.Stdout)
	if rep.Partial() {
		return 4
	}
	return 0
}

// cmdMemoryShadow exits 1 when a file's render from rows is not byte-equal to it.
func cmdMemoryShadow(args []string) int {
	fs := memoryFlagSet("shadow")
	rebuild := fs.Bool("rebuild", false, "import every in-scope memory file into state/memory.db")
	conflicts := fs.Bool("conflicts", false, "list the unresolved export conflicts (kind, file, time)")
	clearID := fs.String("clear", "", "with --conflicts: delete the kept copy at this store path and resolve its conflict")
	if code, done := memoryParseNoArgs(fs, "shadow", args); done {
		return code
	}
	if *clearID != "" {
		if err := shadow.ClearExportConflict(*clearID); err != nil {
			return helptext.Fail("memory shadow: %v", err)
		}
		return 0
	}
	if *conflicts {
		list, err := shadow.ExportConflicts()
		if err != nil {
			return helptext.Fail("memory shadow: %v", err)
		}
		for _, l := range list {
			fmt.Println(l)
		}
		return 0
	}
	if !*rebuild {
		return helptext.FailUsage("memory shadow: name the action: --rebuild or --conflicts")
	}
	rep, err := shadow.RebuildShadow(context.Background())
	if err != nil {
		return helptext.Fail("memory shadow: %v", err)
	}
	fmt.Printf("shadow: %d docs, %d entries, %d not byte-equal\n", rep.Docs, rep.Entries, len(rep.Mismatched))
	if len(rep.Mismatched) > 0 {
		return helptext.Fail("memory shadow: render differs for %s", strings.Join(rep.Mismatched[:min(5, len(rep.Mismatched))], ", "))
	}
	return 0
}

// cmdMemoryStore prints the store's truth, or switches it with --truth.
func cmdMemoryStore(args []string) int {
	fs := memoryFlagSet("store")
	truth := fs.String("truth", "", "switch the source of truth to files or db")
	if code, done := memoryParseNoArgs(fs, "store", args); done {
		return code
	}
	if helptext.Explicit(fs, "truth") {
		if *truth != "files" && *truth != "db" {
			return helptext.FailUsage("memory store: --truth %q: want files or db (usage: %s)", *truth, memorySubPage("store").Usage)
		}
		if err := memory.SetTruth(*truth); err != nil {
			return helptext.Fail("memory store: %v", err)
		}
	}
	t, err := memory.TruthErr()
	if err != nil {
		return helptext.Fail("memory store: %v", err)
	}
	fmt.Printf("truth: %s\n", t)
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
		"search":   cmdMemorySearch,
		"remember": cmdMemoryRemember,
		"init":     cmdMemoryInit,
		"review":   cmdMemoryReview,
		"promote":  cmdMemoryPromote,
		"update":   cmdMemoryUpdate,
		"dream":    cmdMemoryDream,
		// private: plumbing
		"index":   cmdMemoryIndex,
		"path":    cmdMemoryPath,
		"report":  cmdMemoryReport,
		"fmt":     cmdMemoryFmt,
		"watch":   cmdMemoryWatch,
		"import":  cmdMemoryImport,
		"sandbox": cmdMemorySandbox,
		"upgrade": cmdMemoryUpgrade,
		"extract": cmdMemoryExtract,
		"shadow":  cmdMemoryShadow,
		"store":   cmdMemoryStore,
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
		return helptext.FailUsage("memory: unknown subcommand %q (claude-code memory -h lists them)", sub)
	}
	return h(args[1:])
}

// memorySpec describes memory subcommands and their completion domains.
func memorySpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"search": {Flags: []helptext.Flag{helptext.Plain("k"), helptext.Plain("id"), helptext.Plain("open"),
			helptext.BoolFlag("explain"), helptext.CompletionFormatFlag()}},
		"remember": {Flags: []helptext.Flag{helptext.ValuedFlag("type", memoryRememberTypes), helptext.Plain("supersedes")}},
		"init":     {},
		"review": {Flags: []helptext.Flag{helptext.Plain("stale-days"), helptext.BoolFlag("notice"),
			helptext.BoolFlag("brief"), helptext.BoolFlag("candidates"), helptext.BoolFlag("timeline"), helptext.Plain("n"), helptext.Plain("candidate"),
			helptext.BoolFlag("approve"), helptext.Plain("enforced"),
			helptext.Plain("propose")}},
		"promote": {Flags: []helptext.Flag{
			helptext.ValuedFlag("to", func() []string { return memory.PromoteTargets })}},
		"update": {Flags: []helptext.Flag{helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("session"),
			helptext.BoolFlag("dry-run"), helptext.BoolFlag("historical"), helptext.BoolFlag("catchup"), helptext.Plain("skip")}},
		"dream": {Flags: []helptext.Flag{helptext.Plain("cwd"), helptext.BoolFlag("force"), helptext.BoolFlag("dry-run"), helptext.Plain("trigger")}},
		"index": {Flags: []helptext.Flag{helptext.BoolFlag("reembed"), helptext.BoolFlag("mint-only"), helptext.BoolFlag("dry-run")}},
		"path":  {},
		"report": {Flags: []helptext.Flag{helptext.Plain("label"), helptext.Plain("file"), helptext.BoolFlag("stdin"), helptext.Plain("job"),
			helptext.ValuedFlag("kind", func() []string { return memory.ReportKinds }), helptext.CompletionFormatFlag()}},
		"fmt":     {Flags: []helptext.Flag{helptext.BoolFlag("check"), helptext.BoolFlag("own")}},
		"watch":   {Flags: []helptext.Flag{helptext.Plain("session"), helptext.Plain("transcript"), helptext.Plain("cwd"), helptext.Plain("owner"), helptext.Plain("idle")}},
		"sandbox": {Flags: []helptext.Flag{helptext.BoolFlag("empty"), helptext.Plain("format")}},
		"upgrade": {Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.Plain("to"), helptext.Plain("model"), helptext.BoolFlag("no-resolve")}},
		"extract": {Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("purge"), helptext.Plain("format"), helptext.Plain("out")}},
		"shadow":  {Flags: []helptext.Flag{helptext.BoolFlag("rebuild"), helptext.BoolFlag("conflicts"), helptext.ValuedFlag("clear", nil)}},
		"store":   {Flags: []helptext.Flag{helptext.ValuedFlag("truth", func() []string { return []string{"files", "db"} })}},
		"import": {Flags: []helptext.Flag{helptext.Plain("from"), helptext.Plain("host"),
			helptext.BoolFlag("dry-run"), helptext.BoolFlag("topics"), helptext.BoolFlag("sync"), helptext.Plain("replace"),
			helptext.Plain("remove"), helptext.Plain("primary"), helptext.BoolFlag("plans")}},
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
		"reads like a rule? use memory promote --to rules instead; types: "+memory.RememberTypeHelp())
	old := fs.String("supersedes", "", "id of the live entry this fact replaces (see memory search)")
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(terms) == 0 {
		return helptext.Fail("memory remember: no fact text (usage: %s)", memorySubPage("remember").Usage)
	}
	id, err := memory.RememberSupersedes(*kind, strings.Join(terms, " "), *old, time.Now())
	if err != nil {
		return helptext.Fail("memory remember: %v", err)
	}
	fmt.Printf("remembered [%s] -> %s\n", id, paths.MemorySubDir(""))
	return 0
}

// memoryQuery is the flags recall and search share: -k and --format, plus the query terms.
type memoryQuery struct {
	fs         *flag.FlagSet
	k          *int
	format     *string
	sub, terms string
	termCount  int
	id         *string // search only: --id, which allows an empty query
}

func newMemoryQuery(sub string, k int) *memoryQuery {
	fs := memoryFlagSet(sub)
	return &memoryQuery{fs: fs, sub: sub, k: fs.Int("k", k, "maximum hits"), format: helptext.FormatFlag(fs)}
}

// parse reads args into q; done means the verb returns code (help, a usage error or no query).
func (q *memoryQuery) parse(args []string) (code int, done bool) {
	var output bytes.Buffer
	oldOutput := q.fs.Output()
	q.fs.SetOutput(&output)
	terms, err := helptext.ParseArgs(q.fs, args)
	q.fs.SetOutput(oldOutput)
	if err != nil {
		if _, msg, ok := strings.Cut(err.Error(), "for flag -format: "); ok {
			return helptext.BadFormat("memory "+q.sub, errors.New(msg)), true
		}
	}
	_, _ = oldOutput.Write(output.Bytes())
	if err != nil {
		return helptext.ExitCode(err), true
	}
	if helptext.Explicit(q.fs, "id") {
		q.termCount = len(terms)
		return 0, false
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
	q.id = q.fs.String("id", "", "print the one block with this id")
	open := q.fs.Int("open", 0, "append the bodies of the top N hits (4KB cap; -k 3)")
	explain := q.fs.Bool("explain", false, "explain hook recall; rejects --id+--open, --id+--explain, --open+--explain")
	if code, done := q.parse(args); done {
		return code
	}
	if msg := memorySearchConflict(q.fs, *open, *explain); msg != "" {
		return helptext.FailUsage("memory search: %s", msg)
	}
	if *explain {
		return memorySearchExplain(q.terms, q.json())
	}
	if helptext.Explicit(q.fs, "open") {
		return memorySearchOpen(q, *open)
	}
	if helptext.Explicit(q.fs, "id") {
		return memoryShowID(*q.id, q.termCount, q.json())
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

// memorySearchConflict names the first incompatible flag pair (or a bad --open) among --id, --open and --explain.
func memorySearchConflict(fs *flag.FlagSet, open int, explain bool) string {
	id, opened := helptext.Explicit(fs, "id"), helptext.Explicit(fs, "open")
	switch {
	case opened && open < 0:
		return "--open must be 0 or more"
	case id && opened:
		return "--id and --open cannot be combined"
	case id && explain:
		return "--id and --explain cannot be combined"
	case opened && explain:
		return "--open and --explain cannot be combined"
	}
	return ""
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

// memoryShowID is `memory search --id`: one id, no query terms; the access logs as AccessShow.
func memoryShowID(id string, extra int, json bool) int {
	if id == "" || extra != 0 || json {
		return helptext.FailUsage("memory search --id: give one block id (from `memory search` or a MEMORY.md region)")
	}
	return printMemoryID("memory search --id", id)
}

func printMemoryID(verb, id string) int {
	path, block, err := memory.Show(id)
	if err != nil {
		return helptext.Fail("%s %s: %v", verb, id, err)
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
	dryRun := fs.Bool("dry-run", false, "print the counts MEMORY.md and index.md would get; write nothing")
	if code, done := memoryParseNoArgs(fs, "index", args); done {
		return code
	}
	if err := memory.Index(os.Stdout, os.Stderr, memory.IndexOptions{Reembed: *reembed, MintOnly: *mintOnly, DryRun: *dryRun}); err != nil {
		return helptext.Fail("%v", err)
	}
	return 0
}

// memoryPathSubs are the sub dirs `memory path` resolves; SUB NAME names one file in the bank's.
var memoryPathSubs = []string{"handoffs", "reports", "knowledge", "worktrees", "techdebt"}

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
	switch len(pos) {
	case 0:
		fmt.Println(paths.MemorySubDir(""))
	case 1:
		fmt.Println(paths.MemorySubDir(pos[0]))
	default:
		fmt.Println(paths.MemorySub(pos[0], memoryPathName(pos[0], pos[1])))
	}
	return 0
}

// memoryPathName is the store file name for NAME in sub: date-first for dated subs, prefix-free, .md by default.
func memoryPathName(sub, name string) string {
	if sub == "knowledge" || sub == "techdebt" {
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
	job := fs.String("job", "", "dispatch id: write the raw --file/--stdin text to a derived reports/ path stored on its record")
	kind := fs.String("kind", "", "with --job: "+strings.Join(memory.ReportKinds, "|"))
	format := helptext.FormatFlag(fs)
	if code, done := memoryParseNoArgs(fs, "report", args); done {
		return code
	}
	if *job != "" {
		return cmdMemoryJobReport(*job, *kind, *file, *stdin, *format)
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

// cmdMemoryJobReport stores the raw report text under a Go-derived name and records the path on the dispatch.
func cmdMemoryJobReport(job, kind, file string, stdin bool, format string) int {
	if !slices.Contains(memory.ReportKinds, kind) {
		return helptext.FailUsage("memory report: --kind must be one of %s", strings.Join(memory.ReportKinds, ", "))
	}
	if (file == "") == !stdin {
		return helptext.FailUsage("memory report: give exactly one of --file or --stdin")
	}
	rec, err := agent.LoadRecord(job)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	body, err := memory.ReadReportBody(file, stdin)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	path, err := memory.JobReport(rec.Time, kind, cmp.Or(rec.PlanTask, rec.ID), body)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	if err := agent.NoteReport(rec.ID, path); err != nil {
		return helptext.Fail("memory report: %s written but not stored on %s: %v", path, rec.ID, err)
	}
	return printReportPaths([]string{path}, format)
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

// reviewFlagProblem names the first invalid flag combination of `memory review`, "" when it is valid.
func reviewFlagProblem(candidate, enforced, propose string, approve, timeline bool) string {
	switch {
	case (approve || enforced != "") && (candidate == "" || approve == (enforced != "")):
		return "--approve or --enforced needs --candidate and takes only one"
	case propose != "" && (timeline || candidate != ""):
		return "--propose stands alone"
	}
	return ""
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
	candidate := fs.String("candidate", "", "show a feedback candidate and its launch line")
	approve := fs.Bool("approve", false, "with --candidate: the owner said yes; dream skips it")
	enforced := fs.String("enforced", "", "with --candidate: close it with a main commit sha or file:line")
	propose := fs.String("propose", "", "append one feedback candidate, a JSON record without status")
	if code, done := memoryParseNoArgs(fs, "review", args); done {
		return code
	}
	if msg := reviewFlagProblem(*candidate, *enforced, *propose, *approve, *timeline); msg != "" {
		return helptext.FailUsage("memory review: %s", msg)
	}
	if *timeline {
		if err := memory.TimelineReport(os.Stdout, *n); err != nil {
			return helptext.Fail("memory review: %v", err)
		}
		return 0
	}
	err := memory.ReviewReport(os.Stdout, memory.ReviewOptions{StaleDays: *staleDays, Brief: *brief,
		Notice: *notice, Candidates: *candidates, Candidate: *candidate, Approve: *approve, Enforced: *enforced,
		Propose: *propose})
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

// cmdMemoryImport is `memory import`: --dry-run prints the plan and writes nothing.
func cmdMemoryImport(args []string) int {
	fs := helptext.NewFlagSet("memory import", memoryImportPage)
	from := fs.String("from", "", "memory root or store dir to merge in")
	host := fs.String("host", "", "tailnet host whose memory root to fetch over ssh")
	dryRun := fs.Bool("dry-run", false, "print the plan; write nothing")
	topics := fs.Bool("topics", false, "also topic files, backups and MEMORY.md pointers")
	sync := fs.Bool("sync", false, "pull, then push; all peers when no --host")
	replaceFile := fs.String("replace", "", "file of sha256 lines: replace those files on a match")
	removeFile := fs.String("remove", "", "file of sha256 lines: delete those files while the hash still matches")
	primary := fs.String("primary", "", "with --sync: self, a peer host or none; its version wins conflicts and deletions")
	plans := fs.Bool("plans", false, "apply a sync peer's plans from --from DIR")
	if code, done := memoryParseNoArgs(fs, "import", args); done {
		return code
	}
	if *plans {
		return memoryImportPlans(*from, *dryRun, *sync, *host, *replaceFile)
	}
	if *sync {
		return memorySync(*host, memorySyncFlags{*from, *replaceFile, *removeFile, *primary}, *dryRun)
	}

	if (*from == "") == (*host == "") {
		return helptext.FailUsage("memory import: give exactly one of --from or --host (usage: %s)", memoryImportPage.Usage)
	}
	replace, remove, err := memoryHashLists(*replaceFile, *removeFile)
	if err != nil {
		return helptext.FailUsage("memory import: %v", err)
	}
	if *primary != "" {
		return helptext.FailUsage("memory import: --primary goes with --sync (usage: %s)", memoryImportPage.Usage)
	}
	dir, label, err := memoryImportSource(*from, *host)
	if err != nil {
		return helptext.Fail("memory import: %v", err)
	}
	if *host != "" {
		defer func() { _ = os.RemoveAll(dir) }()
	}
	res, err := memory.ImportBank(memory.ImportOptions{From: dir, Label: label, DryRun: *dryRun, Topics: *topics, Replace: replace, Remove: remove})
	if err != nil {
		return memoryImportFail(err)
	}
	fmt.Println(strings.Join(append(res.Lines, res.Summary), "\n"))
	return 0
}

// memoryHashLists reads --replace and --remove, each a file of sha256 lines.
func memoryHashLists(replaceFile, removeFile string) (replace, remove map[string]string, err error) {
	if replace, err = memory.ReadReplaceList(replaceFile); err != nil {
		return nil, nil, fmt.Errorf("--replace: %w", err)
	}
	if remove, err = memory.ReadReplaceList(removeFile); err != nil {
		return nil, nil, fmt.Errorf("--remove: %w", err)
	}
	return replace, remove, nil
}

// memoryImportPlans is the peer side of the plans sync, run by the syncing machine over ssh.
func memoryImportPlans(from string, dryRun, sync bool, host, replaceFile string) int {
	if from == "" || sync || host != "" || replaceFile != "" {
		return helptext.FailUsage("memory import: --plans takes --from DIR and --dry-run only (usage: %s)", memoryImportPage.Usage)
	}
	lines, err := memory.ImportPlans(from, dryRun)
	fmt.Println(strings.Join(lines, "\n"))
	if err != nil {
		return helptext.Fail("memory import --plans: %v", err)
	}
	return 0
}

// memorySyncFlags are the import flags --sync sees; only primary is its own.
type memorySyncFlags struct{ from, replace, remove, primary string }

func (f memorySyncFlags) refusal(dryRun bool) string {
	switch {
	case f.from != "" || f.replace != "" || f.remove != "":
		return "--sync takes none of --from, --replace and --remove"
	case f.primary != "" && dryRun:
		return "--primary is saved, so it does not combine with --dry-run"
	}
	return ""
}

// memorySync is `memory import --sync`: the opt-in first, then the peers (--host, else CLAUDE_REMOTE_HOST).
func memorySync(host string, f memorySyncFlags, dryRun bool) int {
	if msg := f.refusal(dryRun); msg != "" {
		return helptext.FailUsage("memory import: %s (usage: %s)", msg, memoryImportPage.Usage)
	}
	if memory.InSandbox() {
		fmt.Fprintln(os.Stderr, "memory sync: refused: this config dir is a sandbox")
		return 0
	}
	if !memory.SyncOn() {
		return helptext.FailUsage(memory.SyncOffMsg)
	}
	peers := []string{host}
	if host == "" {
		peers = providers.RemoteHosts()
	}
	for i := range peers {
		peers[i] = strings.TrimSpace(peers[i])
	}
	if len(peers) == 0 {
		return helptext.FailUsage("memory import --sync: CLAUDE_REMOTE_HOST names no peer; give --host")
	}
	if f.primary != "" {
		if err := memory.SetSyncPrimary(f.primary); err != nil {
			return helptext.FailUsage("memory import: --primary: %v", err)
		}
	}
	code, err := memory.RunSync(peers, dryRun, os.Stdout)
	if err != nil {
		return helptext.Fail("memory sync: %v", err)
	}
	return code
}

// memoryImportFail is exit 2 for a bad --from or flag combination, 75 for a busy store, else exit 1.
func memoryImportFail(err error) int {
	if errors.Is(err, memory.ErrBankBusy) {
		helptext.Fail("memory import: %v", err)
		return memory.ImportBusyExit
	}
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
	if dir, err = memory.FetchRoot(host); err != nil {
		_ = os.RemoveAll(dir)
		err = fmt.Errorf("fetch from %s: %w", host, err)
	}
	return dir, host, err
}

// memorySearchExplain prints the hook's recall selection; --explain refuses --format json.
func memorySearchExplain(query string, json bool) int {
	if json {
		return helptext.FailUsage("memory search: --explain and --format json cannot be combined")
	}
	memory.RecallExplain(os.Stdout, query)
	return 0
}

// memorySearchOpen is the recall ranking (-k 3 unless given) as JSON, or as "id — title" lines plus n bodies.
func memorySearchOpen(q *memoryQuery, n int) int {
	k := *q.k
	if !helptext.Explicit(q.fs, "k") {
		k = 3
	}
	hits := memory.RecallBank(q.terms, k)
	if q.json() {
		return helptext.WriteJSONOrFail("memory search", hits)
	}
	memory.RecallReport(os.Stdout, os.Stderr, hits, n)
	return 0
}

func cmdMemorySandbox(args []string) int {
	fs := memoryFlagSet("sandbox")
	empty := fs.Bool("empty", false, "seed empty stores instead of copying content")
	format := helptext.FormatFlag(fs)
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
	_, err := fmt.Fprintf(os.Stdout, "sandbox  %s\nexport   CLAUDE_CONFIG_DIR=%s\nunset    NETWORK_MEMORY CLAUDE_REMOTE_HOST\nstores   %s\nremove with: rm -rf %s\n",
		res.Dir, res.Dir, strings.Join(stores, ", "), res.Dir)
	if err != nil {
		return helptext.Fail("memory sandbox: %v", err)
	}
	return 0
}

func cmdMemoryExtract(args []string) int {
	fs := memoryFlagSet("extract")
	dry := fs.Bool("dry-run", false, "classify and list purge candidates; write nothing in the store")
	purge := fs.Bool("purge", false, "purge only proven-dead files in one local commit; no model")
	format := fs.String("format", "text", "output format: text|json")
	out := fs.String("out", "", "write typed extract.json rows outside the store")
	if code, done := memoryParseNoArgs(fs, "extract", args); done {
		return code
	}
	if !*dry && !*purge {
		return helptext.FailUsage("memory extract: specify --dry-run or --purge")
	}
	if *format != "text" && *format != "json" {
		return helptext.FailUsage("memory extract: --format must be text or json")
	}
	if err := memory.RunExtract(memory.ExtractRunOptions{DryRun: *dry, Purge: *purge, Format: *format, Out: *out}, os.Stdout); err != nil {
		return helptext.Fail("memory extract: %v", err)
	}
	return 0
}

const learnEvery = 24 * time.Hour

// Seams for the two learn steps; tests replace them.
var (
	learnRouting = func(now time.Time) error { _, err := foldFit(now, false); return err }
	learnRecall  = memory.LearnRecall
)

// dailyLearn runs both learn steps once a day in the detached catch-up child; a failure logs and retries next day.
func dailyLearn(now time.Time, errw io.Writer) {
	if !catalog.ClaimEvery("learn-daily", learnEvery, now) {
		return
	}
	if err := learnRouting(now); err != nil {
		fmt.Fprintf(errw, "learn: routing fit: %v\n", err)
	}
	if err := learnRecall(now); err != nil {
		fmt.Fprintf(errw, "learn: recall weights: %v\n", err)
	}
}
