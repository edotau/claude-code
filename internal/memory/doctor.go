// Bank health: per-file cap roll-ups and the advisories `memory doctor` + SessionStart's cached notice read.
package memory

import (
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/memory/recall"

	"github.com/edotau/claude-code/internal/memstore"
	"github.com/edotau/claude-code/internal/paths"
)

// BankStat is one bank's roll-up: file count, total lines, newest mtime, per-file cap offenders.
type BankStat struct {
	Dir       string
	Files     int
	Lines     int
	Updated   time.Time
	Oversized []string
}

// HomeBankStat rolls up the shared home bank.
func HomeBankStat() BankStat { return statBank(memstore.HomeBankDir()) }

// Stat rolls up root's local bank.
func Stat(root string) BankStat { return statBank(memstore.Dir(root)) }

func statBank(dir string) BankStat {
	s := BankStat{Dir: dir}
	for _, name := range bank.BankFiles() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		s.Files++
		s.Lines += bank.LineCount(string(data))
		if capLines, ok := FileCaps[name]; ok && bank.LogicalLineCount(string(data)) > capLines {
			s.Oversized = append(s.Oversized, name)
		}
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.ModTime().After(s.Updated) {
			s.Updated = fi.ModTime()
		}
	}
	return s
}

// DefaultStaleDays is the doctor's bank-staleness horizon.
const DefaultStaleDays = 90

// DoctorNoticePath is the per-store cached doctor summary read at SessionStart.
func DoctorNoticePath(root string) string {
	return filepath.Join(memstore.StateDir(), "doctor-notice-"+filepath.Base(memstore.Dir(root)))
}

// WriteDoctorNotice caches root's first problem and a count; an empty file means checked and healthy.
func WriteDoctorNotice(root string, staleDays int, now time.Time) error {
	problems := Doctor(root, staleDays, now)
	line := ""
	if len(problems) > 0 {
		line = problems[0]
		if len(problems) > 1 {
			line += " (+" + strconv.Itoa(len(problems)-1) + " more)"
		}
	}
	return paths.AtomicWrite(DoctorNoticePath(root), []byte(line), 0o600)
}

// Doctor returns advisory problems for root's local bank and the shared home bank: cap offenders,
// staleness, credential-shaped lines, and access-log nominations. Never fatal — advisory only.
func Doctor(root string, staleDays int, now time.Time) []string {
	var problems []string
	if !memstore.IsHomeBank(root) {
		problems = append(problems, bankProblems("local", Stat(root), staleDays, now)...)
	}
	problems = append(problems, bankProblems("home", HomeBankStat(), staleDays, now)...)
	problems = append(problems, updateProblems(now)...)
	problems = append(problems, memstore.NativeLinkProblems(root)...)
	return append(problems, recall.AccessNominations(now)...)
}

// updateProblems surfaces receipts that need a human: a failed update, or one still "pending" past the
// grace window (the worker died before reporting) — otherwise they pile up unreported.
func updateProblems(now time.Time) []string {
	var problems []string
	for _, rc := range readReceipts() {
		id, r := rc.id, rc.r
		// A lost "pending" receipt is a bug to chase whenever it happened; a plain failure ages out after a week.
		if !r.NeedsAttention(now) || (r.Status == UpdateFailed && now.Sub(r.Time) > 7*24*time.Hour) {
			continue
		}
		problems = append(problems, "update "+headID(id)+" "+r.Status+" since "+r.Time.Format("2006-01-02")+
			" — see "+r.Log+" (claude-code memory update --session "+id+")")
	}
	return problems
}

// bankProblems is one bank's advisories, each prefixed with the bank's tier so a reader knows which file to open.
func bankProblems(tier string, s BankStat, staleDays int, now time.Time) []string {
	var problems []string
	for _, f := range s.Oversized {
		problems = append(problems, tier+" "+f+": over "+strconv.Itoa(FileCaps[f])+" lines — consolidate or promote")
	}
	if !s.Updated.IsZero() && s.Updated.Before(now.AddDate(0, 0, -staleDays)) {
		problems = append(problems, tier+" bank untouched since "+s.Updated.Format("2006-01-02")+
			" (>"+strconv.Itoa(staleDays)+"d) — stale")
	}
	for _, p := range bankCredentialFindings(s.Dir) {
		problems = append(problems, tier+" "+p)
	}
	return problems
}

// bankCredentialFindings scans typed files for credential-shaped values — the visible surface for
// promoteFile's silent refusal.
func bankCredentialFindings(dir string) []string {
	var problems []string
	for _, name := range bank.BankFiles() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if bank.CredentialPattern.MatchString(string(data)) {
			problems = append(problems, name+": possible credential-shaped value — scrub before promoting")
		}
	}
	return problems
}
