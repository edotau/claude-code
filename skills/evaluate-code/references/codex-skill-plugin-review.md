# Skill and Plugin Review Procedures

Any reviewer may run these; Codex dispatch:
`claude-code agents ask --overlay '{"agent":"codex"}' "<mode below + artifact path>"`.
Routing: `skills/subagent-workflows/references/agents-and-models.md` → Codex task guidance.

## SKILL mode

1. Read frontmatter/body and `references/`, `examples/`, `scripts/`.
2. Check YAML between `---`, required `name`/`description`, substantial body.
3. Assess trigger coverage, content quality, progressive disclosure and supporting files' quality/relevance/correctness.
4. Flag vague/missing triggers, second-person description, excess body content, missing useful examples/references.
5. Classify critical/major/minor = CRITICAL/HIGH/MEDIUM+LOW (`completion-criteria.md`).
6. Recommend concrete fixes by impact; show before/after when helpful.

### SKILL Output Format

**Skill Review: [name]**, with summary/word counts, description analysis (current, issues, improved wording),
content quality (style/organization), progressive disclosure (body words, reference/example/script counts),
issues by critical/major/minor (counts, file/location, fix), positive aspects and priority recommendations.
Overall rating: **Pass / Needs Improvement / Needs Major Revision**.

Clean description: focus on content. Over 5,000 words: recommend references. Missing referenced files: report paths.

## PLUGIN mode

1. Locate root from manifest; validate JSON with `rtk jq` or Read. Require kebab-case `name`; unknown fields only warn.
2. Glob `commands/`, `agents/`, `skills/`, `hooks/`; check layout against auto-discovery.
3. Commands (`commands/**/*.md`): frontmatter, `description`, `argument-hint` format, optional `allowed-tools` array,
   body and naming conflicts.
4. Agents (`agents/*.md`, flat): `name`/`description`/`model`; lowercase-hyphen name, 3–50 chars, matching file stem;
   plain model alias (`inherit`/`sonnet`/`opus`/`haiku`); substantial prompt.
5. Skills (`skills/*/SKILL.md`): file exists, `name`/`description`, concise description, valid referenced files.
6. Hooks: valid JSON/config, events and scripts. Security: no hardcoded credentials, unsafe hooks or example secrets.
7. Organization: README, no cruft (`node_modules`, `.DS_Store`), .gitignore where needed.

### PLUGIN Output Format

**Plugin Validation Report**: name/location, summary (pass/fail, stats), critical issues and warnings
(counts, path, issue, fix), component counts/validity (commands, agents, skills, hooks), positive findings,
recommendations and overall **PASS/FAIL** with reasoning.

A correct manifest-only plugin is valid. Empty directories/unknown fields warn, never fail.
Group errors by file; report corrupted files, skip them and continue.
