package schemas_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/agent"
	_ "github.com/edotau/claude-code/internal/agent/vendors"
	_ "github.com/edotau/claude-code/internal/audit"
	_ "github.com/edotau/claude-code/internal/cli"
	_ "github.com/edotau/claude-code/internal/crossgen"
	_ "github.com/edotau/claude-code/internal/generate"
	_ "github.com/edotau/claude-code/internal/hooks"
	_ "github.com/edotau/claude-code/internal/launch"
	_ "github.com/edotau/claude-code/internal/memory"
	_ "github.com/edotau/claude-code/internal/network/vscode"
	_ "github.com/edotau/claude-code/internal/preflight"
	_ "github.com/edotau/claude-code/internal/providers"
	_ "github.com/edotau/claude-code/internal/proxy"
	_ "github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/schemas"
	_ "github.com/edotau/claude-code/internal/setup"
	_ "github.com/edotau/claude-code/internal/tools"
	_ "github.com/edotau/claude-code/internal/usage"
	_ "github.com/edotau/claude-code/internal/usage/usageledger"
	_ "github.com/edotau/claude-code/internal/vault"
	_ "github.com/edotau/claude-code/internal/worktree"
)

func unversioned(reason string) string { return "unversioned: " + reason }

var writers = map[string]string{
	"internal/agent/codex.go:codexSchemaFile":                        unversioned("foreign: codex --output-schema temp file"),
	"internal/agent/dispatchlog.go:Save":                             "dispatch-record",
	"internal/agent/dispatchlog.go:claimRecord":                      unversioned("claim: the running record, linked in whole"),
	"internal/agent/dispatchlog.go:openLocked":                       unversioned("log: per-job dispatch log, humans only"),
	"internal/agent/eventlog.go:NewEventLog":                         "dispatch-events",
	"internal/agent/live.go:AcquireLiveSlot":                         unversioned("lock: live-session slot file, flock only, no content"),
	"internal/agent/handoff.go:createExclusive":                      unversioned("foreign: handoff markdown in the repo"),
	"internal/audit/audit.go:Log":                                    "audit",
	"internal/cli/complete_domains.go:writeDispatchCache":            "tailscale-machines",
	"internal/cli/helpsvg.go:cmdMenuSVG":                             unversioned("foreign: -o output"),
	"internal/cli/theme.go:setGradient":                              unversioned("foreign: theme file in a vendor home"),
	"internal/cli/network_browser.go:networkBrowserExtract":          unversioned("foreign: --extract -o output"),
	"internal/clipboard/clipboard.go:grabMac":                        unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/clipboard.go:savePNG":                        unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/launchd.go:installLaunchAgentAt":             unversioned("foreign: launchd plist (Apple format)"),
	"internal/clipboard/proxy.go:installProxyAt":                     unversioned("foreign: wl-paste shim binary"),
	"internal/completion/completion.go:Install":                      unversioned("foreign: shell completion script in the user's shell dirs"),
	"internal/configure/vault_edit.go:Edit":                          unversioned("foreign: editor scratch temp file"),
	"internal/configure/home.go:registerCodexMCP":                    unversioned("foreign: codex config.toml in the vendor home"),
	"internal/agent/copilotreg/rundir.go:WriteRegistry":              unversioned("foreign: copilot registry in a private vendor home"),
	"internal/crossgen/sync.go:writeProjection":                      "projections",
	"internal/databricks/config.go:writeScratch":                     unversioned("foreign: temp rescue config"),
	"internal/databricks/export.go:writeFile0600":                    unversioned("foreign: -o export output"),
	"internal/desktop/transaction.go:atomicWrite":                    "desktop-state",
	"internal/generate/codex_settings.go:restoreCodexConfigLocked":   unversioned("foreign: codex config.toml revert in the vendor home"),
	"internal/generate/codex_settings.go:writeCodexConfig":           unversioned("foreign: codex config.toml in the vendor home"),
	"internal/generate/generate.go:commitOverlayFile":                unversioned("foreign: generated vendor overlay, vendor-owned shape"),
	"internal/generate/versions.go:backupAndWrite":                   unversioned("foreign: vendor config plus its backup"),
	"internal/generate/versions.go:revertOne":                        unversioned("foreign: vendor config revert"),
	"internal/generate/versions.go:writeVendorBaseline":              "vendor-baseline",
	"internal/history/history.go:Snapshot":                           unversioned("text: session history snapshot of a transcript"),
	"internal/hooks/checkpoint.go:contextCheckpoint":                 unversioned("marker: context-checkpoint flag file"),
	"internal/hooks/codexaudit.go:appendCodexRoute":                  "codex-routing",
	"internal/proc/inflight/limiter.go:writeMarker":                  unversioned("marker: in-flight dispatch marker (account key)"),
	"internal/hooks/worktreemode.go:busyLine":                        unversioned("marker: per-session worktree-busy dedupe stamp"),
	"internal/hooks/memory.go:writeChain":                            unversioned("text: last-session chain stamp"),
	"internal/hooks/memory.go:writeCountState":                       unversioned("text: edit count stamp"),
	"internal/hooks/memory.go:writeSidecar":                          unversioned("text: update sidecar text"),
	"internal/hooks/memory.go:writeStamp":                            unversioned("text: update stamp"),
	"internal/hooks/plan.go:planSkeleton":                            unversioned("text: plan skeleton injected marker"),
	"internal/hooks/plantick.go:recordPartials":                      unversioned("text: partly landed plan tasks"),
	"internal/hooks/plan.go:recordDest":                              unversioned("text: plan draft destination pointer"),
	"internal/plans/tick.go:writePlan":                               unversioned("foreign: plan markdown in plans/"),
	"internal/hooks/stop_format.go:stopFormat":                       "stop-format",
	"internal/hooks/watch.go:spawnIdleWatcher":                       unversioned("marker: idle watcher claim file"),
	"internal/launch/install_steps.go:copyBinary":                    unversioned("foreign: installed binary"),
	"internal/launch/launch.go:WriteOverlay":                         "settings-overlay",
	"internal/launch/scienceconfig.go:scienceDefaultsFile":           unversioned("foreign: science TOML defaults for a vendor"),
	"internal/launch/tmux.go:seedTmuxConf":                           unversioned("foreign: user tmux.conf"),
	"internal/network/mcp/doctor.go:mcpList":                         unversioned("foreign: temp output of claude mcp list"),
	"internal/memory/accesslog.go:LogAccessBatch":                    unversioned("log: memory access log, text lines"),
	"internal/memory/accesslog.go:maybeTrimAccessLog":                unversioned("log: memory access log trim"),
	"internal/memory/bankfmt.go:formatFiles":                         unversioned("memory-bank: bank markdown format"),
	"internal/memory/blocks.go:appendBlocks":                         unversioned("memory-bank: bank blocks"),
	"internal/memory/digest.go:Digest":                               unversioned("memory-bank: bank digest markdown"),
	"internal/memory/doctor.go:WriteDoctorNotice":                    unversioned("text: doctor notice line"),
	"internal/memory/dream.go:Dream":                                 unversioned("text: dream timestamp stamp"),
	"internal/memory/embed.go:NewSemantic":                           unversioned("marker: semantic-recall backoff stamp"),
	"internal/memory/git.go:writeIfAbsent":                           unversioned("memory-bank: memory repo seed files"),
	"internal/memory/update.go:WriteUpdateResult":                    unversioned("text: update result text"),
	"internal/memory/ids.go:assignIDs":                               unversioned("memory-bank: bank id comments"),
	"internal/memory/native.go:applyChecked":                         unversioned("memory-bank: native auto-memory files"),
	"internal/memory/native.go:writeExclusive":                       unversioned("memory-bank: native auto-memory files"),
	"internal/memory/promote.go:promoteFile":                         unversioned("memory-bank: bank promote"),
	"internal/memory/promote.go:removeBlock":                         unversioned("memory-bank: bank block removal"),
	"internal/memory/recallindex.go:writeRecallIndex":                unversioned("text: recall index, rebuilt on a miss"),
	"internal/memory/region.go:writeBankRegion":                      unversioned("memory-bank: bank generated region"),
	"internal/memory/region.go:writeCatalog":                         unversioned("memory-bank: memory/index.md catalog"),
	"internal/memory/remember.go:prependBlock":                       unversioned("memory-bank: bank remember"),
	"internal/memory/seen.go:MarkSeen":                               unversioned("log: seen-ids list, text lines"),
	"internal/memory/seen.go:ResetSeen":                              unversioned("log: seen-ids list rewrite"),
	"internal/memory/settle.go:RotateSessionHistory":                 unversioned("memory-bank: sessionHistory rotation"),
	"internal/memory/store.go:Init":                                  unversioned("memory-bank: bank seed files"),
	"internal/memory/txn.go:apply":                                   unversioned("memory-bank: txn applied bank file"),
	"internal/memory/txn.go:captureNative":                           unversioned("memory-bank: txn native copy"),
	"internal/memory/txn.go:commit":                                  unversioned("memory-bank: txn receipt in the memory repo"),
	"internal/memory/txn.go:save":                                    "memory-journal",
	"internal/review/mutate/mutate.go:Check":                         unversioned("foreign: mutation-test scratch copy"),
	"internal/preflight/served.go:ClaimRefresh":                      unversioned("marker: detached served-refresh attempt stamp"),
	"internal/preflight/served.go:SaveServed":                        "served-models",
	"internal/providers/opencode_check.go:SaveCheckCache":            "opencode-models-check",
	"internal/providers/tokenexpiry.go:recordTokenExpiry":            "token-expiry",
	"internal/proxy/daemon.go:Serve":                                 "router-runtime",
	"internal/proxy/sessions.go:saveLocked":                          "router-sessions",
	"internal/review/diff/pranalyzer.go:RunPR":                       unversioned("foreign: -o output"),
	"internal/review/diff/report.go:RunReport":                       unversioned("foreign: -o output"),
	"internal/review/source/quality_cli.go:emitQualityJSON":          unversioned("foreign: -o output"),
	"internal/router/state.go:Update":                                "quota",
	"internal/schemas/ledger.go:LogMigration":                        "migrations",
	"internal/schemas/pending.go:rewrite":                            unversioned("foreign: rewrites registered formats in place during migration"),
	"internal/secrets/dotenv.go:AppendDotenv":                        unversioned("foreign: user .env file"),
	"internal/settings/repodefaults.go:mergeRepoDefaults":            unversioned("foreign: repo defaults settings.json, Claude-owned shape"),
	"internal/settings/settings.go:WriteWithBackup":                  unversioned("foreign: settings.json and backup"),
	"internal/setup/backup.go:copyFile":                              unversioned("foreign: setup backup copy of user files"),
	"internal/setup/nuke.go:createBackupFile":                        unversioned("foreign: nuke backup archive"),
	"internal/setup/state.go:Record":                                 "setup-state",
	"internal/setup/workspace.go:workspaceDirAction":                 unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceFileAction":                unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceIgnoreAction":              unversioned("foreign: workspace repo .gitignore"),
	"internal/ui/statusline/cache.go:writeStamp":                     unversioned("text: statusline pipe-separated stamp"),
	"internal/network/tailscale/cli.go:writeAuthKeyFile":             unversioned("foreign: temp auth key file"),
	"internal/ui/theme/theme.go:writeMerged":                         unversioned("foreign: theme file and backup in a vendor home"),
	"internal/tools/agentversion.go:writeVersionCache":               "agentversion",
	"internal/tools/install.go:runInstallScript":                     unversioned("foreign: temp installer script"),
	"internal/tools/tools.go:installBinary":                          unversioned("foreign: installed binary"),
	"internal/transcript/summary_cache.go:Save":                      "transcript-summary",
	"internal/usage/claude.go:writeClaudeUsageCache":                 "oauth-usage-cache",
	"internal/usage/budget.go:writeBudgetCache":                      "gateway-budget-cache",
	"internal/usage/usageledger/ledger.go:Append":                    "usage-ledger",
	"internal/vault/vault.go:create":                                 "vault",
	"internal/agent/adapter_jobs.go:SpawnArgv":                       unversioned("log: vendor job log, humans only"),
	"internal/agent/adapter_review.go:ReviewArgs":                    unversioned("foreign: temp diff handed to a vendor CLI"),
	"internal/agent/vendors/codexcatalog.go:codexCatalogPath":        "codex-catalog",
	"internal/agent/vendors/codexcatalog.go:writeCodexCatalog":       "codex-catalog",
	"internal/agent/vendors/copilot_registry.go:copySettings":        unversioned("foreign: copilot private home settings copy"),
	"internal/settings/opencode.go:WriteOpencodeOverlay":             "opencode-overlay",
	"internal/agent/vendors/opencodeauth.go:updateOpencodeDiskCache": "opencode-auth-cache",
	"internal/network/vscode/keybindings.go:mergeKeybindings":        unversioned("foreign: VS Code keybindings.json and backup"),
	"internal/network/vscode/vscode.go:merge":                        unversioned("foreign: VS Code settings.json and backup"),
	"internal/network/vscode/vscodeserve.go:prepareDataDir":          unversioned("foreign: VS Code machine settings seed"),
	"internal/network/vscode/vscodeserve.go:writeState":              "vscode-serve-runtime",
	"internal/worktree/ledger.go:editLedger":                         unversioned("memory-bank: worktree ledger log line and task fill"),
	"internal/worktree/ledger.go:CloseLedger":                        unversioned("memory-bank: worktree ledger close"),
	"internal/worktree/ledger.go:OpenLedger":                         unversioned("memory-bank: worktree ledger open"),
	"internal/worktree/operation.go:SaveOperation":                   "worktree-merge-op",
	"internal/worktree/seed.go:seedPath":                             unversioned("text: .env copied into a new worktree"),
}

var writeCalls = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.CreateTemp": true, "os.OpenFile": true,
	"paths.AtomicWrite": true, "paths.WriteIfChanged": true, "paths.WriteBackup": true, "paths.Backup": true,
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
		f, perr := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if perr != nil {
			return perr
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if name := callName(c); writeCalls[name] && (name != "os.OpenFile" || createsFile(c)) {
					found[rel+":"+fn.Name.Name] = true
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
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
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, ok := writers[k]
		if !ok {
			t.Errorf("%s writes a file: register it in internal/schemas or classify it in writers_test.go", k)
			continue
		}
		if !names[v] && !strings.HasPrefix(v, "unversioned: ") {
			t.Errorf("%s maps to %q: not a registered format name nor unversioned(...)", k, v)
		}
	}
	for k := range writers {
		if !found[k] {
			t.Errorf("writers entry %s no longer has a write site: remove it", k)
		}
	}
}
