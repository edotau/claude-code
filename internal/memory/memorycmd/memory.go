package memorycmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/claude"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/hooks/memhook"
	"github.com/edotau/claude-code/internal/memory"
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
	Hint: "plumbing: search, show, index, path, digest, fmt, watch, catchup, timeline,\n" +
		"          archive, alias — dispatchable, off the menu above\n" +
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
	"index":    {"", "settle the bank: rotate and archive over caps"},
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

// memoryFlagSet builds the per-subcommand flag.FlagSet shared by every handler below.
func memoryFlagSet(sub string) *flag.FlagSet {
	return helptext.NewFlagSet("memory "+sub, memorySubPage(sub))
}

// memoryRepoFlagSet adds --repo; only handlers that resolve a bank root take it, so it is never silently ignored.
func memoryRepoFlagSet(sub string) (*flag.FlagSet, *string) {
	fs := memoryFlagSet(sub)
	return fs, fs.String("repo", "", "project root (default: $CLAUDE_PROJECT_DIR, git toplevel, cwd)")
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
		DryRun: *dryRun, Historical: *historical, ClaudeBin: claude.ClaudeBinary, Run: client.MemoryRunner(ask.NewClient), Out: os.Stdout, Err: os.Stderr})
	if err != nil {
		return helptext.Fail("memory update: %v", err)
	}
	return 0
}

func cmdMemoryVerify(args []string, knownVerb func(string) bool) int {
	fs := memoryFlagSet("verify")
	cwd := fs.String("cwd", "", "project root whose bank to verify (default: cwd)")
	archives := fs.Bool("archives", false, "include archived bank blocks and native files")
	all := fs.Bool("all-stores", false, "verify every store, including home")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("memory verify: unexpected argument %q", fs.Arg(0))
	}
	if err := memory.Verify(os.Stdout, memory.VerifyOptions{CWD: *cwd, Archives: *archives,
		AllStores: *all, HelpVerb: func(verb string) bool { return memoryVerifyHelpVerb(verb, knownVerb) }}); err != nil {
		return helptext.Fail("memory verify: %v", err)
	}
	return 0
}

// memoryVerifyHelpVerb excludes launchers that forward -h after credential resolution or other side effects.
func memoryVerifyHelpVerb(verb string, knownVerb func(string) bool) bool {
	switch verb {
	case "claude", "opus", "sonnet", "haiku", "fable", "science", "wl-paste":
		return false
	}
	return knownVerb(verb)
}

func cmdMemoryDream(args []string) int {
	fs := memoryFlagSet("dream")
	cwd := fs.String("cwd", "", "project root whose bank to consolidate (default: cwd)")
	force := fs.Bool("force", false, "run even when not due")
	dryRun := fs.Bool("dry-run", false, "deterministic preview: caps, archivable blocks; no writes")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := memory.RunDream(memory.DreamRunOptions{CWD: *cwd, Force: *force, DryRun: *dryRun, Run: client.MemoryRunner(ask.NewClient), Out: os.Stdout}); err != nil {
		return helptext.Fail("memory dream: %v", err)
	}
	return 0
}

func cmdMemoryDigest(args []string) int {
	fs, repo := memoryRepoFlagSet("digest")
	if code, done := helptext.Parse(fs, args); done {
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
	fs.SetOutput(os.Stderr)
	session := fs.String("session", "", "session id (names the stamp)")
	tr := fs.String("transcript", "", "the session's transcript path")
	cwd := fs.String("cwd", "", "project root whose bank to update")
	idle := fs.Duration("idle", memhook.UpdateIdleAfter, "quiet period before the final update")
	owner := fs.Int("owner", 0, "session process pid; the watcher exits once it is gone (0 = unbounded)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	return memhook.IdleUpdateWatch(memhook.WatchOptions{Session: *session, Transcript: *tr, CWD: *cwd,
		Idle: *idle, Owner: *owner, Stderr: os.Stderr})
}

func cmdMemoryCatchup(args []string) int {
	fs := helptext.NewFlagSet("memory catchup", helptext.Page{Usage: "claude-code memory catchup [flags]",
		About: "Session-start sweep: saves finished sessions whose memory update was missed."})
	fs.SetOutput(os.Stderr)
	dry := fs.Bool("dry-run", false, "list the sessions it would save; write nothing")
	skip := fs.String("skip", "", "session id to leave alone (the one that started the sweep)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	return memhook.CatchUpSweep(memhook.SweepOptions{DryRun: *dry, Skip: *skip, Stdout: os.Stdout, Stderr: os.Stderr})
}

// memorySubcommands dispatches both public verbs and the off-menu plumbing verbs.
func memorySubcommands(knownVerb func(string) bool) map[string]func([]string) int {
	return map[string]func([]string) int{
		// public
		"recall":   cmdMemoryRecall,
		"remember": cmdMemoryRemember,
		"init":     cmdMemoryInit,
		"review":   cmdMemoryReview,
		"verify":   func(args []string) int { return cmdMemoryVerify(args, knownVerb) },
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
		"catchup":  cmdMemoryCatchup,
		"timeline": cmdMemoryTimeline,
		"archive":  cmdMemoryArchive,
		"alias":    cmdMemoryAlias,
	}
}

// Run dispatches the memory verb tree; knownVerb identifies the CLI's registered top-level verbs.
func Run(args []string, knownVerb func(string) bool) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		memoryPage.Fprint(os.Stderr)
		return 0
	}
	sub := args[0]
	h, ok := memorySubcommands(knownVerb)[sub]
	if !ok {
		return helptext.Fail("memory: unknown subcommand %q (claude-code memory -h lists them)", sub)
	}
	return h(args[1:])
}

// Spec describes memory subcommands and their completion domains.
func Spec() *completion.Node {
	flag := func(name string) completion.Flag { return completion.Flag{Name: name} }
	boolFlag := func(name string) completion.Flag { return completion.Flag{Name: name, IsBool: true} }
	valuedFlag := func(name string, values func() []string) completion.Flag {
		return completion.Flag{Name: name, Values: values}
	}
	formatFlag := func() completion.Flag {
		return valuedFlag("format", func() []string { return []string{helptext.Text, helptext.JSON} })
	}
	return &completion.Node{Sub: map[string]*completion.Node{
		"recall": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"),
			flag("load"), formatFlag()}},
		"remember": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), valuedFlag("type", memoryRememberTypes)}},
		"init":     {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"review": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), flag("stale-days"), boolFlag("notice"),
			boolFlag("brief")}},
		"promote": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames),
			valuedFlag("to", memoryPromoteTargets), boolFlag("dry-run")}},
		"update":   {Flags: []completion.Flag{flag("transcript"), flag("cwd"), flag("session")}},
		"dream":    {Flags: []completion.Flag{flag("cwd"), boolFlag("force")}},
		"search":   {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("all"), flag("k"), formatFlag()}},
		"show":     {},
		"digest":   {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"index":    {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"path":     {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames)}},
		"fmt":      {Flags: []completion.Flag{boolFlag("check")}},
		"catchup":  {},
		"watch":    {Flags: []completion.Flag{flag("session"), flag("transcript"), flag("cwd"), flag("owner"), flag("idle")}},
		"timeline": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), flag("n")}},
		"archive": {Flags: []completion.Flag{valuedFlag("repo", memoryRepoNames), boolFlag("dupes"), flag("split"),
			boolFlag("apply"), boolFlag("restore")}},
		"alias": {Flags: []completion.Flag{flag("repo"), valuedFlag("into", memoryRepoNames), boolFlag("dry-run")}},
	}}
}

// memoryRememberTypes mirrors bank.RememberTypes' keys for `memory remember --type` completion.
func memoryRememberTypes() []string { return []string{"user", "feedback", "project", "reference"} }

// memoryPromoteTargets mirrors memory.PromoteTargets for `memory promote --to` completion.
func memoryPromoteTargets() []string { return memory.PromoteTargets }

func memoryRepoNames() []string { return memstore.RepoKeys() }
