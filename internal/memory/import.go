package memory

import (
	"path/filepath"
	"slices"
	"strings"
)

// importMaxFile caps one file a scan reads.
const importMaxFile = 4 << 20

// skipImportDir reports a store folder a bank scan never enters: git, locks and anything outside the flat folders.
func skipImportDir(rel, name string) bool {
	return name == ".git" || name == "locks" || name == ".locks" ||
		rel != "." && !slices.Contains(flatSubs, strings.Split(rel, "/")[0])
}

// foldSkips reports the generated router and lock files, which do not travel.
func foldSkips(rel string) bool {
	return rel == routerFile || strings.HasSuffix(rel, ".lock") || filepath.Base(rel) == ".postupdate"
}
