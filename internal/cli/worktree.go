package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/worktree"
)

var worktreePage = helptext.Page{
	Usage: "claude-code worktree <command> [flags] [<name>]",
	About: "Linked worktrees of the current repo under ~/worktrees/<repo>/: create one\n" +
		"per lane, then rebase it onto main, verify it and land it on your checkout.",
	Commands: [][2]string{
		{"list", "every worktree with status, reopen and cleanup commands"},
		{"create", "new worktree on branch worktree-<name> (--branch)"},
		{"land", "verified land on local, uncommitted; --dry-run, --resume"},
		{"collapse", "merge finished lanes into one collector, removing each (--dry-run)"},
		{"clean", "remove a landed worktree or its kept branch (--force, --dry-run)"},
		{"clean-all", "remove every eligible worktree (--dry-run|--yes)"},
		{"base", "pin the status line's diff base to <ref>, or --clear"},
		{"gate", "print (or --run) the verify gate for --dir (--scope, --lint-check)"},
		{"commit", "commit named files; main session may omit them (-m, --dry-run)"},
		{"pr", "list (--state, --author, --limit) | view|diff <id> | create --title"},
		{"checks", "CI results of a PR <id>, or of --sha"},
		{"comment", "comment on a PR <id> (--body-file, --reply-to)"},
	},
	Hint: worktreeVerifyNote + "\n\nclaude-code worktree <command> -h lists that command's flags.",
}

// worktreeVerifyNote explains land's modes and verification gate; it trails both the verb and the land page.
const worktreeVerifyNote = `Land on local: uncommitted changes in your checkout; you commit.
Land on local refuses (exit 3, nothing changed) when a landed path is dirty
in your checkout.
Rebase in the lane; a conflict aborts it, keeps the lane as it was, exits 3.
Verify in the lane on the rebased tree, before landing.
Set --verify or detect a gate from Makefile's test target, go.mod, pyproject,
package.json, or Cargo.toml. Without a gate, landing is refused.
Makefile + go.mod: build+vet all, test changed, importing and registry pkgs;
--full-verify runs the full Makefile gate. Only --no-verify skips the gate.`

// cmdWorktree dispatches the worktree verb tree.
func cmdWorktree(args []string) int {
	if code, ok := helpOrBare(worktreePage, args); ok {
		return code
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "gate": // reads a toolchain file only, so it runs outside a git repo
		return cmdWorktreeGate(rest)
	case "pr", "checks", "comment":
		return cmdScm(args)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("worktree: %v", err)
	}
	if sub == "commit" { // acts on the cwd's checkout, main or linked
		return cmdWorktreeCommit(cwd, rest)
	}
	repo, err := worktree.ResolveRepo(cwd)
	// a leading -h returns from the sub-verb's flag parse before repo is touched, so help works outside a repo
	if err != nil && !helptext.WantsHelp(rest) {
		return helptext.Fail("worktree: %v", err)
	}
	if handler, ok := worktreeHandlers[sub]; ok {
		return handler(repo, rest)
	}
	return unknownSub("worktree", sub, worktreePage)
}

var worktreeHandlers = map[string]func(*worktree.Repo, []string) int{
	"list":      cmdWorktreeList,
	"create":    cmdWorktreeCreate,
	"land":      cmdWorktreeLand,
	"collapse":  cmdWorktreeCollapse,
	"clean":     cmdWorktreeClean,
	"clean-all": cmdWorktreeCleanAll,
	"base":      cmdWorktreeBase,
}

func cmdWorktreeBase(_ *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree base", helptext.Page{Usage: "claude-code worktree base <ref> | --clear",
		About: "Pin the current branch's status-line diff base (git config branch.<b>.claudeBase)."})
	clear := fs.Bool("clear", false, "remove the pin; the fork point, upstream or default branch applies")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 && !*clear || len(rest) != 0 && *clear {
		return helptext.FailUsage("worktree base: give one <ref>, or --clear alone")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return helptext.Fail("worktree base: %v", err)
	}
	ref := ""
	if !*clear {
		ref = rest[0]
	}
	branch, err := worktree.PinBase(cwd, ref, *clear)
	if err != nil {
		return helptext.Fail("worktree base: %v", err)
	}
	if *clear {
		fmt.Printf("%s: base pin cleared\n", branch)
	} else {
		fmt.Printf("%s: base pinned to %s\n", branch, ref)
	}
	return 0
}

func cmdWorktreeList(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree list", helptext.Page{Usage: "claude-code worktree list [flags]",
		About: "List linked worktrees: +ahead -behind main, overlaps with lanes and main, reopen, cleanup."})
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	entries, err := worktree.List(repo)
	if err != nil {
		return helptext.Fail("worktree list: %v", err)
	}
	if *format == helptext.JSON {
		if entries == nil {
			entries = []worktree.Entry{} // [] not null, so JSON consumers can iterate
		}
		if code := helptext.WriteJSONOrFail("worktree list", entries); code != 0 {
			return code
		}
		return listExit(entries)
	}
	if len(entries) == 0 {
		fmt.Println("no worktrees under", repo.Root)
		return 0
	}
	for _, e := range entries {
		printEntry(e)
		if q, ok := worktree.QueuedFor(repo.Main, e.Path); ok {
			fmt.Printf("  queued:  %s; next: %s\n", strings.TrimSuffix(q.Reason+": "+q.Detail, ": "), q.Next)
		}
	}
	return listExit(entries)
}

// listExit is 1 when any lane's divergence could not be read, after the other lanes were printed.
func listExit(entries []worktree.Entry) int {
	if slices.ContainsFunc(entries, worktree.Entry.DivergenceUnknown) {
		return 1
	}
	return 0
}

// printEntry renders one text-mode row plus a quoted reopen command and, when eligible, a cleanup one.
func printEntry(e worktree.Entry) {
	name := filepath.Base(e.Path)
	ahead, behind := strconv.Itoa(e.Ahead), strconv.Itoa(e.Behind)
	if e.DivergenceUnknown() {
		ahead, behind = "?", "?"
	}
	fmt.Printf("%-24s branch=%-20s +%s -%s %s\n", name, e.Branch, ahead, behind, strings.Join(e.StatusFlags(), ","))
	for _, other := range slices.Sorted(maps.Keys(e.Overlaps)) {
		fmt.Printf("  overlaps %s: %s\n", other, strings.Join(e.Overlaps[other], ", "))
	}
	for _, p := range e.Problems {
		fmt.Printf("  ! %s\n", p)
	}
	fmt.Printf("  reopen:  cd %s\n", textutil.ShellQuote(e.Path))
	if e.EligibleForClean() {
		fmt.Printf("  cleanup: claude-code worktree clean %s\n", textutil.ShellQuote(name))
	}
}

// cmdWorktreeGate prints the scoped verify gate for --dir; exits 1 when the directory has no toolchain.
func cmdWorktreeGate(args []string) int {
	fs := helptext.NewFlagSet("worktree gate", helptext.Page{Usage: "claude-code worktree gate [flags]",
		About: "Print the scoped verify command for --dir's toolchain; exit 1 if none."})
	dir := fs.String("dir", ".", "repo directory to read the toolchain from")
	scope := fs.String("scope", "", "comma-separated dirs to limit the gate to (default: whole repo)")
	lintCheck := fs.Bool("lint-check", false, "prefix the agents diff lint check when the Makefile has a lint target")
	run := fs.Bool("run", false, "run the gate once and print {gate,source,exit,changed,failures} as JSON")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *run {
		return runWorktreeGate(*dir, textutil.SplitCSV(*scope), *lintCheck)
	}
	gate, source := worktree.ScopedVerifyGate(*dir, textutil.SplitCSV(*scope), *lintCheck)
	if gate == "" {
		return helptext.Fail("no verification gate found in %s", *dir)
	}
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("worktree gate", map[string]string{"gate": gate, "source": source})
	}
	if _, err := fmt.Println(gate); err != nil {
		return helptext.Fail("worktree gate: %v", err)
	}
	return 0
}

// runWorktreeGate prints the observed gate result; exit 0 means it ran, a red gate's exit is in the JSON by design.
func runWorktreeGate(dir string, scope []string, lintCheck bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := worktree.RunScopedGate(ctx, dir, scope, lintCheck)
	if err != nil {
		return helptext.Fail("worktree gate: %v", err)
	}
	return helptext.WriteJSONOrFail("worktree gate", r)
}

// cmdWorktreeCommit commits the named files (else this session's claimed ones); 1 refused, 2 usage.
func cmdWorktreeCommit(cwd string, args []string) int {
	fs := helptext.NewFlagSet("worktree commit", helptext.Page{Usage: "claude-code worktree commit -m <message>|--type <t> [flags] [<file>...]",
		About: "One pathspec commit of the named files, else the main session's claimed dirty ones.\n" +
			"Refuses a peer's file or a non-Conventional subject; never pushes.\n" +
			"--task writes Task: <plan-slug> <id>; a named task ticks once the commit is on main.\n" +
			"Quote an id with spaces: --task \"Phase 2.1\".\n" +
			"List the candidates first: claude-code worktree commit --list (no -m)."})
	msg := fs.String("m", "", "commit message; the subject must be a Conventional Commit")
	typ := fs.String("type", "", "Conventional type; with no -m the subject is the one --task's plan title")
	dryRun := fs.Bool("dry-run", false, "print the files and message notes without committing")
	list := fs.Bool("list", false, "print each dirty file as @id<TAB>path and exit; no -m needed")
	var tasks []string
	fs.Func("task", "name an open task `id` (ticks on landing); repeat for several", func(id string) error { tasks = append(tasks, id); return nil })
	plan := fs.String("plan", "", "plan file for --task (default: recent or claimed plans)")
	files, err := helptext.ParseArgs(fs, args) // files come first in the usual call, so flags may follow them
	if err != nil {
		return helptext.ExitCode(err)
	}
	if *list {
		return listCommitCandidates(cwd)
	}
	if *plan != "" {
		top, _ := paths.CheckoutOf(cwd)
		resolved, err := plans.ResolvePlan(top, *plan)
		if err != nil {
			return helptext.FailUsage("worktree commit: %v", err)
		}
		*plan = resolved
	}
	err = worktree.Commit(context.Background(), worktree.CommitRequest{
		Dir: cwd, Message: *msg, Type: *typ, Tasks: tasks, Plan: *plan, Paths: files, DryRun: *dryRun, Out: os.Stdout, Source: callerClaims(),
	})
	var ce *worktree.CommitError
	switch {
	case err == nil:
		if !*dryRun {
			top, _ := paths.CheckoutOf(cwd)
			recordCloseTerms(forTasks(plans.WorktreeName(top), plans.Home(paths.MainCheckout(top)), *plan, tasks), "", os.Stderr)
		}
		return 0
	case errors.As(err, &ce) && ce.Code == 2:
		return helptext.FailUsage("worktree commit: %v", err)
	}
	return helptext.Fail("worktree commit: %v", err)
}

// listCommitCandidates prints one @id<TAB>path line per dirty file.
func listCommitCandidates(cwd string) int {
	cands, err := worktree.Candidates(context.Background(), cwd)
	if err != nil {
		return helptext.Fail("worktree commit: %v", err)
	}
	for _, c := range cands {
		id, path, _ := strings.Cut(c, " ")
		if _, err := fmt.Printf("%s\t%s\n", id, path); err != nil {
			return helptext.Fail("worktree commit: %v", err)
		}
	}
	return 0
}

func cmdWorktreeCreate(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree create", helptext.Page{Usage: "claude-code worktree create [flags] <name>",
		About: "Create ~/worktrees/<repo>/<name> on branch worktree-<name> and print its path (an existing one is reused)."})
	branch := fs.String("branch", "", "branch to use (default worktree-<name>; reused if it already exists)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return helptext.Fail("worktree create: give exactly one name")
	}
	dest, err := worktree.Create(repo, rest[0], worktree.CreateOptions{Branch: *branch, Session: "cli", Log: os.Stderr})
	if err != nil {
		return helptext.Fail("worktree create: %v", err)
	}
	if _, err := fmt.Println(dest); err != nil {
		return helptext.Fail("worktree create: created %s but could not print its path: %v", dest, err)
	}
	return 0
}

// cmdWorktreeLand previews (--dry-run) or applies Preflight's two-phase land under one worktree lease.
func cmdWorktreeLand(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree land", helptext.Page{Usage: "claude-code worktree land [flags] <name>\nclaude-code worktree land --resume [flags] <name>",
		About: "Rebase <name> onto main, verify, then land it on your checkout as uncommitted changes.", Hint: worktreeVerifyNote})
	dryRun := fs.Bool("dry-run", false, "preview the plan and predicted conflicts without landing")
	target := fs.String("target", "", "require this main branch (default: current branch)")
	noClean := fs.Bool("no-clean", false, "leave the worktree on disk after a successful land")
	verify := fs.String("verify", "", "run this worktree gate after integration; failure blocks landing")
	noVerify := fs.Bool("no-verify", false, "land without any verification gate — the only way to skip one")
	fullVerify := fs.Bool("full-verify", false, "run the full Makefile gate instead of the scoped one")
	resolveAgent := fs.String("resolve-agent", "", "resolve a conflict in the lane with a tools-capable agent")
	resume := fs.Bool("resume", false, "resume --resolve-agent for <name> without restarting the agent")
	drain := fs.Bool("drain", false, "land every ready queued lane: one batch gate, a result per lane (no <name>)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if !worktree.ValidResolveAgent(*resolveAgent) {
		return helptext.Fail("worktree land: --resolve-agent %q unknown (%s)", *resolveAgent, strings.Join(worktree.ResolveAgents(), "|"))
	}
	rest := fs.Args()
	if *drain && len(rest) > 0 || !*drain && len(rest) != 1 {
		return helptext.Fail("worktree land: give exactly one name (--drain takes none)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *drain {
		return landDrain(ctx, repo, worktree.LandRequest{Target: *target, ResolveAgent: *resolveAgent, Verify: *verify,
			NoClean: *noClean, FullVerify: *fullVerify, NoVerify: *noVerify, Out: os.Stdout, Err: os.Stderr,
			ShowPlan: printLandPlan, ShowResolved: printResolved}, *dryRun)
	}
	name := rest[0]

	var landed string
	req := worktree.LandRequest{
		Name: name, Target: *target, ResolveAgent: *resolveAgent, Verify: *verify,
		Resume: *resume, NoClean: *noClean, FullVerify: *fullVerify, NoVerify: *noVerify,
		Out: os.Stdout, Err: os.Stderr, ShowPlan: printLandPlan, ShowResolved: printResolved,
		AfterLand: func(head string) { landed = head },
	}
	if *dryRun {
		return landDryRun(ctx, repo, req)
	}

	code := landUnderLease(ctx, repo, req)
	if landed != "" { // landed even when the clean failed
		recordCloseTerms(onLane(name, plans.Home(repo.Main)), landed, os.Stderr)
	}
	return code
}

// landDrain lands the ready queued lanes as one batch, a line per lane (dryRun: the plan only); busy exits 0.
func landDrain(ctx context.Context, repo *worktree.Repo, req worktree.LandRequest, dryRun bool) int {
	if dryRun { // the plan only: nothing is picked, gated, landed, dequeued or removed
		for _, line := range worktree.DrainPlan(ctx, repo, req) {
			fmt.Println(line)
		}
		return 0
	}
	results, err := worktree.DrainQueue(ctx, repo, req)
	if errors.Is(err, worktree.ErrDrainerBusy) {
		fmt.Println(err)
		return 0
	}
	if err != nil {
		return helptext.Fail("worktree land: %v", err)
	}
	if len(results) == 0 {
		fmt.Println("drain: no ready lanes queued")
	}
	code := 0
	for _, r := range results {
		fmt.Println(r.Line())
		if r.Err != nil {
			code = 1
		}
	}
	return code
}

func cmdWorktreeCollapse(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree collapse", helptext.Page{Usage: "claude-code worktree collapse [--dry-run] <collector> <lane>...",
		About: "Cherry-pick each lane's own commits into <collector> (created from main), building and vetting per lane."})
	dryRun := fs.Bool("dry-run", false, "print the pick plan and change nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return helptext.Fail("worktree collapse: give a collector and at least one lane")
	}
	err := worktree.Collapse(context.Background(), repo, worktree.CollapseRequest{Collector: rest[0], Lanes: rest[1:], DryRun: *dryRun, Out: os.Stdout})
	if err == nil {
		return 0
	}
	code := helptext.Fail("worktree collapse: %v", err)
	if errors.Is(err, worktree.ErrCollapseStopped) {
		return 3
	}
	return code
}

// landUnderLease runs mergeLocked holding the lane's lease, waiting for an active land; a lease failure is exit 1.
func landUnderLease(ctx context.Context, repo *worktree.Repo, req worktree.LandRequest) int {
	var code int
	if err := worktree.WithLandLease(ctx, repo, req.Name, os.Stdout, func(waitedOn string) error {
		if waitedOn != "" && worktree.LaneLanded(repo, req.Name) {
			fmt.Printf("land %s: already landed by %s\n", req.Name, waitedOn)
			return nil
		}
		code = landLocked(ctx, repo, req)
		return nil
	}); err != nil {
		return helptext.Fail("worktree land: %v", err)
	}
	return code
}

// landDryRun previews the plan without the lease or any mutation: a nil plan means it was blocked and already printed.
func landDryRun(ctx context.Context, repo *worktree.Repo, req worktree.LandRequest) int {
	if req.Resume {
		if err := worktree.CheckResumable(repo, req.Name); err != nil {
			return helptext.Fail("worktree land: %v", err)
		}
	}
	plan, err := worktree.PlanOrResolve(ctx, repo, req, true)
	if err != nil {
		code := helptext.Fail("worktree land: %v", err)
		if errors.Is(err, worktree.ErrLandRefused) {
			return 3
		}
		return code
	}
	if plan == nil {
		return 1
	}
	return 0
}

// landLocked runs worktree.Merge under the lease cmdWorktreeLand holds and prints the outcome.
func landLocked(ctx context.Context, repo *worktree.Repo, req worktree.LandRequest) int {
	out, err := worktree.Land(ctx, repo, req)
	if err != nil {
		helptext.Fail("worktree land: %v", err)
		if errors.Is(err, worktree.ErrRebaseConflict) || errors.Is(err, worktree.ErrLandRefused) {
			return 3 // rebase aborted, or the live tree left untouched; fix it and re-run
		}
		return 1
	}
	switch {
	case out.Blocked:
		return 1 // blocked, and not eligible for --resolve-agent: already printed
	case out.Clean == nil:
		fmt.Printf("worktree left on disk; clean up with: claude-code worktree clean %s\n", textutil.ShellQuote(req.Name))
	default:
		printCleanResult(*out.Clean, false)
	}
	return 0
}

// printLandPlan renders the pinned OIDs then every Issue; OIDs stay blank when an Issue stopped the plan early.
func printLandPlan(plan *worktree.LandPlan) {
	fmt.Printf("target: %s%s\n", plan.TargetRef, oidSuffix(plan.TargetOID))
	fmt.Printf("source: %s%s\n", plan.WorktreePath, oidSuffix(plan.SourceOID))
	if len(plan.Issues) == 0 {
		fmt.Println("clean — no predicted conflicts")
		return
	}
	for _, iss := range plan.Issues {
		fmt.Printf("issue: %s", iss.Code)
		if len(iss.Paths) > 0 {
			fmt.Printf(" %v", iss.Paths)
		}
		if iss.Detail != "" {
			fmt.Printf(" — %s", iss.Detail)
		}
		fmt.Println()
	}
}

func printResolved(r *worktree.LandResult) {
	fmt.Printf("resolved:   %s -> %s\n", worktree.ShortOID(r.BeforeOID), worktree.ShortOID(r.AfterOID))
}

// oidSuffix is " (abc12345)", or "" when the plan stopped before pinning this OID.
func oidSuffix(oid string) string {
	if oid == "" {
		return ""
	}
	return " (" + worktree.ShortOID(oid) + ")"
}

func cmdWorktreeClean(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree clean", helptext.Page{Usage: "claude-code worktree clean [flags] <name>",
		About: "Remove a merged worktree and its branch; --force discards unmerged work."})
	dryRun := fs.Bool("dry-run", false, "preview the recheck without removing anything")
	force := fs.Bool("force", false, "discard uncommitted and unmerged work")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return helptext.Fail("worktree clean: give exactly one name")
	}
	var result worktree.CleanResult
	var err error
	if *force {
		result, err = worktree.CleanForce(repo, rest[0], *dryRun, func(s worktree.DiscardSummary) {
			printDiscardSummary(s, *dryRun)
		})
	} else {
		result, err = worktree.Clean(repo, rest[0], *dryRun)
	}
	if err != nil {
		return helptext.Fail("worktree clean: %v", err)
	}
	printCleanResult(result, *dryRun)
	if !result.Removed {
		return 1
	}
	if *force && result.Reason != "" { // removeForced sets Reason only when branch -D failed after the removal
		return helptext.Fail("worktree clean: branch not deleted: %s", result.Reason)
	}
	return 0
}

// printDiscardSummary lists what --force is about to throw away, before CleanForce removes anything.
func printDiscardSummary(s worktree.DiscardSummary, dryRun bool) {
	verb := "discarding"
	if dryRun {
		verb = "would discard"
	}
	fmt.Printf("%s: %s\n", verb, s.Path)
	fmt.Printf("  uncommitted files: %d\n", s.Dirty)
	fmt.Printf("  unmerged commits: %d on %s (not in %s)\n", s.Unmerged, s.Branch, s.Target)
}

func cmdWorktreeCleanAll(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree clean-all", helptext.Page{Usage: "claude-code worktree clean-all [flags]",
		About: "Remove every eligible worktree; preview only unless --yes is given."})
	dryRun := fs.Bool("dry-run", false, "preview without removing anything")
	yes := fs.Bool("yes", false, "required to actually remove every eligible worktree")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	apply := *yes && !*dryRun
	results, err := worktree.CleanAll(repo, apply)
	if err != nil {
		return helptext.Fail("worktree clean-all: %v", err)
	}
	removed, failed := 0, 0
	for _, r := range results {
		printCleanResult(r, !apply)
		if r.Removed {
			removed++
		}
		if r.Failed {
			failed++
		}
	}
	if apply {
		fmt.Printf("removed %d, refused %d\n", removed, len(results)-removed)
	} else {
		fmt.Printf("%d eligible, %d refused — preview only, pass --yes to remove\n", removed, len(results)-removed)
	}
	if failed > 0 {
		return helptext.Fail("worktree clean-all: %d worktrees could not be checked", failed)
	}
	return 0
}

func printCleanResult(r worktree.CleanResult, dryRun bool) {
	switch {
	case r.Failed:
		fmt.Printf("error:    %s (%s)\n", r.Path, r.Reason)
	case !r.Removed:
		fmt.Printf("refused:  %s (%s)\n", r.Path, r.Reason)
	case dryRun:
		fmt.Printf("eligible: %s%s\n", r.Path, contentNote(r))
	default:
		fmt.Printf("removed:  %s%s\n", r.Path, contentNote(r))
		if r.BranchDeleted {
			fmt.Printf("  branch deleted: %s\n", r.Branch)
		}
	}
}

func contentNote(r worktree.CleanResult) string {
	if r.ContentFiles == 0 {
		return ""
	}
	return fmt.Sprintf(" (content already on main, %d files)", r.ContentFiles)
}

func names() []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return worktree.Names(cwd)
}

// worktreeSpec is the completion node for the worktree verb tree.
func worktreeSpec() *helptext.Node {
	sub := map[string]*helptext.Node{
		"list":   {Flags: []helptext.Flag{helptext.CompletionFormatFlag()}},
		"create": {Flags: []helptext.Flag{helptext.Plain("branch")}},
		"commit": {Flags: []helptext.Flag{helptext.Plain("m"), helptext.Plain("type"), helptext.Plain("task"), helptext.Plain("plan"), helptext.BoolFlag("dry-run"), helptext.BoolFlag("list")}},
		"land": {Args: []func() []string{names}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.Plain("target"),
			helptext.BoolFlag("no-clean"), {Name: "resolve-agent", Values: worktree.ResolveAgents}, helptext.BoolFlag("resume"), helptext.BoolFlag("drain"), helptext.Plain("verify"), helptext.BoolFlag("no-verify"), helptext.BoolFlag("full-verify")}},
		"clean":     {Args: []func() []string{names}, Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("force")}},
		"clean-all": {Flags: []helptext.Flag{helptext.BoolFlag("dry-run"), helptext.BoolFlag("yes")}},
		"base":      {Flags: []helptext.Flag{helptext.BoolFlag("clear")}},
		"gate":      {Flags: []helptext.Flag{helptext.Plain("dir"), helptext.Plain("scope"), helptext.BoolFlag("lint-check"), helptext.BoolFlag("run"), helptext.CompletionFormatFlag()}},
	}
	maps.Copy(sub, scmSpec().Sub)
	return &helptext.Node{Sub: sub}
}
