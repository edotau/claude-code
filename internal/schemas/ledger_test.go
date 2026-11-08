package schemas_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/audit"
	_ "github.com/edotau/claude-code/internal/cli"
	_ "github.com/edotau/claude-code/internal/hooks"
	_ "github.com/edotau/claude-code/internal/launch"
	_ "github.com/edotau/claude-code/internal/memory"
	_ "github.com/edotau/claude-code/internal/network/vscode"
	_ "github.com/edotau/claude-code/internal/providers"
	_ "github.com/edotau/claude-code/internal/proxy"
	_ "github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	_ "github.com/edotau/claude-code/internal/setup"
	_ "github.com/edotau/claude-code/internal/tokenledger"
	_ "github.com/edotau/claude-code/internal/usage"
	_ "github.com/edotau/claude-code/internal/vault"
	_ "github.com/edotau/claude-code/internal/worktree"
)

func unversioned(reason string) string { return "unversioned: " + reason }

var writers = map[string]string{
	"internal/codex/codex.go:codexSchemaFile":                         unversioned("foreign: codex --output-schema temp file"),
	"internal/agent/job/record.go:Save":                               "dispatch-record",
	"internal/providers/lastprofile.go:writeLastProfiles":             "last-profile",
	"internal/providers/lastprofile.go:loadLastProfiles":              unversioned("backup: the corrupt last-profile.json copied aside before it is replaced"),
	"internal/agentconfig/overlay_debug.go:appendOverlayDebug":        unversioned("debug: COWORKER_OVERLAY_DEBUG env names + HMAC fingerprints, no values"),
	"internal/agentconfig/overlay_debug.go:overlayDebugSalt":          unversioned("debug: the generated fingerprint salt beside the --overlay-debug file"),
	"internal/agent/client/pack_prompt.go:write":                      unversioned("foreign: the packed context file an agy run reads, removed after the run"),
	"internal/agent/job/record.go:claimRecord":                        unversioned("claim: the running record, linked in whole"),
	"internal/agent/wire/log.go:openLocked":                           unversioned("log: per-job dispatch log, humans only"),
	"internal/agent/wire/event.go:NewEventLog":                        "dispatch-events",
	"internal/agent/job/live_slot.go:AcquireLiveSlot":                 unversioned("lock: live-session slot file, flock only, no content"),
	"internal/copilot/copilot.go:createExclusive":                     unversioned("foreign: handoff markdown in the repo"),
	"internal/audit/audit.go:Log":                                     "audit",
	"internal/network/tailscale/machines_cache.go:WriteMachinesCache": "tailscale-machines",
	"internal/cli/helpsvg.go:cmdMenuSVG":                              unversioned("foreign: -o output"),
	"internal/ui/theme/look.go:SetGradient":                           unversioned("foreign: theme file in a vendor home"),
	"internal/network/network_browser.go:networkBrowserExtract":       unversioned("foreign: --extract -o output"),
	"internal/clipboard/clipboard.go:grabMac":                         unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/clipboard.go:savePNG":                         unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/ssh_bridge.go:installLaunchAgentAt":           unversioned("foreign: launchd plist (Apple format)"),
	"internal/clipboard/wlproxy.go:installProxyAt":                    unversioned("foreign: wl-paste shim binary"),
	"internal/completion/completion.go:Install":                       unversioned("foreign: shell completion script in the user's shell dirs"),
	"internal/vault/vaultcmd/vault_storage.go:writeEditorScratch":     unversioned("foreign: editor scratch temp file"),
	"internal/configure/home.go:RegisterCodexMCP":                     unversioned("foreign: codex config.toml in the vendor home"),
	"internal/agentconfig/project/sync.go:writeProjection":            "projections",
	"internal/databricks/config.go:writeScratch":                      unversioned("foreign: temp rescue config"),
	"internal/databricks/export.go:mergeInto":                         unversioned("foreign: export --merge into ~/.databrickscfg"),
	"internal/databricks/export.go:writeFile0600":                     unversioned("foreign: -o export output"),
	"internal/desktop/transaction.go:restoreFile":                     "desktop-state",
	"internal/codex/permissions.go:writeRetargetedConfig":             unversioned("foreign: per-dispatch CODEX_HOME config.toml retargeted from the user's, removed after the run"),
	"internal/codex/permissions.go:saveRefreshedAuth":                 unversioned("foreign: codex auth.json refreshed during a dispatch, copied back to the real CODEX_HOME"),
	"internal/clipboard/ssh_bridge.go:logServed":                      unversioned("log: one line per Mac file served over the clip bridge (path, never bytes)"),
	"internal/clipboard/ssh_bridge.go:fetchFrom":                      unversioned("cache: a Mac file fetched over the clip bridge into /tmp/claude-drop"),
	"internal/codex/permissions.go:writeRulesDir":                     unversioned("foreign: per-dispatch CODEX_HOME rules dir and execpolicy file, removed after the run"),
	"internal/codex/settings.go:applyCodexRestore":                    unversioned("foreign: codex config.toml revert in the vendor home"),
	"internal/codex/settings.go:writeCodexConfigLocked":               unversioned("foreign: codex config.toml in the vendor home"),
	"internal/agentconfig/render/render.go:CommitOverlayFile":         unversioned("foreign: generated vendor overlay, vendor-owned shape"),
	"internal/agentconfig/render/versions.go:backupAndWrite":          unversioned("foreign: vendor config plus its backup"),
	"internal/agentconfig/render/versions.go:RevertOne":               unversioned("foreign: vendor config revert"),
	"internal/agentconfig/render/versions.go:writeBaseline":           "vendor-baseline",
	"internal/history/history.go:stageFile":                           unversioned("text: session history snapshot of a transcript"),
	"internal/hosttest/capture.go:ClosedStdout":                       unversioned("test helper: an empty temp file standing in for a closed stdout"),
	"internal/hosttest/gittest/gittest.go:WriteRel":                   unversioned("test fixture: a file in a test's temp repo"),
	"internal/hooks/memhook/checkpoint.go:contextCheckpoint":          unversioned("marker: context-checkpoint flag file"),
	"internal/hooks/memhook/checkpoint.go:writeCompactRecord":         unversioned("marker: per-session PreCompact record (trigger, pct, at) read once by SessionStart"),
	"internal/hooks/agents/codex.go:appendCodexRoute":                 "codex-routing",
	"internal/hooks/wthook/claims.go:appendClaims":                    "claims",
	"internal/notices/notices.go:Append":                              "notices",
	"internal/hooks/wthook/claimcheck.go:appendIfNew":                 unversioned("marker: per-session claim advisory dedupe lines"),
	"internal/inflight/limiter.go:writeMarker":                        unversioned("marker: in-flight dispatch marker (account key)"),
	"internal/hooks/wthook/worktree.go:busyLine":                      unversioned("marker: per-session worktree-busy dedupe stamp"),
	"internal/memory/recall/sessionroot.go:RecordSessionRoot":         unversioned("text: per-session launch repository root"),
	"internal/hooks/memhook/memory.go:writeChain":                     unversioned("text: last-session chain stamp"),
	"internal/hooks/memhook/agent_input.go:writeSynthTranscript":      unversioned("text: Claude-shaped transcript synthesized from an agent session"),
	"internal/hooks/memhook/memory_update.go:writeCountState":         unversioned("text: edit count stamp"),
	"internal/hooks/memhook/memory_update.go:writeSidecar":            unversioned("text: update sidecar text"),
	"internal/hooks/memhook/memory_update.go:writeStamp":              unversioned("text: update stamp"),
	"internal/hooks/docs/plan.go:Skeleton":                            unversioned("text: plan skeleton injected marker"),
	"internal/hooks/docs/plantick.go:recordPartials":                  unversioned("text: partly landed plan tasks"),
	"internal/plans/mirror.go:RecordDest":                             unversioned("text: plan draft destination pointer"),
	"internal/hooks/docs/plan.go:claimPlanFile":                       unversioned("marker: reserved dated plan file, replaced by the plan"),
	"internal/plans/tick.go:writePlan":                                unversioned("foreign: plan markdown in plans/"),
	"internal/hooks/quality/stop_format.go:stopFormat":                "stop-format",
	"internal/hooks/memhook/watch.go:spawnIdleWatcher":                unversioned("marker: idle watcher claim file"),
	"internal/launch/install_steps.go:copyBinary":                     unversioned("foreign: installed binary"),
	"internal/agent/launcher.go:WriteFiles":                           unversioned("per-launch 0600 files: settings-overlay row; copilot's foreign registry"),
	"internal/claude/science.go:scienceDefaultsFile":                  unversioned("foreign: science TOML defaults for a vendor"),
	"internal/launch/tmux.go:seedTmuxConf":                            unversioned("foreign: user tmux.conf"),
	"internal/network/mcp/doctor.go:mcpList":                          unversioned("foreign: temp output of claude mcp list"),
	"internal/memory/recall/accesslog.go:logAccessBatch":              unversioned("log: memory access log, text lines"),
	"internal/memory/recall/accesslog.go:maybeTrimAccessLog":          unversioned("log: memory access log trim"),
	"internal/memory/bank/fmt.go:formatFiles":                         unversioned("memory-bank: bank markdown format"),
	"internal/memory/bank/blocks.go:AppendBlocks":                     unversioned("memory-bank: bank blocks"),
	"internal/memory/digest.go:Digest":                                unversioned("memory-bank: bank digest markdown"),
	"internal/memory/doctor.go:WriteDoctorNotice":                     unversioned("text: per-store doctor notice line"),
	"internal/memory/dream.go:stampDream":                             unversioned("text: per-store dream timestamp stamp"),
	"internal/memory/recall/embed/refresh.go:RefreshEmbeddings":       unversioned("marker: embedding refresh spawn stamp"),
	"internal/memory/recall/embed/embed.go:NewSemantic":               unversioned("marker: semantic-recall backoff stamp"),
	"internal/memory/bank/git.go:writeIfAbsent":                       unversioned("memory-bank: memory repo seed files"),
	"internal/memory/update.go:WriteUpdateResult":                     unversioned("text: update result text"),
	"internal/memory/bank/ids.go:assignFileIDs":                       unversioned("memory-bank: bank id comments"),
	"internal/memory/native.go:applyChecked":                          unversioned("memory-bank: native auto-memory files"),
	"internal/memory/native.go:writeExclusive":                        unversioned("memory-bank: native auto-memory files"),
	"internal/memory/promote.go:archiveFile":                          unversioned("memory-bank: bank overflow archive"),
	"internal/hooks/memhook/memory.go:runSweep":                       unversioned("text: catch-up sweep done count"),
	"internal/memory/promote.go:removeBlock":                          unversioned("memory-bank: bank block removal"),
	"internal/memory/recall/index.go:writeRecallIndex":                unversioned("text: recall index, rebuilt on a miss"),
	"internal/memory/bank/region.go:writeBankRegion":                  unversioned("memory-bank: bank generated region"),
	"internal/memory/bank/region.go:WriteCatalog":                     unversioned("memory-bank: memory/index.md catalog"),
	"internal/memory/bank/remember.go:prependBlock":                   unversioned("memory-bank: bank remember"),
	"internal/memory/recall/seen.go:MarkSeen":                         unversioned("log: seen-ids list, text lines"),
	"internal/memory/recall/seen.go:ResetSeen":                        unversioned("log: seen-ids list rewrite"),
	"internal/memory/bank/settle.go:RotateSessionHistory":             unversioned("memory-bank: sessionHistory rotation"),
	"internal/memory/store.go:seedFile":                               unversioned("memory-bank: bank seed files"),
	"internal/memory/store.go:createExcl":                             unversioned("memory-bank: bank seed files (no-hard-link fallback)"),
	"internal/memory/bank/txn.go:apply":                               unversioned("memory-bank: txn applied bank file"),
	"internal/memory/bank/txn.go:captureNative":                       unversioned("memory-bank: txn native copy"),
	"internal/memory/bank/txn.go:commit":                              unversioned("memory-bank: txn receipt in the memory repo"),
	"internal/memory/bank/txn.go:save":                                "memory-journal",
	"internal/testtool/mutate/mutate.go:writeOverlay":                 unversioned("foreign: mutation-test scratch copy"),
	"internal/preflight/live.go:ClaimNamed":                           unversioned("marker: detached refresh attempt stamp"),
	"internal/preflight/discover.go:SpawnDiscover":                    unversioned("foreign: /dev/null opened for a detached refresh"),
	"internal/proxy/restart.go:claimRestartLocked":                    unversioned("marker: watchdog proxy restart stamp"),
	"internal/proxy/restart.go:Hold":                                  unversioned("marker: deliberate proxy stop, watchdog stays off"),
	"internal/router/served.go:SaveServed":                            "served-models",
	"internal/opencode/check.go:SaveCheckCache":                       "opencode-models-check",
	"internal/providers/tokenexpiry.go:recordTokenExpiry":             "token-expiry",
	"internal/proxy/daemon.go:WriteState":                             "router-runtime",
	"internal/proxy/server/sessions.go:saveLocked":                    "router-sessions",
	"internal/review/diff/pranalyzer.go:writeOutput":                  unversioned("foreign: -o output"),
	"internal/review/diff/report.go:writeReport":                      unversioned("foreign: -o output"),
	"internal/review/source/quality_cli.go:emitQualityJSON":           unversioned("foreign: -o output"),
	"internal/router/state.go:Update":                                 "quota",
	"internal/schemas/schemas.go:LogMigration":                        "migrations",
	"internal/schemas/schemas.go:rewrite":                             unversioned("foreign: rewrites registered formats in place during migration"),
	"internal/vault/nofollow_unix.go:openAppendNoFollow":              unversioned("foreign: user .env file"),
	"internal/vault/nofollow_other.go:openAppendNoFollow":             unversioned("foreign: user .env file"),
	"internal/settings/apply.go:ResetSettings":                        unversioned("foreign: owner-run settings.json reset and its .bak copy"),
	"internal/settings/apply.go:SeedSettings":                         unversioned("foreign: first-setup settings.json seed, only when missing"),
	"internal/settings/apply.go:writeBackup":                          unversioned("foreign: ResetSettings' .bak copy, created exclusively"),
	"internal/vault/schema.go:snapshotBeforeMigration":                unversioned("foreign: pre-migration byte copy of the vault"),
	"internal/setup/backup.go:copyFile":                               unversioned("foreign: setup backup copy of user files"),
	"internal/setup/nuke.go:createBackupFile":                         unversioned("foreign: nuke backup archive"),
	"internal/setup/state.go:Record":                                  "setup-state",
	"internal/setup/workspace.go:workspaceDirAction":                  unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceFileAction":                 unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceIgnoreAction":               unversioned("foreign: workspace repo .gitignore"),
	"internal/statusline/cache.go:writeStamp":                         unversioned("text: statusline pipe-separated stamp"),
	"internal/network/tailscale/cli.go:writeAuthKeyFile":              unversioned("foreign: temp auth key file"),
	"internal/ui/theme/theme.go:writeMerged":                          unversioned("foreign: theme file and backup in a vendor home"),
	"internal/agent/tools/version.go:writeVersionCache":               "agentversion",
	"internal/agent/tools/install.go:runInstallScript":                unversioned("foreign: temp installer script"),
	"internal/agent/tools/tools.go:installBinary":                     unversioned("foreign: installed binary"),
	"internal/agent/tools/science.go:installScienceApp":               unversioned("foreign: downloaded DMG temp file"),
	"internal/transcript/summary_cache.go:Save":                       "transcript-summary",
	"internal/usage/claude.go:writeClaudeUsageCache":                  "oauth-usage-cache",
	"internal/usage/budget.go:writeBudgetCache":                       "gateway-budget-cache",
	"internal/tokenledger/store.go:Append":                            "usage-ledger",
	"internal/vault/vault.go:create":                                  "vault",
	"internal/agent/job/job.go:SpawnArgv":                             unversioned("log: vendor job log, humans only"),
	"internal/agent/roles/review.go:ReviewArgs":                       unversioned("foreign: temp diff handed to a vendor CLI"),
	"internal/codex/catalog.go:validateCodexCatalog":                  "codex-catalog",
	"internal/codex/catalog.go:storeCodexCatalog":                     "codex-catalog",
	"internal/codex/hooks.go:writeCodexHooks":                         unversioned("foreign: codex hooks.json in the vendor home"),
	"internal/copilot/home.go:copySettings":                           unversioned("foreign: copilot private home settings copy"),
	"internal/agentconfig/render_opencode.go:WriteOpencodeOverlay":    "opencode-overlay",
	"internal/opencode/hooks.go:openHookLog":                          unversioned("log: opencode hook log, text lines"),
	"internal/opencode/plugin.go:writePlugin":                         unversioned("foreign: generated opencode JS plugin, opencode-owned shape"),
	"internal/opencode/auth.go:updateOpencodeDiskCache":               "opencode-auth-cache",
	"internal/network/vscode/vscode.go:writeGuarded":                  unversioned("foreign: VS Code settings.json, keybindings.json and backup"),
	"internal/network/vscode/serve.go:prepareDataDir":                 unversioned("foreign: VS Code machine settings seed"),
	"internal/network/vscode/serve.go:writeState":                     "vscode-serve-runtime",
	"internal/worktree/ledger.go:editLedger":                          unversioned("memory-bank: worktree ledger log line and task fill"),
	"internal/worktree/ledger.go:CloseLedger":                         unversioned("memory-bank: worktree ledger close"),
	"internal/worktree/ledger.go:OpenLedger":                          unversioned("memory-bank: worktree ledger open"),
	"internal/worktree/operation.go:SaveOperation":                    "worktree-merge-op",
	"internal/worktree/seed.go:copyEnvFile":                           unversioned("text: .env copied into a new worktree"),
}

var writeCalls = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.CreateTemp": true, "os.OpenFile": true,
	"paths.AtomicWrite": true, "paths.AppendLine": true, "paths.WriteIfChanged": true, "paths.Backup": true,
}

func scanWriters(t *testing.T) map[string]bool {
	t.Helper()
	root := ".."
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(filepath.Join("internal", strings.TrimPrefix(filepath.ToSlash(p), "../")))
		if d.IsDir() {
			if d.Name() == "testdata" || rel == "internal/paths" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		return scanWriterFile(p, rel, found)
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func scanWriterFile(path, rel string, found map[string]bool) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return err
	}
	scanWriterDeclarations(f, rel, found)
	return nil
}

func scanWriterDeclarations(f *ast.File, rel string, found map[string]bool) {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if functionWrites(fn) {
			found[rel+":"+fn.Name.Name] = true
		}
	}
}

func functionWrites(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		c, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name := callName(c); writeCalls[name] && (name != "os.OpenFile" || createsFile(c)) {
			found = true
		}
		return true
	})
	return found
}

func callName(c *ast.CallExpr) string {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return id.Name + "." + sel.Sel.Name
}

func createsFile(c *ast.CallExpr) bool {
	if len(c.Args) < 2 {
		return false
	}
	hit := false
	ast.Inspect(c.Args[1], func(n ast.Node) bool {
		if s, ok := n.(*ast.SelectorExpr); ok {
			switch s.Sel.Name {
			case "O_CREATE", "O_APPEND", "O_WRONLY":
				hit = true
			}
		}
		return true
	})
	return hit
}

func TestWriters(t *testing.T) {
	names := map[string]bool{}
	for _, f := range schemas.All() {
		names[f.Name] = true
	}
	found := scanWriters(t)
	for _, issue := range registeredWriterIssues(found, writers, names) {
		t.Error(issue)
	}
	for _, issue := range staleWriterIssues(found, writers) {
		t.Error(issue)
	}
}

func registeredWriterIssues(found map[string]bool, registry map[string]string, names map[string]bool) []string {
	var issues []string
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := registry[k]
		if !ok {
			issues = append(issues, k+" writes a file: register it in internal/schemas or classify it in writers_test.go")
			continue
		}
		if !names[v] && !strings.HasPrefix(v, "unversioned: ") {
			issues = append(issues, fmt.Sprintf("%s maps to %q: not a registered format name nor unversioned(...)", k, v))
		}
	}
	return issues
}

func staleWriterIssues(found map[string]bool, registry map[string]string) []string {
	var issues []string
	for k := range registry {
		if !found[k] {
			issues = append(issues, fmt.Sprintf("writers entry %s no longer has a write site: remove it", k))
		}
	}
	return issues
}

func TestWriterRegistryRejectsUnregisteredAndStale(t *testing.T) {
	const source = `package fixture
	func write() { _ = os.WriteFile("file", nil, 0600) }
	func read() { _, _ = os.OpenFile("file", os.O_RDONLY, 0600) }
	func appendFile() { _, _ = os.OpenFile("file", os.O_APPEND, 0600) }`
	f, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	scanWriterDeclarations(f, "fixture.go", found)
	t.Run("unregistered", func(t *testing.T) {
		issues := registeredWriterIssues(found, map[string]string{}, map[string]bool{})
		want := []string{
			"fixture.go:appendFile writes a file: register it in internal/schemas or classify it in writers_test.go",
			"fixture.go:write writes a file: register it in internal/schemas or classify it in writers_test.go",
		}
		if strings.Join(issues, "\n") != strings.Join(want, "\n") {
			t.Fatalf("unregistered writer issues = %v, want %v", issues, want)
		}
	})
	t.Run("stale", func(t *testing.T) {
		registry := map[string]string{"fixture.go:read": unversioned("foreign: fixture")}
		issues := staleWriterIssues(found, registry)
		if len(issues) != 1 || issues[0] != "writers entry fixture.go:read no longer has a write site: remove it" {
			t.Fatalf("stale writer issues = %v", issues)
		}
	})
}
