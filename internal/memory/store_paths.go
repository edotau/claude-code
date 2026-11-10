package memory

// The unified store tree (plan §1): ~/.claude/memory/ is one local Git repo holding the home bank under
// home/, every repo's memory under <repo>/ (keyed by its origin remote), and the harness's own under
// claude-code/. HomeBankDir and Dir (below) are the live path resolvers.

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// CatalogFile is the reserved global catalog at the store root; BuildCatalog (Phase 2) is its only writer.
const CatalogFile = "index.md"

// storeReservedDirs are store subdirectories that are never a repo's memory dir.
var storeReservedDirs = map[string]bool{
	".git": true, "archive": true, "handoffs": true, "receipts": true, "reports": true, "home": true,
}

// StoreDir is the memory store root and the work tree of the local memory repo.
func StoreDir() string { return filepath.Join(paths.ConfigDir(), "memory") }

// harnessStoreDir is the harness's own memory dir inside the store; keyed by the same rule as any repo.
func harnessStoreDir() string { return filepath.Join(StoreDir(), harnessStoreKey()) }

// archiveDir is dir's cold tier for rotated entries and prior revisions.
func archiveDir(dir string) string { return filepath.Join(dir, "archive") }

// HandoffsDir is dir's long-form session handoffs.
func HandoffsDir(dir string) string { return filepath.Join(dir, "handoffs") }

// ReportsDir is dir's command/agent reports (reviews, audits, research briefs).
func ReportsDir(dir string) string { return filepath.Join(dir, "reports") }

// knowledgeDir is dir's local, near-rules tier: durable lessons kept short of a rule (plan C.2, post-T10).
func knowledgeDir(dir string) string { return filepath.Join(dir, "knowledge") }

// JournalDir holds pending commit-transaction journals; state only, never committed.
func JournalDir() string { return filepath.Join(StateDir(), "journal") }

// AccessStateDir holds migrated access logs; the single source of truth for migrate and clean.
func AccessStateDir() string { return filepath.Join(StateDir(), "access") }

// insideStore reports whether p resolves under the store (and not under its .git); rel is slash-separated.
func insideStore(p string) (rel string, ok bool) {
	store := paths.CanonDeep(StoreDir())
	r, err := filepath.Rel(store, paths.CanonDeep(p))
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	rel = filepath.ToSlash(r)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", false
	}
	return rel, true
}

// StateDir holds update stamps, results, logs and the session chain.
func StateDir() string { return filepath.Join(paths.StateDir(), "memory") }

// HomeBankDir is the shared bank: store/home.
func HomeBankDir() string { return homeStoreDir() }

// homeStoreDir is the home dir inside the store.
func homeStoreDir() string { return filepath.Join(StoreDir(), "home") }

// Dir is root's local bank; an unresolvable repo gets an inert state-dir path, never the home bank.
func Dir(root string) string {
	if d, err := RepoDir(root); err == nil { // one dir per repo shared with native memory; the harness is memory/claude-code
		return d
	}
	switch id := Resolve(root); {
	case id.HomeBank:
		return homeStoreDir()
	case id.Harness:
		return harnessStoreDir()
	}
	return filepath.Join(StateDir(), "unresolved-store")
}

// isHomeBank reports whether root's local bank is the home bank (read it once, not twice).
func isHomeBank(root string) bool { return paths.SamePath(Dir(root), HomeBankDir()) }

// Root resolves --repo → $CLAUDE_PROJECT_DIR (only when it is a repo or the config dir) → git toplevel → cwd.
// A launcher may stamp CLAUDE_PROJECT_DIR with a non-repo parent (e.g. $HOME); honoring that attached every
// bank verb to a junk root instead of the repo the user is standing in.
func Root(repo string) string {
	if repo != "" {
		abs, _ := filepath.Abs(repo)
		return abs
	}
	if d := os.Getenv("CLAUDE_PROJECT_DIR"); d != "" && (paths.SamePath(d, paths.ConfigDir()) || paths.SamePath(paths.MainCheckout(d), paths.ConfigDir()) || paths.SamePath(GitToplevel(d), d)) {
		return d
	}
	cwd, _ := os.Getwd()
	// Inside the store its own toplevel is the store root; keep cwd so memory/<repo>/ resolves to that repo.
	if top := GitToplevel(cwd); top != "" && !paths.SamePath(top, StoreDir()) {
		return top
	}
	return cwd
}
