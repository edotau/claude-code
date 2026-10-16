// `access-trace` — PostToolUse(Read): a model read of a bank file (or its generated index) is logged to
// that bank's access log, so memory doctor's hot/cold nominations count what the model actually opened,
// not only what memory-recall injected. Fail-open — a trace is a signal, never a gate.
package hooks

import (
	"io"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
)

// AccessTrace logs the read when the path is a typed file of the global bank or of cwd's local bank.
func AccessTrace(r io.Reader) int {
	in := ParseInput(r)
	logReadAccess(in.nested("tool_input", "file_path"), in.CWD, time.Now())
	return ExitProceed
}

// logReadAccess appends `bank:<file> -- read` to the owning bank's log; any other path is ignored.
func logReadAccess(path, cwd string, now time.Time) {
	if path == "" {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil || !isBankFile(filepath.Base(abs)) {
		return
	}
	dir := filepath.Dir(abs)
	banks := []string{memory.GlobalDir()}
	if cwd != "" {
		banks = append(banks, memory.Dir(cwd))
	}
	for _, bank := range banks {
		if paths.SamePath(dir, bank) {
			memory.LogAccess(bank, now, "bank:"+filepath.Base(abs), "read")
			return
		}
	}
}

// isBankFile reports whether base is one of the typed bank files or the generated index.
func isBankFile(base string) bool {
	if base == memory.BankIndexFile {
		return true
	}
	for _, n := range memory.BankFiles() {
		if base == n {
			return true
		}
	}
	return false
}
