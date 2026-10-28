# Plan: Oversized-task boundary fixture

## Tasks

- [ ] A1 nested — agent: claude — owns: internal/foo/sub/a.go, internal/foo/deep/c.go, internal/foo/x/y/z.go
- [ ] A2 token starts — agent: claude — owns: `skills/a/SKILL.md` (agents/b.md) rules/c.md,scripts/d.sh
- [ ] A3 dot-slash — agent: claude — owns: internal/a/x.go — verify: go test ./internal/b/ ./internal/c/ ./internal/d/
- [ ] A4 runner alternatives — agent: claude (codex/copilot) — owns: internal/a/x.go, internal/b/y.go, internal/c/z.go
- [ ] B1 cmd distinct — agent: claude — owns: internal/cli/a.go, cmd/cli/main.go, internal/agent/b.go, internal/hooks/c.go
- [ ] C1 owns at end of line — agent: claude — owns:
- [ ] C2 owns then em dash — agent: claude — owns: — verify: go test ./internal/cli/
- [ ] C3 owns filled — agent: claude — owns: internal/cli/agent.go

```markdown
- [ ] D1 backtick fence — agent: claude
```

~~~
- [ ] D2 tilde fence — agent: claude
```
- [ ] D3 backticks do not close a tilde fence — agent: claude
~~~

- [ ] D4 after the fences — agent: claude
