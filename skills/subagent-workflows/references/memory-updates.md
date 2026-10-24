# Proposed Memory Updates

After finishing, if you learned something reusable, append a **Proposed Memory Updates** section
to your final report. Do NOT write memory files yourself, and never touch the approval-gated
RULES region — the parent decides what gets persisted. Frame each as topic-file content plus its
one-line `MEMORY.md` TOC entry, with a rationale, keeping `MEMORY.md` a lean index.

## Codex

Candidate content: frequently missed constraints, common task patterns worth flagging,
project-specific evaluation criteria, baseline scores worth anchoring, build/test quirks
(flaky tests, build order, platform differences), common skill-quality issues,
triggering-description improvements, recurring manifest failures.

## Opencode

Candidate content: a staleness signal unique to this harness, a naming convention that worked, a
reliable documentation source, an effective search query.

## Copilot

Candidate content, durable facts only: a fixture whose contract is easy to misread, a suite whose
failure output is misleading, a repo whose merge order is non-obvious, a gate that only fails
post-merge. Skip anything the diff or git history already records.

## Gemini

Candidate content, durable facts only: an agy sign-in or model-id quirk (a 404 on an id `models --live` no
longer lists), a `--dirs`/`--max-files` budget that fit a repo in one bridge call, a web query shape that returned
citable URLs, a `file:line` the spot-check refuted. Skip anything the bridge's own error table already covers.
