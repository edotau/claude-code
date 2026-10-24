# Competition Tournament

N leaves attempt the **same** task independently in isolated worktrees; the manager measures and merges one. Use it
when the objective is measurable (latency, size, pass rate, coverage) or the approaches truly diverge. Skip it when
one approach is obviously right, outcomes aren't comparable, or the win is smaller than the N× cost (tokens and rate
limit scale with N; cap N by the concurrency tier).

## Process

1. **Baseline**: run the eval command on the base branch and record the number.
2. **One brief, reused verbatim** for every competitor (same objective, criteria, eval command). To force divergence,
   vary one named constraint per leaf ("no new deps") and say so.
3. **Dispatch in one message**: `code-worker`, `isolation: "worktree"`, branch `compete/<slug>/<n>`; each commits and
   reports its measured number. Comparing model families instead: each competitor is
   `claude-code ask --agent <runner> --format json "<same brief>"` in its own worktree
   ([cost-routing.md](cost-routing.md)).
4. **Re-measure yourself** in each worktree; discard red ones. A self-reported number is a claim.
5. **Rank** on the diff, never the leaf's summary:

   | Mode | Use for | How |
   |---|---|---|
   | Metric | benchmarks, size, pass rate | same eval in each worktree vs baseline |
   | Judge | readability, structure, API shape | `git diff <base>...<branch>`; correctness → simplicity → quality |
   | Hybrid | metric near-ties | metric first, judge leaves within ~10% of the leader |

6. **Merge the winner** (`claude-code worktree merge`) and re-run the gates on the merged tree.
7. **Archive losers** (`git tag compete/<slug>/<n>`) before removing their worktrees.

## Stop conditions

Every leaf red → merge nothing, report the common failure (the task is underspecified, not unlucky). No leaf beats the
baseline → keep the baseline, archive, say what was tried; a tie is not a win. Clean leftover worktrees before
the next run.

Competitors never read each other's branches; never rebase or force-push one; leaves cannot start their own tournament.
