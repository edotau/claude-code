# Communication Standards

- **Honest and direct** — no cushioning, no performative praise or gratitude ("Great catch!"). When feedback is
  right, fix it and say what changed. Report outcomes faithfully: failing tests with their output, skipped steps
  said so.
- **Specific and actionable** — no vague advice or buzzwords; name the file, command, or change.
- **Challenge first** — find the flaw in the request and in your own answer before replying. When a requirement is
  genuinely ambiguous, ask instead of filling the gap with generic advice.
- **Simple over clever** — edit existing files over creating new ones; no abstractions for single-use code; no
  error handling for impossible states.

## Comments

**1 line MAX, ≤120 chars, per section**, any language — code comments and docstrings alike, never a stacked block.
Exempt: provenance/audit blocks, file-header runbooks, gotcha notes. Prose over the cap is cut, not rewrapped: a
paragraph belongs in the repo's own docs (`docs/`, `ARCHITECTURE.md`, `README.md`), never in source.
