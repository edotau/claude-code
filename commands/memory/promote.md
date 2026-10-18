---
description: "Graduate durable, cross-project lessons from a repo's local memory bank into the shared global bank."
---

# /memory:promote — Graduate Local Bank Lessons to the Global Store

Two scopes in one store: this repo's dir (`~/.claude/memory/<repo>/`) and the **shared global**
store (`~/.claude/memory/`, the tree TOP — used by any non-repo cwd and reachable from every
session via the catalog, `memory search --all`, and cross-repo routing). Settle never promotes on
its own — over-cap overflow archives within the repo's own `archive/`; `claude-code memory promote`
is the explicit, deduped mechanical move. This command is the **judgment** step: look at what has
settled in this repo's dir and decide whether anything in it is durable enough, and general enough,
to belong in the global store (or, if near-rule and public-safe, in tracked `docs/knowledge/`).

## Procedure

1. **Resolve the store** — run `claude-code memory path` to get this repo's dir; the global dir
   is the store top (`claude-code memory path` from any non-repo cwd). Never hardcode either.
2. **Read the local dir** — `conventions.md` and `decisionLog.md` are the two append-only files
   that accumulate durable lessons; `activeContext.md`/`progress.md`/`sessionHistory.md` are
   session state and never promote.
3. **Judge each candidate entry** — promote ONLY if it is:
   - **durable + cross-project** — a truth that holds beyond this one repo (not project state
     like "this repo's build is flaky", which stays local);
   - **not already** present in the global bank (`grep` it there first).
   Drop anything project-specific, one-off, or already captured globally.
4. **Run the promote verb**:
   ```bash
   claude-code memory promote
   ```
   For entries that qualify by judgment but haven't hit the cap yet, move them by hand: append
   to the global `conventions.md`/`decisionLog.md`, then strike the local entry (supersede, never
   silently delete — leave a one-line pointer if the local file still needs the context).
5. **Regenerate the routing index**:
   ```bash
   claude-code memory index
   ```
6. **Report**:
   ```
   Promoted N entries from <repo-slug> → global bank:
     - <entry title/first 80 chars>
   Index regenerated: yes
   ```

## Promote when / don't

- **Promote** when the lesson would help a session in a *different* repo, or is infrastructure
  doctrine (a provider quirk, a router behavior, a hook contract) rather than this repo's state.
- **Don't** promote anything still mid-change, or specific to this repo's own code — that belongs
  in `rules/` (via `/reflection:promote`) or stays in the local bank.
