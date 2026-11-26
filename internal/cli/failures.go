package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/health"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

var failuresPage = helptext.Page{
	Usage: "claude-code failures <command> [flags]",
	About: "Failures broadcast to every live session: list them, claim one to fix, mark it done.",
	Commands: [][2]string{
		{"ls", "list failures, open first; --all adds done, --json"},
		{"claim", "claim <id>; prints the fix command, or exits 3 naming the holder"},
		{"done", "mark <id> fixed"},
		{"report", "record a failure for every live session"},
	},
	Hint: "claude-code failures <command> -h lists that command's flags.",
}

func failureKindList() string {
	names := make([]string, len(worktree.FailureKinds))
	for i, k := range worktree.FailureKinds {
		names[i] = string(k)
	}
	return strings.Join(names, "|")
}

// cmdFailures is the `failures` verb over the shared failure log.
func cmdFailures(args []string) int {
	if code, ok := helpOrBare(failuresPage, args); ok {
		return code
	}
	root := worktree.FailuresDir()
	switch args[0] {
	case "ls":
		return failuresLs(root, args[1:])
	case "claim":
		return failuresClaim(root, args[1:])
	case "done":
		return failuresDone(root, args[1:])
	case "report":
		return failuresReport(root, args[1:])
	}
	return unknownSub("failures", args[0], failuresPage)
}

// failureRow is one `ls --json` entry: the record, who holds it and whether it is fixed.
type failureRow struct {
	worktree.Failure
	Holder string `json:"holder,omitempty"`
	Done   bool   `json:"done,omitempty"`
}

// failureRows is the newest record per id, open first (newest first within each group).
func failureRows(root string, all bool) ([]failureRow, error) {
	var rows []failureRow
	seen := map[string]bool{}
	recs, err := worktree.ListFailures(root)
	if err != nil {
		return nil, err
	}
	for _, f := range slices.Backward(recs) {
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		holder, done := worktree.FailureHolder(root, f.ID)
		if done && !all {
			continue
		}
		rows = append(rows, failureRow{f, holder, done})
	}
	slices.SortStableFunc(rows, func(a, b failureRow) int {
		switch {
		case a.Done == b.Done:
			return 0
		case a.Done:
			return 1
		}
		return -1
	})
	return rows, nil
}

func failuresLs(root string, args []string) int {
	fs := helptext.NewFlagSet("failures ls", helptext.Page{Usage: "claude-code failures ls [--all] [--json]",
		About: "List failures, open first; a claimed one shows its holder."})
	all := fs.Bool("all", false, "include failures marked done")
	asJSON := fs.Bool("json", false, "print a JSON array")
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("failures ls: takes no arguments")
	}
	rows, err := failureRows(root, *all)
	if err != nil {
		return helptext.Fail("failures ls: %v", err)
	}
	if *asJSON {
		if rows == nil {
			rows = []failureRow{}
		}
		return helptext.WriteJSONOrFail("failures ls", rows)
	}
	for _, r := range rows {
		state := "open"
		switch {
		case r.Done:
			state = "done"
		case r.Holder != "":
			state = "claimed by " + r.Holder
		}
		fmt.Printf("%s  %-11s %s  [%s]  %s\n", r.ID, r.Kind, r.Subject, state, r.Summary)
	}
	return 0
}

// failureSession names the claimant: --session, else the harness session id, else this shell's parent pid.
func failureSession(flagVal string) string {
	for _, s := range []string{flagVal, os.Getenv("CLAUDE_SESSION_ID")} {
		if s != "" {
			return s
		}
	}
	return fmt.Sprintf("cli-%d", os.Getppid())
}

// failureFix is the fix command for f; path is the brief file it wrote, for the caller to delete.
func failureFix(f worktree.Failure) (cmd, path string, err error) {
	where := "its log"
	if f.Log != "" {
		where = f.Log
	}
	switch f.Kind {
	case worktree.FailDegraded:
		reason, _, _ := strings.Cut(f.Subject, " ")
		if slices.Contains([]string{health.ProxyDown, health.ProxyUnhealthy, health.RouterSecret, health.EnsureFailed}, reason) {
			return "claude-code doctor --proxy", "", nil
		}
		return health.Notice(reason), "", nil // a task-local reason: the main session takes over, nothing to dispatch
	case worktree.FailProxyDown, worktree.FailProxy5xx:
		return "claude-code doctor --proxy", "", nil
	case worktree.FailBench, worktree.FailHookP95:
		return "claude-code agents ask --agent claude --tools -- " + textutil.QuoteSH(
			fmt.Sprintf("Profile and fix the %s regression %s without dropping any check; evidence: %s", f.Kind, f.Subject, where)), "", nil
	}
	path, err = agent.NewBriefPath("claude")
	if err != nil {
		return "", "", err
	}
	task := fmt.Sprintf("Fix the %s failure %s.\n\nLog: %s\nRead it first.\n", f.Kind, f.Subject, where)
	if err := os.WriteFile(path, []byte(task), 0o600); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	return "claude-code agents ask --agent claude --tools - < " + textutil.QuoteSH(path), path, nil
}

func failuresClaim(root string, args []string) int {
	fs := helptext.NewFlagSet("failures claim", helptext.Page{Usage: "claude-code failures claim <id> [--session <id>]",
		About: "Claim a failure and print its fix command; a held one exits 3 naming the holder."})
	session := fs.String("session", "", "claimant (default: $CLAUDE_SESSION_ID, else this shell)")
	id, args := leadingID(args)
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	id, ok := oneID(id, fs.Args())
	if !ok {
		return helptext.FailUsage("failures claim: give exactly one <id>")
	}
	f, ok, err := findFailure(root, id)
	if err != nil {
		return helptext.Fail("failures claim: %v", err)
	}
	if !ok {
		return helptext.FailUsage("failures claim: no failure %q (see failures ls)", id)
	}
	if _, done := worktree.FailureHolder(root, id); done {
		return helptext.FailUsage("failures claim: %s is already done", id)
	}
	fix, brief, err := failureFix(f) // before the claim, so a failed write never leaves one held
	if err != nil {
		return helptext.Fail("failures claim: %v", err)
	}
	me := worktree.SessionSlug(failureSession(*session))
	holder, err := worktree.ClaimFailure(root, id, me)
	if err != nil || holder != me {
		if brief != "" {
			_ = os.Remove(brief)
		}
		if err != nil {
			return helptext.Fail("failures claim: %v", err)
		}
		fmt.Fprintf(os.Stderr, "held by %s\n", holder)
		return helptext.ExitAuth
	}
	fmt.Println(fix)
	return 0
}

// oneID is the single id given before or after the flags; ok is false for none or several.
func oneID(lead string, rest []string) (string, bool) {
	switch {
	case lead != "" && len(rest) == 0:
		return lead, true
	case lead == "" && len(rest) == 1:
		return rest[0], true
	}
	return "", false
}

func findFailure(root, id string) (worktree.Failure, bool, error) {
	recs, err := worktree.ListFailures(root)
	if err != nil {
		return worktree.Failure{}, false, err
	}
	for _, r := range slices.Backward(recs) {
		if r.ID == id {
			return r, true, nil
		}
	}
	return worktree.Failure{}, false, nil
}

func failuresDone(root string, args []string) int {
	fs := helptext.NewFlagSet("failures done", helptext.Page{Usage: "claude-code failures done <id>",
		About: "Mark a failure fixed; it leaves the default list."})
	id, args := leadingID(args)
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	id, ok := oneID(id, fs.Args())
	if !ok {
		return helptext.FailUsage("failures done: give exactly one <id>")
	}
	if _, ok, err := findFailure(root, id); err != nil {
		return helptext.Fail("failures done: %v", err)
	} else if !ok {
		return helptext.FailUsage("failures done: no failure %q (see failures ls)", id)
	}
	if err := worktree.DoneFailure(root, id); err != nil {
		return helptext.Fail("failures done: %v", err)
	}
	fmt.Println("done", id)
	return 0
}

func failuresReport(root string, args []string) int {
	fs := helptext.NewFlagSet("failures report", helptext.Page{Usage: "claude-code failures report --kind K --subject S --summary S [--log P]",
		About: "Record a failure for every live session."})
	kind := fs.String("kind", "", "failure kind (completion lists them)")
	subject := fs.String("subject", "", "what failed (with kind, it fixes the id)")
	summary := fs.String("summary", "", "one line of what happened")
	logPath := fs.String("log", "", "path of the log to read")
	if code, stop := helptext.Parse(fs, args); stop {
		return code
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("failures report: give --kind and --subject, no positional arguments")
	}
	k := worktree.FailureKind(*kind)
	if !slices.Contains(worktree.FailureKinds, k) {
		return helptext.FailUsage("failures report: --kind %q is not one of %s", *kind, failureKindList())
	}
	subj := strings.Join(strings.Fields(*subject), " ")
	if subj == "" || len(subj) > 200 {
		return helptext.FailUsage("failures report: --subject must be 1 to 200 bytes")
	}
	appended, err := worktree.ReportFailure(root, worktree.Failure{Kind: k, Subject: subj, Summary: *summary, Log: *logPath})
	if err != nil {
		return helptext.Fail("failures report: %v", err)
	}
	id := worktree.FailureID(k, subj)
	if appended {
		fmt.Println("reported", id)
	} else {
		fmt.Println("duplicate", id)
	}
	return 0
}

// failuresSpec returns the failures completion tree.
func failuresSpec() *helptext.Node {
	return &helptext.Node{Sub: map[string]*helptext.Node{
		"ls":     {Flags: []helptext.Flag{helptext.BoolFlag("all"), helptext.BoolFlag("json")}},
		"claim":  {Flags: []helptext.Flag{{Name: "session"}}},
		"done":   {},
		"report": {Flags: []helptext.Flag{helptext.ValuedFlag("kind", func() []string { return strings.Split(failureKindList(), "|") }), {Name: "subject"}, {Name: "summary"}, {Name: "log"}}},
	}}
}
