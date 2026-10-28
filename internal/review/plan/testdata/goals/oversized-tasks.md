# Plan: Oversized tasks fixture

## Phases

### Phase 1 — Fix many packages at once

**Verify:** go test ./internal/cli

## Tasks

- [ ] Phase 1 — Touch internal/cli, internal/launch, internal/configure and internal/clipboard in one pass
- [ ] Phase 2 — Fix 31 findings across the hooks package — agent: copilot — owns: internal/hooks/safety.go
- [ ] Phase 3 — Split cmdMemory, cmdVSCode, cmdAsk and cmdSettings — agent: code-worker
- [ ] Phase 4 — Add --attach to the copilot runner — agent: claude — owns: internal/agent/vendor.go
