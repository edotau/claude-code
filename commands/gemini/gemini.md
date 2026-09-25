---
description: "Invoke the in-process Gemini bridge for long-context code exploration, analysis, and documentation generation"
allowed-tools: Bash, Glob, Read
argument-hint: "[--model name] [--dirs path,...] [--files pattern,...] [--index] [--diff] [--rubric file] [--format text|json] <task>"
---

# /gemini — Gemini Bridge (native Go)

Use the in-process Gemini bridge for long-context code exploration, architecture
review, documentation synthesis, and structured data analysis. The bridge collects
local context first, then makes one deterministic call to Google's API — all in
native Go (`claude-code gemini bridge`), no node or python at runtime.

## Usage

```bash
/gemini <task>
/gemini --model <name> <task>
/gemini --dirs <path,...> <task>
/gemini --files <pattern,...> <task>
/gemini --index <task>
/gemini --diff <task>
/gemini --rubric <file> <task>
/gemini --format json <task>
```

## Arguments

| Argument | Description | Example |
|----------|-------------|---------|
| `--model <name>` | Gemini model override | `--model opus` or a bare id |
| `--provider <name>` | Provider to run against (default `gemini`) | `--provider gemini` |
| `--task <text>` | Explicit task text instead of trailing args | `--task "summarize the router"` |
| `--dirs <paths>` | Recursively inline directories into the bridge context | `--dirs internal/router,internal/agent` |
| `--files <pattern,...>` | Inline matching files into the bridge context | `--files "internal/models/*.go"` |
| `--index` | Append a repo skip/keep index to the inlined bodies | `--index` |
| `--max-files <n>` | Cap the number of files inlined | `--max-files 200` |
| `--max-file-bytes <n>` | Cap bytes inlined per file | `--max-file-bytes 65536` |
| `--diff` | Scope context to files changed vs `HEAD` | `--diff` |
| `--rubric <file>` | Prefix the task with a rubric file's contents | `--rubric skills/simplicity/SKILL.md` |
| `--effort <low\|medium\|high>` | Reasoning effort | `--effort high` |
| `--format <type>` | Output format (`text`, `json`) | `--format json` |
| `--timeout <dur>` | Per-attempt deadline (default `10m`) | `--timeout 5m` |
| `<task>` | Analysis task or question | (required) |

## Execution Instructions

Parse arguments into:

1. `MODEL` from `--model` if present
2. `DIRS` from `--dirs` if present
3. `FILES` from `--files` if present
4. `FORMAT` from `--format` if present, otherwise `text`
5. `TASK` from the remaining text

Always execute through the native bridge command:

```bash
claude-code gemini bridge [--model <MODEL>] [--dirs <DIRS>] [--files <FILES>] [--index] [--diff] [--rubric <FILE>] [--format <FORMAT>] -- "<TASK>"
```

Guidance:
- Routing is always the in-process Google API path (`GEMINI_API_KEY`, no vendor CLI, no node/python).
- Use `--dirs` for broad package or module areas.
- Use `--files` for precise globs or structured data slices.
- `--index` appends a path index to the inlined bodies; pair with `--max-files 0` for an index-only run with no bodies.
- Use `--format json` only when the caller explicitly wants machine-readable output.
- Keep the task direct, scoped, and explicit about the output shape.

## Examples

### Simple query

```bash
/gemini what is 2+2
```

### Architecture review

```bash
/gemini --dirs internal,docs explain the architecture of this codebase
```

### Package deep-dive

```bash
/gemini --dirs internal/router,internal/agent how does a request flow from vendor CLI to provider call
```

### Structured data review

```bash
/gemini --files "internal/providers/*.json" summarize the provider schema and highlight breaking changes
```

### Model override

```bash
/gemini --model opus --dirs internal/crossgen analyze the refactor impact of renaming Sync
```

### JSON output

```bash
/gemini --format json --dirs internal/cli summarize the public API surface
```

## Best Use Cases

Gemini fits:
- whole-codebase architecture understanding
- cross-file consistency audits
- refactoring impact analysis
- unfamiliar codebase orientation
- documentation generation
- structured text data synthesis

Gemini is not the right tool for:
- quick single-file edits
- tight interactive debugging loops
- trivial tasks with no cross-file or data-shape component

For multi-turn analyses with follow-ups, dispatch the `gemini` agent (Gemini
backend) via the `Agent` tool instead of chaining `/gemini` calls.

## Error Handling

| Error | Solution |
|-------|----------|
| No credential | Set `GEMINI_API_KEY` in `env.d/secrets.env` |
| HTTP 404 model | The model id isn't served. Run `claude-code models --live --provider gemini` and pick a listed id. |
| HTTP 400 input token limit | Narrow the inlined scope with `--dirs`/`--files`, lower `--max-files`, or add `--index` instead of full file bodies |
| HTTP 429 | The runner retries honouring `Retry-After` — no action needed unless it persists |
| Timeout | Reduce the context set and tighten the task, or raise `--timeout` (default `10m`) |

## Prompt scaffolds (token-efficient `<TASK>` shapes)

Keep the bridge context lean — reference files/lines, don't paste large snippets.

- **Analysis / Plan** (no code changes): `Task: <what to analyze>` · `Repo pointers: <paths + line numbers>` · `Constraints: concise, actionable, reference files not snippets` · `Output: bullet findings + proposed plan`.
- **Patch** (unified diff only): `Task: <what to change>` · `Repo pointers: <paths + lines>` · `Constraints: OUTPUT unified diff ONLY, no actual edits, minimal focused changes` · `Output: a single unified diff`.
- **Review** (audit a diff): `Task: review the diff for correctness, edge cases, missing tests` · `Constraints: checklist of issues + fixes, no code unless asked` · then paste the diff.

## Notes

- Native command: `claude-code gemini bridge` — in-process Go, no node/python.
- **Routing**: one chat-completions call to Google's API via the `gemini` provider (registry entry,
  `kind: openai`), authenticated with `GEMINI_API_KEY` from `env.d/secrets.env`. No vendor CLI at
  runtime, so a broken `gemini` install can't take the bridge offline.
- Model tiers: `opus` = `gemini-3.1-pro-preview` (reasoning; use for second-opinion review),
  `sonnet` = `gemini-3.6-flash` (default), `haiku` = `gemini-3.5-flash-lite`.
- Model resolution: `--model` → `CLAUDE_CODE_GEMINI_MODEL` → `GEMINI_MODEL` → the provider's sonnet
  tier. `--model` accepts a tier name (`opus`/`sonnet`/`haiku`) or a bare model id.
- `--rubric <file>` takes a file path (e.g. `skills/simplicity/SKILL.md`), not a skill name.
- Related: the `gemini` agent's Gemini backend wraps this bridge in a structured-findings format;
  `claude-code gemini ask "<task>"` drives the vendor `gemini` CLI directly instead of the bridge.
