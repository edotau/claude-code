package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/worktree"
)

var worktreePage = helptext.Page{
	Usage: "claude-code worktree <command> [flags] [<name>]",
	About: "Linked worktrees of the current repo under ~/worktrees/<repo>/: create one\n" +
		"per task, then merge it back with a verified fast-forward and clean it up.",
	Commands: [][2]string{
		{"list", "every worktree with status, reopen and cleanup commands"},
		{"create <name>", "new worktree on branch worktree-<name> (--branch)"},
		{"merge <name>", "verified fast-forward merge; --dry-run previews, --resume"},
		{"clean <name>", "remove one merged worktree (--force, --dry-run)"},
		{"clean-all", "remove every eligible worktree (--dry-run|--yes)"},
	},
	Hint: worktreeVerifyNote + "\n\nclaude-code worktree <command> -h lists that command's flags.",
}

// worktreeVerifyNote explains merge's verification gate; it trails both the verb and the merge page.
const worktreeVerifyNote = `Verify inside the worktree after integration, before promotion.
Set --verify or detect a gate from Makefile's test target, go.mod, pyproject,
package.json, or Cargo.toml. Without a gate, promotion is refused.
Only --no-verify skips the gate.`

func cmdWorktree(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		worktreePage.Fprint(os.Stderr)
		return 0
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail("worktree: %v", err)
	}
	sub, rest := args[0], args[1:]
	repo, err := worktree.ResolveRepo(cwd)
	// a leading -h returns from the sub-verb's flag parse before repo is touched, so help works outside a repo
	if err != nil && (len(rest) == 0 || (rest[0] != "-h" && rest[0] != "--help")) {
		return fail("worktree: %v", err)
	}
	switch sub {
	case "list":
		return cmdWorktreeList(repo, rest)
	case "create":
		return cmdWorktreeCreate(repo, rest)
	case "merge":
		return cmdWorktreeMerge(repo, rest)
	case "clean":
		return cmdWorktreeClean(repo, rest)
	case "clean-all":
		return cmdWorktreeCleanAll(repo, rest)
	default:
		fail("worktree: unknown subcommand %q", sub)
		worktreePage.Fprint(os.Stderr)
		return 2
	}
}

func cmdWorktreeList(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree list", helptext.Page{Usage: "claude-code worktree list [flags]"})
	format := outfmt.Flag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if err := outfmt.Check(*format); err != nil {
		return fail("%v", err)
	}
	entries, err := worktree.List(repo)
	if err != nil {
		return fail("worktree list: %v", err)
	}
	if *format == outfmt.JSON {
		_ = outfmt.Write(os.Stdout, entries)
		return 0
	}
	if len(entries) == 0 {
		fmt.Println("no worktrees under", repo.Root)
		return 0
	}
	for _, e := range entries {
		printEntry(e)
	}
	return 0
}

// printEntry renders one text-mode row plus a quoted reopen command and, when eligible, a cleanup one.
func printEntry(e worktree.Entry) {
	name := filepath.Base(e.Path)
	var status []string
	if e.Dirty {
		status = append(status, "dirty")
	}
	if e.Merged {
		status = append(status, "merged")
	}
	if e.Stale {
		status = append(status, fmt.Sprintf("stale(%s)", e.ActivitySource))
	}
	if e.Orphaned {
		status = append(status, "orphaned")
	}
	if e.Prunable {
		status = append(status, "prunable")
	}
	if e.Locked {
		status = append(status, "locked")
	}
	if len(status) == 0 {
		status = append(status, "clean")
	}
	fmt.Printf("%-24s branch=%-20s %s\n", name, e.Branch, strings.Join(status, ","))
	for _, p := range e.Problems {
		fmt.Printf("  ! %s\n", p)
	}
	fmt.Printf("  reopen:  cd %s\n", shellQuote(e.Path))
	if eligibleForClean(e) {
		fmt.Printf("  cleanup: claude-code worktree clean %s\n", shellQuote(name))
	}
}

// eligibleForClean mirrors the bar Clean itself rechecks live; list only offers the command, never acts.
func eligibleForClean(e worktree.Entry) bool {
	return e.Exists && !e.Orphaned && !e.Locked && !e.Dirty && e.Merged
}

func cmdWorktreeCreate(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree create", helptext.Page{Usage: "claude-code worktree create [flags] <name>"})
	branch := fs.String("branch", "", "branch to use (default worktree-<name>; reused if it already exists)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail("worktree create: give exactly one name")
	}
	dest, err := worktree.Create(repo, rest[0], *branch)
	if err != nil {
		return fail("worktree create: %v", err)
	}
	fmt.Println(dest)
	return 0
}

// cmdWorktreeMerge previews (or, without --dry-run, applies) Preflight's two-phase merge: Preflight
// always runs and prints the plan first, so an Issue — including a predicted conflict — is visible
// before anything mutates. A predicted conflict normally stops here; --resolve-agent hands it to an
// agent instead (worktree.ResolveWithAgent), and --resume reattaches to a previously started one
// without ever re-invoking the agent. A SIGINT/SIGTERM cancels ctx: Promote never starts the ff-only
// update once that happens, so a cancel during apply either completes the promote or leaves main
// untouched. The whole non-dry-run apply (resolve-or-plan -> verify gate -> integrate -> verify ->
// promote -> clean) runs under one worktree.WithWorktreeLease hold, so nothing else can interleave.
func cmdWorktreeMerge(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree merge", helptext.Page{Usage: "claude-code worktree merge [flags] <name>\nclaude-code worktree merge --resume [flags] <name>", Hint: worktreeVerifyNote})
	dryRun := fs.Bool("dry-run", false, "preview the plan and predicted conflicts without merging")
	target := fs.String("target", "", "require this main branch (default: current branch)")
	noClean := fs.Bool("no-clean", false, "leave the worktree on disk after a successful promote")
	verify := fs.String("verify", "", "run this worktree gate after integration; failure blocks promotion")
	noVerify := fs.Bool("no-verify", false, "promote without any verification gate — the only way to skip one")
	resolveAgent := fs.String("resolve-agent", "", "resolve predicted conflicts with claude|codex")
	resume := fs.Bool("resume", false, "resume --resolve-agent for <name> without restarting the agent")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if *resolveAgent != "" && *resolveAgent != "claude" && *resolveAgent != "codex" {
		return fail("worktree merge: --resolve-agent %q unknown (claude|codex)", *resolveAgent)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail("worktree merge: give exactly one name")
	}
	name := rest[0]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *dryRun {
		return mergeDryRun(ctx, repo, name, *target, *resolveAgent)
	}

	var code int
	if err := worktree.WithWorktreeLease(repo, name, func() error {
		code = mergeLocked(ctx, repo, name, *target, *resolveAgent, *resume, *noClean, *verify, *noVerify)
		return nil
	}); err != nil {
		return fail("worktree merge: %v", err)
	}
	return code
}

// mergeDryRun previews the plan (including a predicted conflict) without acquiring the worktree lease
// or mutating anything: plan == nil means planOrResolve already printed why this is blocked.
func mergeDryRun(ctx context.Context, repo *worktree.Repo, name, target, resolveAgent string) int {
	plan, err := planOrResolve(ctx, repo, name, target, resolveAgent, true)
	if err != nil {
		return fail("worktree merge: %v", err)
	}
	if plan == nil {
		return 1
	}
	return 0
}

// mergeLocked runs the full non-dry-run merge lifecycle under the lease cmdWorktreeMerge already
// acquired: resume-or-plan, the verify-gate decision, then integrate -> verify -> promote -> clean.
func mergeLocked(ctx context.Context, repo *worktree.Repo, name, target, resolveAgent string, resume, noClean bool, verify string, noVerify bool) int {
	var plan *worktree.MergePlan
	var err error
	if resume {
		plan, err = resumeWorktreeMerge(ctx, repo, name, target)
	} else {
		plan, err = planOrResolve(ctx, repo, name, target, resolveAgent, false)
	}
	if err != nil {
		return fail("worktree merge: %v", err)
	}
	if plan == nil {
		return 1 // blocked, and not eligible for --resolve-agent: already printed
	}

	gate, gateSource, err := chooseVerifyGate(plan.WorktreePath, verify, noVerify)
	if err != nil {
		return fail("worktree merge: %v", err)
	}
	return applyMerge(ctx, repo, plan, name, gate, gateSource, noClean)
}

// applyMerge runs IntegrateTarget -> the verify gate (if any) -> Promote -> an optional Clean, already
// inside the caller's worktree lease — so it calls worktree.CleanInLease, never the self-locking Clean.
func applyMerge(ctx context.Context, repo *worktree.Repo, plan *worktree.MergePlan, name, gate, gateSource string, noClean bool) int {
	integrated, err := worktree.IntegrateTarget(ctx, repo, plan)
	if err != nil {
		return fail("worktree merge: integrate: %v", err)
	}
	fmt.Printf("integrated: %s -> %s\n", shortOID(integrated.BeforeOID), shortOID(integrated.AfterOID))

	if gate != "" {
		if err := runVerify(ctx, plan.WorktreePath, gate); err != nil {
			fmt.Fprintf(os.Stderr, "worktree left on disk at %s for inspection\n", plan.WorktreePath)
			return fail("worktree merge: verify failed, not promoting: %v", err)
		}
		fmt.Printf("verify: passed (%s)\n", gateSource)
	} else {
		fmt.Println("verify: skipped (--no-verify) — promoting unverified")
	}

	promoted, err := worktree.Promote(ctx, repo, plan)
	if err != nil {
		if promoted != nil && promoted.ResumeHint != "" {
			fmt.Fprintln(os.Stderr, "  retry:", promoted.ResumeHint)
		}
		return fail("worktree merge: promote: %v", err)
	}
	fmt.Printf("promoted:   %s -> %s on %s\n", shortOID(promoted.BeforeOID), shortOID(promoted.AfterOID), plan.TargetRef)

	if noClean {
		fmt.Printf("worktree left on disk; clean up with: claude-code worktree clean %s\n", shellQuote(name))
		return 0
	}
	result, err := worktree.CleanInLease(repo, name, false)
	if err != nil {
		return fail("worktree merge: clean: %v", err)
	}
	printCleanResult(result, false)
	return 0
}

// chooseVerifyGate picks the command applyMerge runs after integration and before promotion: --no-verify
// skips it outright, an explicit --verify wins next, otherwise it is auto-detected from worktreePath the
// same way the green-keeper-loop workflow does (workflows/green-keeper-loop.js: Makefile test target -> go.mod -> pyproject
// -> package.json -> Cargo.toml) — pure file checks, no execution, no network. Finding nothing and not
// passed --no-verify refuses outright, before anything is integrated.
func chooseVerifyGate(worktreePath, verify string, noVerify bool) (gate, source string, err error) {
	if noVerify {
		return "", "", nil
	}
	if verify != "" {
		return verify, "flag", nil
	}
	gate, source = detectVerifyGate(worktreePath)
	if gate == "" {
		return "", "", errors.New(`no verification gate found — pass --verify "<cmd>" or --no-verify`)
	}
	return gate, source, nil
}

// detectVerifyGate mirrors workflows/green-keeper-loop.js's detection order over dir's own files.
func detectVerifyGate(dir string) (gate, source string) {
	if hasMakeTarget(filepath.Join(dir, "Makefile"), "test") {
		return "make test", "Makefile"
	}
	if fileExists(filepath.Join(dir, "go.mod")) {
		return "go build ./... && go vet ./... && go test ./...", "go.mod"
	}
	if fileExists(filepath.Join(dir, "pyproject.toml")) || fileExists(filepath.Join(dir, "setup.cfg")) {
		return "pytest -q", "pyproject"
	}
	if hasNPMTestScript(filepath.Join(dir, "package.json")) {
		return "npm test", "package.json"
	}
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return "cargo build && cargo test", "Cargo.toml"
	}
	return "", ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// hasMakeTarget reports whether makefile declares a target named exactly target (a line starting with
// "target:"). A missing Makefile is simply "no target", never an error.
func hasMakeTarget(makefile, target string) bool {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return false
	}
	prefix := target + ":"
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// hasNPMTestScript reports whether pkgJSON declares a "test" script.
func hasNPMTestScript(pkgJSON string) bool {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	_, ok := pkg.Scripts["test"]
	return ok
}

// printMergePlan renders Preflight's plan: the pinned OIDs, then every Issue found (empty = clean).
// An Issue found before OIDs are pinned (dirty/detached/mismatch) leaves them blank, not stale.
func printMergePlan(plan *worktree.MergePlan) {
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

// planOrResolve runs Preflight and prints the plan; a predicted conflict with --resolve-agent set
// hands it to worktree.ResolveWithAgent and re-Preflights once the worktree is clean and committed
// again. A nil plan with a nil error means the caller already printed why this is blocked (exit 1):
// dry-run, no --resolve-agent, or an Issue --resolve-agent can't act on (anything but a conflict).
func planOrResolve(ctx context.Context, repo *worktree.Repo, name, target, resolveAgent string, dryRun bool) (*worktree.MergePlan, error) {
	plan, err := worktree.Preflight(ctx, repo, name, target)
	if err != nil {
		return nil, err
	}
	printMergePlan(plan)
	if len(plan.Issues) == 0 {
		return plan, nil
	}
	if dryRun || resolveAgent == "" || !onlyConflictIssue(plan.Issues) {
		return nil, nil
	}

	origTargetRef, origTargetOID := plan.TargetRef, plan.TargetOID
	_, result, err := worktree.ResolveWithAgent(ctx, repo, plan, resolveAgent, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	printResolved(result)

	plan, err = worktree.PreflightAfterResolution(ctx, repo, name, target) // the resolution is committed now
	if err != nil {
		return nil, err
	}
	if len(plan.Issues) > 0 {
		printMergePlan(plan)
		return nil, fmt.Errorf("still blocked after agent resolution (%s)", issueCodes(plan.Issues))
	}
	if plan.TargetRef != origTargetRef || plan.TargetOID != origTargetOID {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) during resolution — re-run worktree merge explicitly to retry against the new target",
			origTargetRef, shortOID(origTargetOID), plan.TargetRef, shortOID(plan.TargetOID))
	}
	return plan, nil
}

// resumeWorktreeMerge reattaches to the --resolve-agent operation saved for name: it polls the
// worktree's current state (worktree.ResolveWithAgent with a non-nil op never re-invokes the agent)
// and, once resolved, returns a fresh Preflight plan ready for the normal integrate+promote path. Both
// the saved target branch and OID must still match — a branch switched to a different ref at the same
// commit is still a moved target.
func resumeWorktreeMerge(ctx context.Context, repo *worktree.Repo, name, target string) (*worktree.MergePlan, error) {
	op, err := worktree.LoadOperation(repo, name)
	if err != nil {
		return nil, fmt.Errorf("no in-progress --resolve-agent operation found for %q: %w", name, err)
	}
	if op.Repo != repo.Main {
		return nil, fmt.Errorf("operation %s belongs to %s, not %s", op.ID, op.Repo, repo.Main)
	}
	worktreePath := filepath.Join(repo.Root, op.Worktree)
	if _, err := os.Stat(worktreePath); err != nil {
		return nil, fmt.Errorf("worktree %q not found at %s: %w", op.Worktree, worktreePath, err)
	}
	plan := &worktree.MergePlan{WorktreePath: worktreePath}
	_, result, err := worktree.ResolveWithAgent(ctx, repo, plan, op.Agent, op)
	if err != nil {
		return nil, err
	}
	printResolved(result)
	fresh, err := worktree.PreflightAfterResolution(ctx, repo, name, target)
	if err != nil {
		return nil, err
	}
	if len(fresh.Issues) > 0 {
		printMergePlan(fresh)
		return nil, fmt.Errorf("still blocked after resuming resolution (%s)", issueCodes(fresh.Issues))
	}
	if fresh.TargetRef != op.TargetRef || fresh.TargetOID != op.TargetOID {
		return nil, fmt.Errorf("target moved from %s (%s) to %s (%s) since the operation started — re-run worktree merge explicitly to retry against the new target",
			op.TargetRef, shortOID(op.TargetOID), fresh.TargetRef, shortOID(fresh.TargetOID))
	}
	return fresh, nil
}

// onlyConflictIssue reports whether every blocking Issue is a predicted conflict — the one case
// --resolve-agent is allowed to act on, never a dirty/detached/mismatched checkout.
func onlyConflictIssue(issues []worktree.Issue) bool {
	if len(issues) == 0 {
		return false
	}
	for _, iss := range issues {
		if iss.Code != worktree.IssueConflict {
			return false
		}
	}
	return true
}

func issueCodes(issues []worktree.Issue) string {
	codes := make([]string, len(issues))
	for i, iss := range issues {
		codes[i] = iss.Code
	}
	return strings.Join(codes, ", ")
}

func printResolved(r *worktree.MergeResult) {
	fmt.Printf("resolved:   %s -> %s\n", shortOID(r.BeforeOID), shortOID(r.AfterOID))
}

func shortOID(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	return oid
}

// oidSuffix is " (abc12345)", or "" when the plan stopped before pinning this OID.
func oidSuffix(oid string) string {
	if oid == "" {
		return ""
	}
	return " (" + shortOID(oid) + ")"
}

func cmdWorktreeClean(repo *worktree.Repo, args []string) int {
	fs := helptext.NewFlagSet("worktree clean", helptext.Page{Usage: "claude-code worktree clean [flags] <name>"})
	dryRun := fs.Bool("dry-run", false, "preview the recheck without removing anything")
	force := fs.Bool("force", false, "discard uncommitted and unmerged work")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail("worktree clean: give exactly one name")
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
		return fail("worktree clean: %v", err)
	}
	printCleanResult(result, *dryRun)
	if !result.Removed {
		return 1
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
	fs := helptext.NewFlagSet("worktree clean-all", helptext.Page{Usage: "claude-code worktree clean-all [flags]"})
	dryRun := fs.Bool("dry-run", false, "preview without removing anything")
	yes := fs.Bool("yes", false, "required to actually remove every eligible worktree")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	apply := *yes && !*dryRun
	results, err := worktree.CleanAll(repo, apply)
	if err != nil {
		return fail("worktree clean-all: %v", err)
	}
	removed := 0
	for _, r := range results {
		printCleanResult(r, !apply)
		if r.Removed {
			removed++
		}
	}
	if apply {
		fmt.Printf("removed %d, refused %d\n", removed, len(results)-removed)
	} else {
		fmt.Printf("%d eligible, %d refused — preview only, pass --yes to remove\n", removed, len(results)-removed)
	}
	return 0
}

// runVerify runs cmdStr inside workDir via `bash -c 'set -o pipefail; ...'`, so a failing stage in a
// pipeline (e.g. `false | cat`) fails the gate instead of reading as passed; inherits stdout/stderr so
// the caller sees the gate's own output. ctx cancellation kills cmdStr; a nonzero exit (including from
// cancellation) is the one signal that stops promotion.
func runVerify(ctx context.Context, workDir, cmdStr string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -o pipefail; "+cmdStr)
	cmd.Dir = workDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printCleanResult(r worktree.CleanResult, dryRun bool) {
	switch {
	case !r.Removed:
		fmt.Printf("refused:  %s (%s)\n", r.Path, r.Reason)
	case dryRun:
		fmt.Printf("eligible: %s\n", r.Path)
	default:
		fmt.Printf("removed:  %s\n", r.Path)
		if r.BranchDeleted {
			fmt.Printf("  branch deleted: %s\n", r.Branch)
		}
	}
}
