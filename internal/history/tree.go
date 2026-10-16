package history

import (
	"path/filepath"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// MemoryFiles is the --memory view: the shared global bank's six typed files, plus MEMORY.md from the
// harness's OWN native auto-memory dir (not every project's — that would make the watch list grow per
// project worked in). Unlike dbx's history_tree.go, this is a fixed, known set of filenames rather than
// a whole-directory walk: the bank's churn files (access.log, harvest locks) already live outside it,
// so there is no crawl to exclude them from.
func MemoryFiles() []HistoryFile {
	bank := memory.GlobalDir()
	out := make([]HistoryFile, 0, len(memory.BankFiles())+1)
	for _, name := range memory.BankFiles() {
		out = append(out, HistoryFile{Label: name, Path: filepath.Join(bank, name)})
	}
	native := memory.NativeDir(paths.ConfigDir())
	out = append(out, HistoryFile{
		Label: memory.NativeIndexFile,
		Path:  filepath.Join(native, memory.NativeIndexFile),
	})
	return out
}
