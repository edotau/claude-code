package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/outfmt"
	"github.com/edotau/claude-code/internal/worktree"
)

const worktreeUsage = `usage: claude-code worktree list [--format text|json]
       claude-code worktree create <name> [--branch <b>]
       claude-code worktree merge [--dry-run] [--target <ref>] [--no-clean] <name>
       claude-code worktree clean [--dry-run] <name>
       claude-code worktree clean-all [--dry-run|--yes]
`

func cmdWorktree(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, worktreeUsage)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail("worktree: %v", err)
	}
	repo, err := worktree.ResolveRepo(cwd)
	if err != nil {
		return fail("worktree: %v", err)
	}
	sub, rest := args[0], args[1:]
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
		fmt.Fprint(os.Stderr, worktreeUsage)
		return 2
	}
}

func cmdWorktreeList(repo *worktree.Repo, args []string) int {
	fs := flag.NewFlagSet("worktree list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	format := outfmt.Flag(fs)
	if fs.Parse(args) != nil {
		return 2
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
	fmt.Printf("  reopen:  cd %q\n", e.Path)
	if eligibleForClean(e) {
		fmt.Printf("  cleanup: claude-code worktree clean %q\n", name)
	}
}

// eligibleForClean mirrors the bar Clean itself rechecks live; list only offers the command, never acts.
func eligibleForClean(e worktree.Entry) bool {
	return e.Exists && !e.Orphaned && !e.Locked && !e.Dirty && e.Merged
}

func cmdWorktreeCreate(repo *worktree.Repo, args []string) int {
	fs := flag.NewFlagSet("worktree create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	branch := fs.String("branch", "", "branch to use (default worktree-<name>; reused if it already exists)")
	if fs.Parse(args) != nil {
		return 2
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
// before anything mutates. Apply then runs IntegrateTarget -> Promote -> an optional Clean.
func cmdWorktreeMerge(repo *worktree.Repo, args []string) int {
	fs := flag.NewFlagSet("worktree merge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "preview the plan, including predicted conflicts, without integrating or promoting")
	target := fs.String("target", "", "require the main checkout to be on this branch (default: whatever it's on)")
	noClean := fs.Bool("no-clean", false, "leave the worktree on disk after a successful promote")
	if fs.Parse(args) != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail("worktree merge: give exactly one name")
	}
	name := rest[0]

	plan, err := worktree.Preflight(repo, name, *target)
	if err != nil {
		return fail("worktree merge: %v", err)
	}
	printMergePlan(plan)
	if len(plan.Issues) > 0 {
		return 1
	}
	if *dryRun {
		return 0
	}

	integrated, err := worktree.IntegrateTarget(repo, plan)
	if err != nil {
		return fail("worktree merge: integrate: %v", err)
	}
	fmt.Printf("integrated: %s -> %s\n", shortOID(integrated.BeforeOID), shortOID(integrated.AfterOID))

	promoted, err := worktree.Promote(repo, plan)
	if err != nil {
		if promoted != nil && promoted.ResumeHint != "" {
			fmt.Fprintln(os.Stderr, "  retry:", promoted.ResumeHint)
		}
		return fail("worktree merge: promote: %v", err)
	}
	fmt.Printf("promoted:   %s -> %s on %s\n", shortOID(promoted.BeforeOID), shortOID(promoted.AfterOID), plan.TargetRef)

	if *noClean {
		fmt.Printf("worktree left on disk; clean up with: claude-code worktree clean %s\n", name)
		return 0
	}
	result, err := worktree.Clean(repo, name, false)
	if err != nil {
		return fail("worktree merge: clean: %v", err)
	}
	printCleanResult(result, false)
	return 0
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
	fs := flag.NewFlagSet("worktree clean", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "preview the recheck without removing anything")
	if fs.Parse(args) != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail("worktree clean: give exactly one name")
	}
	result, err := worktree.Clean(repo, rest[0], *dryRun)
	if err != nil {
		return fail("worktree clean: %v", err)
	}
	printCleanResult(result, *dryRun)
	if !result.Removed {
		return 1
	}
	return 0
}

func cmdWorktreeCleanAll(repo *worktree.Repo, args []string) int {
	fs := flag.NewFlagSet("worktree clean-all", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dryRun := fs.Bool("dry-run", false, "preview without removing anything")
	yes := fs.Bool("yes", false, "required to actually remove every eligible worktree")
	if fs.Parse(args) != nil {
		return 2
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
