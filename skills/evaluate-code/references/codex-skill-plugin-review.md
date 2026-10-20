# Codex Skill and Plugin Review Procedures

## SKILL mode

1. **Locate and read.** Find the SKILL.md (user indicates path). Read frontmatter + body; check for
   `references/`, `examples/`, `scripts/`.
2. **Validate structure.** YAML frontmatter between `---`; required `name`, `description`; substantial body.
3. **Apply review criteria** — description triggering, content quality, progressive disclosure.
4. **Review supporting files** (if present) for quality, relevance, and correctness.
5. **Identify issues**, categorized critical/major/minor. Anti-patterns: vague trigger descriptions,
   too much content in SKILL.md (belongs in `references/`), second person in the description,
   missing key triggers, no examples/references when they'd help.
6. **Recommend** specific fixes, before/after examples when helpful, prioritized by impact.

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

### SKILL edge cases

Clean description → focus on content/organization. >5,000 words → strongly recommend splitting into
`references/`. Minimal new skill → constructive building guidance. Missing referenced files →
report with paths.

---

## PLUGIN mode

1. **Locate plugin root.** Check for the plugin manifest file; note location.
2. **Validate manifest.** JSON syntax (`jq` or Read); required `name` (kebab-case); warn (don't fail) on unknown fields.
3. **Validate directory structure.** `Glob` component dirs (`commands/`, `agents/`, `skills/`,
   `hooks/`); confirm layout/auto-discovery match expected locations.
4. **Validate commands** (`commands/**/*.md`): frontmatter present, `description` exists,
   `argument-hint` format, `allowed-tools` is an array if present, markdown body exists; check
   naming conflicts.
5. **Validate agents** (`agents/*.md`, flat): frontmatter `name`/`description`/`model`; name
   lowercase-hyphen 3–50 chars and equal to the file stem; model is a plain alias
   (`inherit`/`sonnet`/`opus`/`haiku`); substantial system prompt.
6. **Validate skills** (`skills/*/SKILL.md`): SKILL.md exists; frontmatter `name`/`description`;
   concise description; referenced files exist.
7. **Validate hooks**: valid JSON/config; valid event names; commands reference existing scripts.
8. **File organization**: README present/comprehensive; no cruft (node_modules, .DS_Store); .gitignore if needed.
9. **Security**: no hardcoded credentials anywhere; no obvious hook security issues; no secrets in examples.

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

### PLUGIN edge cases

Minimal plugin (just the manifest) → valid if manifest correct. Empty dirs / unknown manifest
fields → warn, don't fail. Multiple errors → group by file, prioritize critical. Plugin not found →
clear error + guidance. Corrupted files → skip, report, continue.
