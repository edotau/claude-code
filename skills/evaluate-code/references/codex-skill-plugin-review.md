# Skill and Plugin Review Procedures

Any reviewer runs these; on codex via `claude-code codex ask "<the mode section below + the artifact path>"`
(`agents/codex.md` → Skill or plugin audit).

## SKILL mode

1. **Read** the SKILL.md (frontmatter + body) and any `references/`, `examples/`, `scripts/`.
2. **Validate structure.** YAML frontmatter between `---`; required `name`, `description`; substantial body.
3. **Apply criteria** — description triggering, content quality, progressive disclosure.
4. **Review supporting files** for quality, relevance, correctness.
5. **Categorize issues** critical/major/minor. Anti-patterns: vague trigger descriptions, too much in SKILL.md
   (belongs in `references/`), second person in the description, missing key triggers, no examples/references
   where they'd help.
6. **Recommend** specific fixes (before/after when helpful), prioritized by impact.

### SKILL Output Format

```text
## Skill Review: [skill-name]

### Summary            [overall assessment + word counts]
### Description Analysis
**Current:** … / **Issues:** … / **Recommendations:** (incl. suggested improved description)
### Content Quality    [word count, writing style, organization]
### Progressive Disclosure   [SKILL.md words; references/examples/scripts counts; effective?]
### Specific Issues
#### Critical ([n]) / #### Major ([n]) / #### Minor ([n])   — [file/location]: [issue] - [fix]
### Positive Aspects
### Overall Rating     [Pass / Needs Improvement / Needs Major Revision]
### Priority Recommendations   1. … 2. … 3. …
```

Edge cases: clean description → focus on content; >5,000 words → recommend splitting into `references/`; missing
referenced files → report with paths.

## PLUGIN mode

1. **Locate the plugin root** via its manifest.
2. **Manifest.** Valid JSON (`jq` or Read); required `name` (kebab-case); unknown fields warn, never fail.
3. **Directory structure.** `Glob` component dirs (`commands/`, `agents/`, `skills/`, `hooks/`); layout matches
   auto-discovery.
4. **Commands** (`commands/**/*.md`): frontmatter, `description`, `argument-hint` format, `allowed-tools` an array if
   present, markdown body; naming conflicts.
5. **Agents** (`agents/*.md`, flat): frontmatter `name`/`description`/`model`; name lowercase-hyphen 3–50 chars
   equal to the file stem; model a plain alias (`inherit`/`sonnet`/`opus`/`haiku`); substantial system prompt.
6. **Skills** (`skills/*/SKILL.md`): exists; frontmatter `name`/`description`; concise description; referenced
   files exist.
7. **Hooks**: valid JSON/config and event names; commands reference existing scripts.
8. **Organization**: README present; no cruft (node_modules, .DS_Store); .gitignore if needed.
9. **Security**: no hardcoded credentials, hook security issues, or secrets in examples.

### PLUGIN Output Format

```text
## Plugin Validation Report

### Plugin: [name]   Location: [path]
### Summary            [pass/fail + key stats]
### Critical Issues ([n])   - `file/path` - [issue] - [fix]
### Warnings ([n])          - `file/path` - [issue] - [recommendation]
### Component Summary  [commands/agents/skills found & valid; hooks]
### Positive Findings
### Recommendations    1. … 2. …
### Overall Assessment [PASS/FAIL] - [reasoning]
```

Edge cases: a manifest-only plugin is valid if the manifest is correct; empty dirs and unknown manifest fields warn,
never fail; group multiple errors by file; skip corrupted files, report them and continue.
