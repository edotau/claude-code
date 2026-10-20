# Plan: Oversized-task boundary fixture

## Tasks

- [ ] A1 nested — runner: claude — owns: internal/foo/sub/a.go, internal/foo/deep/c.go, internal/foo/x/y/z.go
- [ ] A2 token starts — runner: claude — owns: `skills/a/SKILL.md` (agents/b.md) rules/c.md,scripts/d.sh
- [ ] A3 dot-slash — runner: claude — owns: internal/a/x.go — verify: go test ./internal/b/ ./internal/c/ ./internal/d/
- [ ] A4 runner alternatives — runner: claude (codex/copilot) — owns: internal/a/x.go, internal/b/y.go, internal/c/z.go
- [ ] B1 cmd distinct — runner: claude — owns: internal/cli/a.go, cmd/cli/main.go, internal/agent/b.go, internal/hooks/c.go
- [ ] C1 owns at end of line — runner: claude — owns:
- [ ] C2 owns then em dash — runner: claude — owns: — verify: go test ./internal/cli/
- [ ] C3 owns filled — runner: claude — owns: internal/cli/agent.go

```markdown
- [ ] D1 backtick fence — runner: claude
```

~~~
- [ ] D2 tilde fence — runner: claude
```
- [ ] D3 backticks do not close a tilde fence — runner: claude
~~~

- [ ] D4 after the fences — runner: claude
