// Bank health: per-file cap roll-ups and the advisories `memory doctor` + SessionStart's cached notice read.
package memory

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// bankStat is one bank's roll-up: newest mtime, per-file cap offenders, and files that could not be read.
type bankStat struct {
	Dir        string
	Updated    time.Time
	Oversized  []string
	Unreadable []string // "name: error" for each file whose read failed for a reason other than absence
}

// homeBankStat rolls up the shared home bank.
func homeBankStat() bankStat { return statBank(HomeBankDir()) }

// stat rolls up root's local bank.
func stat(root string) bankStat { return statBank(Dir(root)) }

func statBank(dir string) bankStat {
	s := bankStat{Dir: dir}
	for _, name := range BankFiles() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if !os.IsNotExist(err) {
				s.Unreadable = append(s.Unreadable, name+": "+err.Error())
			}
			continue
		}
		if capLines, ok := fileCaps[name]; ok && logicalLineCount(string(data)) > capLines {
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
	return filepath.Join(StateDir(), "doctor-notice-"+filepath.Base(Dir(root)))
}

// writeDoctorNotice caches root's first problem and a count (hot hints excluded); an empty file means checked and healthy.
func writeDoctorNotice(root string, staleDays int, now time.Time) error {
	problems := slices.DeleteFunc(doctor(root, staleDays, now), func(p string) bool {
		return strings.Contains(p, ": hot — recalled") // advice, not a problem; `memory review` still lists it
	})
	line := ""
	if len(problems) > 0 {
		line = problems[0]
		if len(problems) > 1 {
			line += " (+" + strconv.Itoa(len(problems)-1) + " more)"
		}
	}
	return paths.AtomicWrite(DoctorNoticePath(root), []byte(line), 0o600)
}

// doctor returns advisory problems for root's local bank and the shared home bank: cap offenders,
// staleness, credential-shaped lines, and access-log nominations. Never fatal — advisory only.
func doctor(root string, staleDays int, now time.Time) []string {
	var problems []string
	if !isHomeBank(root) {
		problems = append(problems, bankProblems("local", stat(root), staleDays, now)...)
	}
	problems = append(problems, bankProblems("home", homeBankStat(), staleDays, now)...)
	problems = append(problems, updateProblems(now)...)
	problems = append(problems, journalProblems(now)...)
	problems = append(problems, nativeLinkProblems(root)...)
	return append(problems, accessNominations(now)...)
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

// journalGrace leaves an in-flight commit's journal alone; older ones are ones recovery keeps skipping.
const journalGrace = 10 * time.Minute

// journalProblems names each bank journal left on disk past the grace window, so a stuck one is seen, not retried silently.
func journalProblems(now time.Time) []string {
	entries, err := os.ReadDir(JournalDir())
	if err != nil {
		return nil // a missing dir means no journals; any other read error is the next commit's to report
	}
	var problems []string
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".json") || now.Sub(fi.ModTime()) <= journalGrace {
			continue
		}
		problems = append(problems, "bank journal "+filepath.Join(JournalDir(), e.Name())+" left on disk for "+
			now.Sub(fi.ModTime()).Round(time.Minute).String()+" — recovery keeps skipping it; inspect, then delete or fix")
	}
	return problems
}

// bankProblems is one bank's advisories, each prefixed with the bank's tier so a reader knows which file to open.
func bankProblems(tier string, s bankStat, staleDays int, now time.Time) []string {
	var problems []string
	for _, f := range s.Oversized {
		problems = append(problems, tier+" "+f+": over "+strconv.Itoa(fileCaps[f])+" lines — consolidate or promote")
	}
	for _, u := range s.Unreadable {
		problems = append(problems, tier+" "+u+" — unreadable, so not checked")
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
// commitStore's refusal to stage them.
func bankCredentialFindings(dir string) []string {
	var problems []string
	for _, name := range BankFiles() {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if credentialPattern.MatchString(string(data)) {
			problems = append(problems, name+": possible credential-shaped value — scrub before promoting")
		}
	}
	return problems
}
