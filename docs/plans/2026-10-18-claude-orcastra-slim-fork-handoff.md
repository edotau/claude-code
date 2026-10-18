# claude-orcastra — slim fork handoff (2026-10-18)

Repo: `/Users/edotau/claude-orcastra`. Goal: shrink the packaged app (`dist/mac-arm64/Orca.app` = 702 MB) and its
surface area. Build speed is done. Long term the fork may go Go-native (merge into the claude-code harness) or become
a VS Code extension, so prefer small, self-contained fork modules over deep Electron rewrites.

## Done on `main`

| Commit | What |
| --- | --- |
| `af0c9d442a` | `make build` = host arch only (mac `--dir` .app, Linux/WSL AppImage, Windows NSIS); `make package` = full installer set; Windows (MINGW/MSYS) + WSL detection; `make doctor` prints target. 2m35s → 54s warm. |
| `b375d544bc` | Hide (not delete) Linear, GitLab, Gitea, Azure DevOps, automations, skills sharing, plugins via `src/shared/fork-hidden-features.ts` (vitest setup mocks it `false`). |

`make build` on main verified green after both (1m33s incl. typecheck). `make install-app` not run (replaces the
running app) and no packaged-app smoke launch was done (a second instance would focus the running Orca).

## Why things are hidden, not deleted

Upstream Orca is deeply entangled: removing Gitea + Azure alone touched 192 files (per-provider `linked<Host>PR`
fields through metadata, IPC, store, sidebar, mobile); automations/skills/plugins ~830 files; deleting 12 unused
agents left ~800 typecheck errors after the shared layer alone (branch discarded). Feature code is also a tiny share
of app size — the bytes are bundled artifacts (below). Delete only siloed things; hide the rest.

## Remaining work, in priority order

### 1. Low-effort size cuts — DONE
- `fc8d509912`: English-only Electron locales, win32 ripgrep only in Windows builds, no feature-wall GIFs → app
  702 → 661 MB (macOS arm64).
- `33194c146a`: only 16 allowlisted agents appear in pickers/catalog/auto-default (`isForkHiddenTuiAgent` in
  `src/shared/fork-hidden-features.ts`); 29 others stay launchable by id; new upstream agents default to hidden.
  Not gated: `orchestration-skill-coverage.ts` (feature-wall orchestration status page sorts detected agents).
  `mobile/vitest.config.ts` has no fork mock — rerun mobile specs after policy changes.
- Still pending: drop `posthog-node` and `serve-sim` once the cruft branch removes telemetry and the emulator.

### 2. Finish the cruft branch (`strip/cruft`, worktree `/Users/edotau/claude-orcastra-wt/cruft`)
WIP commit `d734a0b87f` (196 files, −121k lines, mostly 5 locale catalogs). Agent stopped mid-pass; **typecheck never
ran**. Done: star-nag + local-builds deleted (updater test harness adjusted), PostHog transport a no-op, ja/ko/fr/es/zh
catalogs deleted, language-picker edits in progress. Not started: foreign-sqlite-readers, theme importers (warp/
ghostty; keep `src/shared/app-theme-presets/*.vscode-theme.json`, used by the Monaco theme), Android emulator,
ephemeral VMs.
1. Typecheck in the worktree: `node config/scripts/run-typecheck-projects-in-parallel.mjs` (node_modules are symlinks
   there so `pnpm run` fails; cold ~7 min, ~13 GB RAM — one at a time on this 18 GB machine).
2. Fix to green; finish or drop the unstarted items — first check each isn't used by a kept feature
   (foreign-sqlite-readers may back browser cookie import / OpenCode session reading).
3. A saved non-English UI language must fall back to English without crashing.
4. Scoped vitest, `node config/scripts/check-changed-code-quality.mjs`, squash the WIP commit, merge to main.
Worktree symlinks (recreate if missing): node_modules, mobile/node_modules, src/packages/process-host/node_modules,
native/windows-{security,registry}/node_modules → main tree.

### 3. Bigger size items (decide later)
| Item | Size | Note |
| --- | --- | --- |
| Browser web client `out/web` | 64 MB | Copy of the renderer served for opening Orca in a desktop browser (`src/main/runtime/rpc/static-web-client-handler.ts`). Phone uses `mobile-web` (8 MB). Drop, or serve from the renderer copy. |
| `sherpa-onnx` (dictation) | 38 MB | Remove dictation. |
| Terminal Host helper | 127 MB | Node runtime copy run via launchd for TCC; read `docs/reference/macos-daemon-tcc-attribution.md` first. |
| Electron Framework | 278 MB | Fixed cost. |

Small gaps from the hide pass: `--linear-issue` / `--gitlab-*` flags remain on `orca worktree create/set`;
`getForgeProviderById('gitlab')` now falls back to the GitHub provider for old GitLab-linked worktrees.

## Verify after each step
- `make build` (or `SKIP_TYPECHECK=1 make build`), then `du -sh dist/mac-arm64/Orca.app`.
- `make install-app`, Cmd+Q Orca, relaunch; check terminal, worktree create, editor, git diff, Claude launch.
- Tests: `node node_modules/vitest/vitest.mjs run --config config/vitest.config.ts <paths>` (bun not installed).
- Full `pnpm test` has not been run in this effort.
