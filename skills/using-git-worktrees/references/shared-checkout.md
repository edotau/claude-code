# Shared-Checkout Coordination

Policy: [Git and Worktrees](../../../rules/standards/worktree-git.md#worktrees).

## Claims are warnings, not isolation

`worktree-claim` records edit-tool paths and Git-staged paths per session/leaf. Live ownership, dirty state and newer
commits determine whether a claim still matters. Bash formatters and external editors are not comprehensively
tracked before staging: inspect the diff even when no claim warning appeared.

Claim checks advise, never block staging or commits. A named-file commit takes the whole working-tree file,
not just your hunks. Check the file diff and staged content; leave peer hunks to their owner.

Inspect peer sessions with `claude-code agents tree`, dirty paths with `rtk git status --short`, and the selected files
with `rtk git diff HEAD -- <paths>` before committing. An isolated snapshot check avoids a false green from peers'
uncommitted dependencies (verification procedure: the standard's LAST-MILE).

## Notices and queues

Conflict notices go to the session recorded in the lane ledger; `worktree-busy` and `worktree-claim-check`
drain them. Read the notices and actual diff; never infer collection or landing from a leaf's completion.
Collection: `rules/standards/worktree-git.md` → Worktrees.

## Registry coordination is policy

Give shared registries to the orchestrator and return proposed entries with exact file/anchor/text. Common hot
files include `internal/platform/settings/hookspec.go`, the hook handler map, provider defaults and `rules/harness/hooks.md`.
This ownership split is **prose**, not a registry-specific hook refusal. New file writers have package-owned rows
in `internal/platform/schemas/testdata/writers/<pkg>.txt`; do not recreate a central writer-registry bottleneck.
