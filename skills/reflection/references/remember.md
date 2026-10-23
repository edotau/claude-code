# `/reflection:remember` — Type Guidance

> Manual intervention (capture is normally automatic): explicitly save a fact to the repo's memory
> bank. `claude-code memory remember` writes it, stamps an `<!-- id:... -->` marker, and settles —
> deterministically. The one judgment call left is **which `--type` the fact is**, which picks the file.

| type | Use for | Written to |
|------|---------|------------|
| `project` | Project convention, architecture, gotcha (default) | `decisionLog.md` (dated entry, top) |
| `user` | A stable user preference ("prefers explicit error handling") | `conventions.md` (bullet) |
| `feedback` | A correction the user made that should persist | `conventions.md` (bullet) |
| `reference` | A durable external/pointer fact (endpoint, provider name) | `conventions.md` (bullet) |

Honor an explicit `--type` flag; default to `project` when the fact doesn't clearly fit `user`,
`feedback`, or `reference`.

## Don't use for

- Temporary context — just say it in conversation.
- Enforced rules — use `/reflection:promote --to rules`.
- Reusable procedures — use `/reflection:promote --to skill`.
- Secrets — never.

## Next tier

If the fact reads like an imperative rule (always/never, a convention), nudge the user toward
`/reflection:promote --to rules` to graduate it to durable enforcement instead of leaving it as a note.
