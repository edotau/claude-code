# Plan: Improve parsing

## Context

Parse phases as complete steps.

## Phases

### Phase 1 — Parse input

**Files:** `input.go`
**Change:**
1. Read the input.
2. Preserve section bodies.
**Verify:**
- `go test ./internal/paths` exits 0.
**Runner / parallel:** runner: codex; independent.

### Phase 2 — Improve output

**Files:** `output.go`
**Change:** Update rendering.
**Verify:** should work
**Runner / parallel:** runner: codex; depends on Phase 1.

### Phase 3 — Run final regression

**Files:** `output_test.go`
**Change:** Exercise rendering.
**Verify:**
- `go test ./internal/review/plan` exits 0.
**Runner / parallel:** runner: codex; depends on Phase 2.

## Tasks

- [x] Phase 1 — Parse input
- [ ] Phase 2 — Improve output
- [ ] Phase 3 — Run final regression

## Risks

- Delivery should work; confirm manually.
