package schemas

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

type rec struct {
	Schema int    `json:"schema"`
	Name   string `json:"name"`
}

func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func ledgerLines(t *testing.T, format string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(LedgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("ledger line not JSON: %q: %v", l, err)
		}
		if m["format"] == format {
			out = append(out, m)
		}
	}
	return out
}

func TestDecodeV0ThroughSameShapeReportsFromZero(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-v0", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	from, err := Decode(f, []byte(`{"name":"a"}`), &got)
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || got.Name != "a" {
		t.Fatalf("from=%d got=%+v, want from=0 name=a", from, got)
	}
}

func TestDecodeNewerRecordReturnsErrNewer(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-newer", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	if _, err := Decode(f, []byte(`{"schema":2,"name":"a"}`), &got); !errors.Is(err, ErrNewer) {
		t.Fatalf("err=%v, want ErrNewer", err)
	}
}

func TestDecodeRejectsNegativeSchemaBeforeMigration(t *testing.T) {
	calls := 0
	f := Format{Name: "negative-schema", Version: 1, Migrate: func(_ int, _ []byte) ([]byte, error) {
		calls++
		return nil, errors.New("unexpected migration")
	}}
	var got rec
	_, err := Decode(f, []byte(`{"schema":-2147483648}`), &got)
	if calls != 0 {
		t.Fatalf("negative schema invoked migration %d times, want 0", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "negative schema version -2147483648") {
		t.Fatalf("Decode error = %v, want negative schema version rejection", err)
	}
}

func TestDecodeOlderWithoutMigrateReturnsErrNoMigration(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-nomig", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"})
	var got rec
	if _, err := Decode(f, []byte(`{"name":"a"}`), &got); !errors.Is(err, ErrNoMigration) {
		t.Fatalf("err=%v, want ErrNoMigration", err)
	}
}

// reg registers f for one test and drops the row at cleanup, so -count>1 re-registers cleanly.
func reg(t *testing.T, f Format) Format {
	t.Helper()
	t.Cleanup(func() { mu.Lock(); delete(rows, f.Name); mu.Unlock() })
	return Register(f)
}

func TestRegisterPanicsOnDuplicateName(t *testing.T) {
	f := Format{Name: "t-dup", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"}
	reg(t, f)
	defer func() {
		if recover() == nil {
			t.Fatal("second Register of the same Name did not panic")
		}
	}()
	Register(f)
}

func TestLedgerAppendsOneJSONLineMode0600(t *testing.T) {
	hermetic(t)
	LogMigration("t-ledger", 0, 1, 3)
	lines := ledgerLines(t, "t-ledger")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	m := lines[0]
	if m["schema"] != float64(1) || m["from"] != float64(0) || m["to"] != float64(1) || m["records"] != float64(3) {
		t.Fatalf("line = %v", m)
	}
	if at, _ := m["at"].(string); at == "" {
		t.Fatalf("missing at: %v", m)
	}
	st, err := os.Stat(LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

// TestDecodeNullIsErrorNotPanic: a `null` document is rejected, never a nil-map panic.
func TestDecodeNullIsErrorNotPanic(t *testing.T) {
	f := Format{Name: "null-doc", Version: 1, Migrate: SameShape}
	var got map[string]any
	if _, err := Decode(f, []byte("null"), &got); err == nil {
		t.Fatal("Decode(null) returned nil error")
	}
	if _, err := SameShape(0, []byte("null")); err == nil {
		t.Fatal("SameShape(0, null) returned nil error")
	}
}

func unversioned(reason string) string { return "unversioned: " + reason }

var writers = map[string]string{
	"internal/codex/codex.go:codexSchemaFile":                unversioned("foreign: codex --output-schema temp file"),
	"internal/agent/record.go:Save":                          "dispatch-record",
	"internal/providers/lastprofile.go:writeLastProfiles":    "last-profile",
	"internal/agentconfig/overlay.go:appendOverlayDebug":     unversioned("debug: COWORKER_OVERLAY_DEBUG env names + HMAC fingerprints, no values"),
	"internal/agentconfig/overlay.go:overlayDebugSalt":       unversioned("debug: the generated fingerprint salt beside the --overlay-debug file"),
	"internal/agent/run.go:write":                            unversioned("foreign: the packed context file an agy run reads, removed after the run"),
	"internal/agent/record.go:claimRecord":                   unversioned("claim: the running record, linked in whole"),
	"internal/agent/wire/log.go:openLocked":                  unversioned("log: per-job dispatch log, humans only"),
	"internal/agent/wire/event.go:NewEventLog":               "dispatch-events",
	"internal/agent/live.go:AcquireLiveSlot":                 unversioned("lock: live-session slot file, flock only, no content"),
	"internal/copilot/copilot.go:createExclusive":            unversioned("foreign: handoff markdown in the repo"),
	"internal/audit/audit.go:Log":                            "audit",
	"internal/network/tailscale/host.go:WriteMachinesCache":  "tailscale-machines",
	"internal/cli/menu.go:cmdMenuSVG":                        unversioned("foreign: -o output"),
	"internal/ui/theme/look.go:SetGradient":                  unversioned("foreign: theme file in a vendor home"),
	"internal/clipboard/clipboard.go:grabMac":                unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/clipboard.go:savePNG":                unversioned("foreign: clipboard PNG in the temp dir"),
	"internal/clipboard/ssh_bridge.go:installLaunchAgentAt":  unversioned("foreign: launchd plist (Apple format)"),
	"internal/clipboard/wlproxy.go:installProxyAt":           unversioned("foreign: wl-paste shim binary"),
	"internal/ui/helptext/completion.go:Install":             unversioned("foreign: shell completion script in the user's shell dirs"),
	"internal/vault/vaultdoc/doc.go:writeEditorScratch":      unversioned("foreign: editor scratch temp file"),
	"internal/configure/home.go:RegisterCodexMCP":            unversioned("foreign: codex config.toml in the vendor home"),
	"internal/agentconfig/project/sync.go:writeProjection":   "projections",
	"internal/databricks/config.go:writeScratch":             unversioned("foreign: temp rescue config"),
	"internal/databricks/export.go:mergeInto":                unversioned("foreign: export --merge into ~/.databrickscfg"),
	"internal/databricks/export.go:writeFile0600":            unversioned("foreign: -o export output"),
	"internal/codex/permissions.go:writeRetargetedConfig":    unversioned("foreign: per-dispatch CODEX_HOME config.toml retargeted from the user's, removed after the run"),
	"internal/codex/permissions.go:saveRefreshedAuth":        unversioned("foreign: codex auth.json refreshed during a dispatch, copied back to the real CODEX_HOME"),
	"internal/clipboard/ssh_bridge.go:logServed":             unversioned("log: one line per Mac file served over the clip bridge (path, never bytes)"),
	"internal/clipboard/ssh_bridge.go:fetchFrom":             unversioned("cache: a Mac file fetched over the clip bridge into /tmp/claude-drop"),
	"internal/clipboard/ssh_bridge.go:claimDropPath":         unversioned("claim: empty O_EXCL placeholder in /tmp/claude-drop, filled by fetchFrom"),
	"internal/codex/permissions.go:writeRulesDir":            unversioned("foreign: per-dispatch CODEX_HOME rules dir and execpolicy file, removed after the run"),
	"internal/codex/settings.go:applyCodexRestore":           unversioned("foreign: codex config.toml revert in the vendor home"),
	"internal/codex/settings.go:writeCodexConfigLocked":      unversioned("foreign: codex config.toml in the vendor home"),
	"internal/agentconfig/vendorfile.go:CommitOverlayFile":   unversioned("foreign: generated vendor overlay, vendor-owned shape"),
	"internal/agentconfig/vendorfile.go:backupAndWrite":      unversioned("foreign: vendor config plus its backup"),
	"internal/agentconfig/vendorfile.go:RevertOne":           unversioned("foreign: vendor config revert"),
	"internal/agentconfig/vendorfile.go:writeBaseline":       "vendor-baseline",
	"internal/history/history.go:stageFile":                  unversioned("text: session history snapshot of a transcript"),
	"internal/hosttest/capture.go:ClosedStdout":              unversioned("test helper: an empty temp file standing in for a closed stdout"),
	"internal/hosttest/git.go:WriteRel":                      unversioned("test fixture: a file in a test's temp repo"),
	"internal/hooks/memhook/checkpoint.go:contextCheckpoint": unversioned("marker: context-checkpoint flag file"),
	"internal/memory/session_start.go:WriteCompactRecord":    unversioned("marker: per-session PreCompact record (trigger, pct, at) read once by SessionStart"),
	"internal/hooks/agents/codex.go:appendCodexRoute":        "codex-routing",
	"internal/hooks/wthook/claims.go:appendClaims":           "claims",
	"internal/worktree/sessions.go:Append":                   "notices",
	"internal/hooks/wthook/claimcheck.go:appendIfNew":        unversioned("marker: per-session claim advisory dedupe lines"),
	"internal/inflight/limiter.go:writeMarker":               unversioned("marker: in-flight dispatch marker (account key)"),
	"internal/hooks/wthook/worktree.go:busyLine":             unversioned("marker: per-session worktree-busy dedupe stamp"),
	"internal/hooks/safety/rtk.go:rtkFirstShadowNotice":      unversioned("marker: per-session rtk-shadow notice stamp"),
	"internal/memory/session_start.go:recordSessionRoot":     unversioned("text: per-session launch repository root"),
	"internal/memory/session_start.go:writeChain":            unversioned("text: last-session chain stamp"),
	"internal/memory/agent_input.go:writeSynthTranscript":    unversioned("text: Claude-shaped transcript synthesized from an agent session"),
	"internal/memory/update_gate.go:writeCountState":         unversioned("text: edit count stamp"),
	"internal/memory/update_gate.go:writeSidecar":            unversioned("text: update sidecar text"),
	"internal/memory/update_gate.go:writeStamp":              unversioned("text: update stamp"),
	"internal/hooks/docs/plan.go:Skeleton":                   unversioned("text: plan skeleton injected marker"),
	"internal/hooks/docs/plantick.go:recordPartials":         unversioned("text: partly landed plan tasks"),
	"internal/plans/tick.go:RecordDest":                      unversioned("text: plan draft destination pointer"),
	"internal/hooks/docs/plan.go:claimPlanFile":              unversioned("marker: reserved dated plan file, replaced by the plan"),
	"internal/hooks/docs/plan.go:dropRecord":                 unversioned("marker: plan mirror record emptied when it cannot be removed"),
	"internal/plans/tick.go:writePlan":                       unversioned("foreign: plan markdown in plans/"),
	"internal/hooks/quality/stop_format.go:StopFormat":       "stop-format",
	"internal/memory/watch.go:spawnIdleWatcher":              unversioned("marker: idle watcher claim file"),
	"internal/setup/install_steps.go:copyBinary":             unversioned("foreign: installed binary"),
	"internal/agent/launch.go:WriteFiles":                    unversioned("per-launch 0600 files: settings-overlay row; copilot's foreign registry"),
	"internal/claude/science.go:scienceDefaultsFile":         unversioned("foreign: science TOML defaults for a vendor"),
	"internal/setup/install_steps.go:seedTmuxConf":           unversioned("foreign: user tmux.conf"),
	"internal/doctor/mcphealth.go:mcpList":                   unversioned("foreign: temp output of claude mcp list"),
	"internal/memory/recall_accesslog.go:appendAccessLines":  unversioned("log: memory access log, text lines"),
	"internal/memory/recall_accesslog.go:maybeTrimAccessLog": unversioned("log: memory access log trim"),
	"internal/memory/bank_fmt.go:formatFiles":                unversioned("memory-bank: bank markdown format"),
	"internal/memory/bank_blocks.go:appendBlocks":            unversioned("memory-bank: bank blocks"),
	"internal/memory/digest.go:Digest":                       unversioned("memory-bank: bank digest markdown"),
	"internal/memory/doctor.go:writeDoctorNotice":            unversioned("text: per-store doctor notice line"),
	"internal/memory/dream.go:stampDream":                    unversioned("text: per-store dream timestamp stamp"),
	"internal/memory/embed_store.go:refreshEmbeddings":       unversioned("marker: embedding refresh spawn stamp"),
	"internal/memory/embed.go:NewSemantic":                   unversioned("marker: semantic-recall backoff stamp"),
	"internal/memory/spawn.go:spawnSelf":                     unversioned("log: detached worker log, humans only"),
	"internal/memory/bank_git.go:writeIfAbsent":              unversioned("memory-bank: memory repo seed files"),
	"internal/memory/update.go:writeUpdateResult":            unversioned("text: update result text"),
	"internal/memory/bank_ids.go:assignFileIDs":              unversioned("memory-bank: bank id comments"),
	"internal/memory/native.go:applyChecked":                 unversioned("memory-bank: native auto-memory files"),
	"internal/memory/native.go:writeExclusive":               unversioned("memory-bank: native auto-memory files"),
	"internal/memory/promote.go:archiveFileLocked":           unversioned("memory-bank: bank overflow archive"),
	"internal/memory/catchup.go:runSweep":                    unversioned("text: catch-up sweep done count"),
	"internal/memory/promote.go:removeBlock":                 unversioned("memory-bank: bank block removal"),
	"internal/memory/recall_index.go:writeRecallIndex":       unversioned("text: recall index, rebuilt on a miss"),
	"internal/memory/bank_region.go:writeBankRegion":         unversioned("memory-bank: bank generated region"),
	"internal/memory/bank_settle.go:writeCatalog":            unversioned("memory-bank: memory/index.md catalog"),
	"internal/memory/bank_remember.go:writeRememberFact":     unversioned("memory-bank: bank remember"),
	"internal/memory/recall_seen.go:markSeen":                unversioned("log: seen-ids list, text lines"),
	"internal/memory/recall_seen.go:resetSeen":               unversioned("log: seen-ids list rewrite"),
	"internal/memory/bank_settle.go:rotateSessionHistory":    unversioned("memory-bank: sessionHistory rotation"),
	"internal/memory/store.go:seedFile":                      unversioned("memory-bank: bank seed files"),
	"internal/memory/store.go:createExcl":                    unversioned("memory-bank: bank seed files (no-hard-link fallback)"),
	"internal/memory/bank_report.go:appendText":              unversioned("memory-bank: report and decisions append"),
	"internal/memory/bank_settle.go:restore":                 unversioned("memory-bank: previous report restored after a failed decisions append"),
	"internal/memory/bank_txn.go:apply":                      unversioned("memory-bank: txn applied bank file"),
	"internal/memory/bank_txn.go:captureNative":              unversioned("memory-bank: txn native copy"),
	"internal/memory/bank_txn.go:commit":                     unversioned("memory-bank: txn receipt in the memory repo"),
	"internal/memory/bank_txn.go:save":                       "memory-journal",
	"internal/catalog/discover.go:ClaimNamed":                unversioned("marker: detached refresh attempt stamp"),
	"internal/catalog/discover.go:SpawnDiscover":             unversioned("foreign: /dev/null opened for a detached refresh"),
	"internal/gateway/restart.go:claimRestartLocked":         unversioned("marker: watchdog proxy restart stamp"),
	"internal/gateway/restart.go:Hold":                       unversioned("marker: deliberate proxy stop, watchdog stays off"),
	"internal/router/served.go:SaveServed":                   "served-models",
	"internal/catalog/check.go:SaveCheckCache":               "opencode-models-check",
	"internal/providers/tokenexpiry.go:recordTokenExpiry":    "token-expiry",
	"internal/gateway/daemon.go:WriteState":                  "router-runtime",
	"internal/gateway/server/sessions.go:saveLocked":         "router-sessions",
	"internal/review/diff/pranalyzer.go:writeOutput":         unversioned("foreign: -o output"),
	"internal/review/diff/gate.go:runStagedComplexity":       unversioned("foreign: staged-blob scratch copy, removed after the gate"),
	"internal/review/source/quality_cli.go:emitQualityJSON":  unversioned("foreign: -o output"),
	"internal/router/state.go:writeState":                    "quota",
	"internal/router/state.go:backupCorruptState":            unversioned("backup: the corrupt quota.json copied aside before it is replaced"),
	"internal/schemas/schemas.go:LogMigration":               "migrations",
	"internal/vault/nofollow_unix.go:openAppendNoFollow":     unversioned("foreign: user .env file"),
	"internal/vault/nofollow_other.go:openAppendNoFollow":    unversioned("foreign: user .env file"),
	"internal/settings/settings.go:ResetSettings":            unversioned("foreign: owner-run settings.json reset and its .bak copy"),
	"internal/settings/settings.go:SeedSettings":             unversioned("foreign: first-setup settings.json seed, only when missing"),
	"internal/settings/settings.go:writeBackup":              unversioned("foreign: ResetSettings' .bak copy, created exclusively"),
	"internal/vault/vault.go:snapshotBeforeMigration":        unversioned("foreign: pre-migration byte copy of the vault"),
	"internal/setup/adopt.go:copyFile":                       unversioned("foreign: setup backup copy of user files"),
	"internal/setup/nuke.go:createBackupFile":                unversioned("foreign: nuke backup archive"),
	"internal/setup/adopt.go:Record":                         "setup-state",
	"internal/setup/workspace.go:workspaceDirAction":         unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceFileAction":        unversioned("foreign: workspace repo files"),
	"internal/setup/workspace.go:workspaceIgnoreAction":      unversioned("foreign: workspace repo .gitignore"),
	"internal/ui/statusline/cache.go:writeStamp":             unversioned("text: statusline pipe-separated stamp"),
	"internal/network/tailscale/cli.go:writeAuthKeyFile":     unversioned("foreign: temp auth key file"),
	"internal/agentconfig/vendorfile.go:restore":             unversioned("foreign: agent config restored after a failed staged render"),
	"internal/ui/theme/theme.go:replaceIfUnchanged":          unversioned("foreign: theme file and backup in a vendor home"),
	"internal/agent/tools/version.go:writeVersionCache":      "agentversion",
	"internal/agent/tools/install.go:runInstallScript":       unversioned("foreign: temp installer script"),
	"internal/agent/tools/tools.go:installBinary":            unversioned("foreign: installed binary"),
	"internal/agent/tools/science.go:installScienceApp":      unversioned("foreign: downloaded DMG temp file"),
	"internal/transcript/summary_cache.go:Save":              "transcript-summary",
	"internal/usage/claude.go:writeClaudeUsageCache":         "oauth-usage-cache",
	"internal/usage/budget.go:writeBudgetCache":              "gateway-budget-cache",
	"internal/vault/vault.go:create":                         "vault",
	"internal/agent/job.go:SpawnArgv":                        unversioned("log: vendor job log, humans only"),
	"internal/agent/ask/reviewdiff.go:ReviewArgs":            unversioned("foreign: temp diff handed to a vendor CLI"),
	"internal/codex/catalog.go:validateCodexCatalog":         "codex-catalog",
	"internal/codex/catalog.go:storeCodexCatalog":            "codex-catalog",
	"internal/codex/hooks.go:writeCodexHooks":                unversioned("foreign: codex hooks.json in the vendor home"),
	"internal/copilot/home.go:copySettings":                  unversioned("foreign: copilot private home settings copy"),
	"internal/agentconfig/opencode.go:WriteOpencodeOverlay":  "opencode-overlay",
	"internal/opencode/hooks.go:openHookLog":                 unversioned("log: opencode hook log, text lines"),
	"internal/opencode/plugin.go:writePlugin":                unversioned("foreign: generated opencode JS plugin, opencode-owned shape"),
	"internal/opencode/auth.go:updateOpencodeDiskCache":      "opencode-auth-cache",
	"internal/ui/vscode/vscode.go:writeGuarded":              unversioned("foreign: VS Code settings.json, keybindings.json and backup"),
	"internal/worktree/ledger.go:editLedger":                 unversioned("memory-bank: worktree ledger log line and task fill"),
	"internal/worktree/ledger.go:CloseLedger":                unversioned("memory-bank: worktree ledger close"),
	"internal/worktree/ledger.go:openLedger":                 unversioned("memory-bank: worktree ledger open"),
	"internal/worktree/operation.go:SaveOperation":           "worktree-merge-op",
	"internal/worktree/seed.go:copyEnvFile":                  unversioned("text: .env copied into a new worktree"),
}

var writeCalls = map[string]bool{
	"os.WriteFile": true, "os.Create": true, "os.CreateTemp": true, "os.OpenFile": true,
	"paths.AtomicWrite": true, "paths.AppendLine": true, "paths.WriteIfChanged": true, "paths.WriteJSON": true, "paths.Backup": true,
	"paths.WriteIfUnchanged": true,
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

// goldenNames reads the format names from the registry golden, which internal/cli checks against the linked binary.
func goldenNames(t *testing.T) map[string]bool {
	t.Helper()
	b, err := os.ReadFile("testdata/registry.golden")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for l := range strings.SplitSeq(string(b), "\n") {
		if name, _, ok := strings.Cut(l, " | "); ok && !strings.HasPrefix(l, "#") {
			names[name] = true
		}
	}
	return names
}

func TestWriters(t *testing.T) {
	found := scanWriters(t)
	names := goldenNames(t)
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
			issues = append(issues, k+" writes a file: register it in internal/schemas or classify it in schemas_test.go")
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
			"fixture.go:appendFile writes a file: register it in internal/schemas or classify it in schemas_test.go",
			"fixture.go:write writes a file: register it in internal/schemas or classify it in schemas_test.go",
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
