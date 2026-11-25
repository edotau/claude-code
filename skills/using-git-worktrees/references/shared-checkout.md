# Shared-Checkout Coordination

Policy lives in [Git and Worktrees](../../../rules/standards/worktree-git.md); the
[skill's enforcement table](../SKILL.md#hook-enforcement-vs-policy) distinguishes the checks.
Use [the lane protocol](../SKILL.md#the-lane-protocol) for isolated streams.

## Claims are warnings, not isolation

`worktree-claim` records edit-tool paths and Git-staged paths per session/leaf. Live ownership, dirty state and newer
commits determine whether a claim still matters. Bash formatters and external editors are not comprehensively
tracked before staging: inspect the diff even when no claim warning appeared.

Peer-file enforcement is in the [skill table](../SKILL.md#hook-enforcement-vs-policy); interactive hunk staging
is exempt. A named-file commit takes the whole working-tree file, not just your hunks. Check both the file diff
and staged content; leave peer hunks to their owner.

Inspect peer sessions with `claude-code agents tree`, dirty paths with `git status --short`, and the selected files
with `git diff HEAD -- <paths>` before committing. An isolated snapshot check avoids a false green from peers'
uncommitted dependencies (verification procedure: the standard's LAST-MILE).

## Notices and queues

Conflict notices go to the session recorded in the lane ledger; `worktree-busy` / `worktree-claim-check` drain
notices on prompts or Git commands. Dirty-target refusals identify claimants; divergence context is advisory, not
a universal commit ban. `lane-land-queue` shows lanes that did not auto-land and their next commands. Read those
reasons rather than assuming a finished leaf was committed or landed.

## Registry coordination is policy

Give shared registries to the orchestrator and return proposed entries with exact file/anchor/text. Common hot
files include `internal/settings/hookspec.go`, the hook handler map, provider defaults and `rules/harness/hooks.md`.
This ownership split is **prose**, not a registry-specific hook refusal. New file writers have package-owned rows
in `internal/schemas/testdata/writers/<pkg>.txt`; do not recreate a central writer-registry bottleneck.
