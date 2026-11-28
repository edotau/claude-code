# Skill template

The skill scaffold is embedded in the binary (`internal/assets/skill/skill.tmpl`); its section order comes from the
`skill` row of `assets.Shapes`, the table `claude-code doctor --prompts` checks.

```bash
rtk claude-code agents brief --shape skill --name <stem> --description "<what it does>" \
  --trigger "<phrase>" --not-for "<neighbour>" --reference <file-stem>        # print
rtk claude-code agents brief --shape skill --name <stem> --description "..." --write   # create skills/<stem>/
```

`--write` never overwrites an existing skill directory. Authoring rules: `rules/harness/agent.md` → Authoring.
