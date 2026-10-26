package memory

// The unified store tree (plan §1): ~/.claude/memory/ is one local Git repo holding the home bank under
// home/, every repo's memory under <repo>/ (keyed by its origin remote), and the harness's own under
// claude-code/. Legacy paths (HomeBankDir, Dir, NativeDir) stay untouched until the Phase 2 cutover.

import (
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// CatalogFile is the reserved global catalog at the store root; BuildCatalog (Phase 2) is its only writer.
const CatalogFile = "index.md"

// StoreReservedDirs are store subdirectories that are never a repo's memory dir.
var StoreReservedDirs = map[string]bool{
	".git": true, "archive": true, "handoffs": true, "receipts": true, "reports": true, "home": true,
}

// StoreDir is the memory store root and the work tree of the local memory repo.
func StoreDir() string { return filepath.Join(paths.ConfigDir(), "memory") }

// HarnessStoreDir is the harness's own memory dir inside the store; keyed by the same rule as any repo.
func HarnessStoreDir() string { return filepath.Join(StoreDir(), harnessStoreKey()) }

// ArchiveDir is dir's cold tier for rotated entries and prior revisions.
func ArchiveDir(dir string) string { return filepath.Join(dir, "archive") }

// HandoffsDir is dir's long-form session handoffs.
func HandoffsDir(dir string) string { return filepath.Join(dir, "handoffs") }

// ReportsDir is dir's command/agent reports (reviews, audits, research briefs).
func ReportsDir(dir string) string { return filepath.Join(dir, "reports") }

// KnowledgeDir is dir's local, near-rules tier: durable lessons kept short of a rule (plan C.2, post-T10).
func KnowledgeDir(dir string) string { return filepath.Join(dir, "knowledge") }

// JournalDir holds pending commit-transaction journals; state only, never committed.
func JournalDir() string { return filepath.Join(StateDir(), "journal") }

// AccessStateDir holds migrated access logs; the single source of truth for migrate and clean.
func AccessStateDir() string { return filepath.Join(StateDir(), "access") }

// InsideStore reports whether p resolves under the store (and not under its .git); rel is slash-separated.
func InsideStore(p string) (rel string, ok bool) {
	store := resolvePath(StoreDir())
	r, err := filepath.Rel(store, resolvePath(p))
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	rel = filepath.ToSlash(r)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return "", false
	}
	return rel, true
}

// resolvePath is EvalSymlinks tolerant of an absent tail: the deepest existing ancestor resolves, the rest rejoins.
func resolvePath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolvePath(parent), filepath.Base(p))
}
