package memorycmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall"
	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

func cmdMemoryInit(args []string) int {
	fs, repo := memoryRepoFlagSet("init")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := memory.InitBank(os.Stdout, memstore.Root(*repo)); err != nil {
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
	root := memstore.Root(*repo)
	id, err := bank.Remember(root, *kind, strings.Join(terms, " "), time.Now())
	if err != nil {
		return helptext.Fail("memory remember: %v", err)
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
		return helptext.BadFormat("memory search", err)
	}
	if len(terms) == 0 {
		return helptext.Fail("memory search: no query (usage: %s)", memorySubPage("search").Usage)
	}
	res := memory.Search(memory.SearchOptions{Repo: *repo, All: *all, Query: strings.Join(terms, " "), K: *k,
		Expand: len(terms) == 1 && *format != helptext.JSON})
	if res.Block != "" {
		fmt.Printf("<!-- %s -->\n%s\n", res.Path, strings.TrimSpace(res.Block))
		return 0
	}
	if *format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, res.Hits); err != nil {
			return helptext.Fail("memory search: %v", err)
		}
		return 0
	}
	printMemorySearchHits(res.Hits)
	return 0
}

func printMemorySearchHits(hits []bank.SearchHit) {
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
		return helptext.Fail("%v", err)
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

// cmdMemoryReport appends a JSON report and its decisions to the repo's bank reports/ and prints both paths.
func cmdMemoryReport(args []string) int {
	fs, repo := memoryRepoFlagSet("report")
	label := fs.String("label", "", "report name: reports/<date>-<label>.md (sanitized to [A-Za-z0-9._-])")
	file := fs.String("file", "", "JSON input file: {\"report\": markdown, \"decisions\": [...]}")
	stdin := fs.Bool("stdin", false, "read the JSON input from stdin")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory report: unexpected argument %q", fs.Arg(0))
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("memory report", err)
	}
	if (*file == "") == !*stdin {
		return helptext.FailUsage("memory report: give exactly one of --file or --stdin")
	}
	in, err := readReportInput(*file, *stdin)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	// EnsureRepoDir registers the origin under the bank lock so the next call resolves the same key.
	dir, err := memstore.EnsureRepoDir(memstore.Root(*repo))
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	out, err := bank.AppendReport(dir, *label, in)
	if err != nil {
		return helptext.Fail("memory report: %v", err)
	}
	return printReportPaths(out, *format)
}

// readReportInput decodes the report JSON from file, or from stdin when stdin is set.
func readReportInput(file string, stdin bool) (bank.ReportInput, error) {
	var in bank.ReportInput
	var data []byte
	var err error
	if stdin {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return in, err
	}
	if err := json.Unmarshal(data, &in); err != nil {
		return in, fmt.Errorf("bad input JSON: %w", err)
	}
	return in, nil
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
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 { // a positional stops flag parsing, so --check after it would be ignored
		return helptext.FailUsage("memory fmt: unexpected argument %q", fs.Arg(0))
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
		return helptext.Fail("memory review: %v", err)
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
		helptext.Fail("memory promote: usage: %s", memorySubPage("promote").Usage)
		return 2
	}
	out, err := memory.Promote(memstore.Root(*repo), pos[0], *to, *dryRun, time.Now())
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

// `memory archive` governs Claude Code's native MEMORY.md/SEARCHABLE.md pair: the cap/settle
// machinery in memory.go owns the six typed bank files BY NAME, so native auto-memory — injected
// every session, uncapped — has no verb at all otherwise.

var memoryArchivePage = helptext.Page{
	Usage: "claude-code memory archive [flags] [<topic>...]",
	About: "Audit the native MEMORY.md store's injected cost (no writes); --apply moves named\n" +
		"topics to or from the archive, or --split N trims MEMORY.md to N pointer lines.",
}

// cmdMemoryArchive audits the native store's injected cost by default (names + stats, no writes);
// --apply moves named topics to/from the archive, or --split N trims MEMORY.md to N pointer lines.
func cmdMemoryArchive(args []string) int {
	fs := helptext.NewFlagSet("memory archive", memoryArchivePage)
	repo := fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
	apply := fs.Bool("apply", false, "perform the moves/split (default: dry run)")
	restore := fs.Bool("restore", false, "move the named topics back out of the archive")
	dupes := fs.Bool("dupes", false, "report near-duplicate topic files (no writes)")
	split := fs.Int("split", -1, "trim MEMORY.md to N pointers; move rest to SEARCHABLE.md")
	names, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	err = memory.ArchiveReport(os.Stdout, os.Stderr, memory.ArchiveReportOptions{Dir: memstore.NativeDir(memstore.Root(*repo)),
		Names: names, Apply: *apply, Restore: *restore, Dupes: *dupes, Split: *split})
	if err != nil {
		fmt.Fprintln(os.Stderr, "memory archive:", err)
		return 1
	}
	return 0
}

// memoryRecallExplain prints the hook's recall selection; --explain refuses the flags that need real hits.
func memoryRecallExplain(repo, query string, conflict bool) int {
	if conflict {
		return helptext.Fail("memory recall: --explain does not support --all, --format json, or --load")
	}
	memory.RecallExplain(os.Stdout, repo, query)
	return 0
}

func cmdMemoryRecall(args []string) int {
	fs, repo := memoryRepoFlagSet("recall")
	k := fs.Int("k", 3, "maximum hits")
	all := fs.Bool("all", false, "search the home tier and every repo store")
	load := fs.Int("load", 0, "append the show bodies of the top N hits (4KB cap)")
	explain := fs.Bool("explain", false, "explain hook recall selection and candidate scores")
	format := helptext.FormatFlag(fs)
	terms, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if err := helptext.CheckFormat(*format); err != nil {
		return helptext.BadFormat("memory recall", err)
	}
	if len(terms) == 0 {
		return helptext.Fail("memory recall: no query (usage: %s)", memorySubPage("recall").Usage)
	}
	if *explain {
		return memoryRecallExplain(*repo, strings.Join(terms, " "), *all || *format == helptext.JSON || *load != 0)
	}
	hits, _ := recall.SearchRepo(*repo, *all, strings.Join(terms, " "), *k)
	return memoryRecallEmit(hits, *format, *load)
}

// memoryRecallEmit writes hits as JSON, or as the text report with an optional body load.
func memoryRecallEmit(hits []bank.SearchHit, format string, load int) int {
	if format == helptext.JSON {
		if err := helptext.WriteJSON(os.Stdout, hits); err != nil {
			return helptext.Fail("memory recall: %v", err)
		}
		return 0
	}
	memory.RecallReport(os.Stdout, os.Stderr, hits, load)
	return 0
}
