package memory

import "slices"

// bankFiles are the six typed files a bank carries (order = injection order).
var bankFiles = []string{
	"projectContext.md", "activeContext.md", "progress.md",
	"decisionLog.md", "conventions.md", "sessionHistory.md",
}

// BankFiles exports the six typed filenames for callers outside this package (e.g. internal/history)
// that must watch them without duplicating the list.
func BankFiles() []string { return slices.Clone(bankFiles) }

// SessionHistoryMaxEntries rotates the oldest sessionHistory entries to <bank>/archive/.
const SessionHistoryMaxEntries = 30
