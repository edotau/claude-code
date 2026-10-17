// Bank health: per-file cap roll-ups and the advisories `memory doctor` + SessionStart's cached notice read.
package memory

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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

// GlobalStat rolls up the shared global bank.
func GlobalStat() BankStat { return statBank(GlobalDir()) }

// Stat rolls up root's local bank.
func Stat(root string) BankStat { return statBank(Dir(root)) }

func statBank(dir string) BankStat {
	s := BankStat{Dir: dir}
	for _, name := range bankFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		s.Files++
		lines := lineCount(string(data))
		s.Lines += lines
		if capLines, ok := FileCaps[name]; ok && lines > capLines {
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

// DoctorNoticePath is the cached one-line doctor summary SessionStart reads instead of running Doctor
// (which tokenizes the global bank + access log) on the launch path.
func DoctorNoticePath() string { return filepath.Join(StateDir(), "doctor-notice") }

// WriteDoctorNotice runs Doctor for root and caches its first problem (with a +N-more count) as one line;
// an empty file means "checked, nothing wrong", distinct from no file at all.
func WriteDoctorNotice(root string, staleDays int, now time.Time) error {
	problems := Doctor(root, staleDays, now)
	line := ""
	if len(problems) > 0 {
		line = problems[0]
		if len(problems) > 1 {
			line += " (+" + strconv.Itoa(len(problems)-1) + " more)"
		}
	}
	return paths.AtomicWrite(DoctorNoticePath(), []byte(line), 0o600)
}

// credentialPattern is a coarse credential-shaped tripwire, not a full secrets scanner.
var credentialPattern = regexp.MustCompile(`sk-[A-Za-z0-9]{10,}|ghp_[A-Za-z0-9]{10,}|dapi[0-9a-f]{10,}|AKIA[A-Z0-9]{10,}|-----BEGIN`)

// Doctor returns advisory problems for root's local bank and the shared global bank: cap offenders,
// staleness, credential-shaped lines, and access-log nominations. Never fatal — advisory only.
func Doctor(root string, staleDays int, now time.Time) []string {
	var problems []string
	if !IsGlobal(root) {
		problems = append(problems, bankProblems("local", Stat(root), staleDays, now)...)
	}
	problems = append(problems, bankProblems("global", GlobalStat(), staleDays, now)...)
	problems = append(problems, harvestProblems(now)...)
	return append(problems, AccessNominations(now)...)
}

// harvestProblems surfaces receipts that need a human: a failed harvest, or one still "pending" past the
// grace window (the worker died before reporting) — otherwise `memory stats` counts them forever.
func harvestProblems(now time.Time) []string {
	var problems []string
	matches, _ := filepath.Glob(filepath.Join(StateDir(), "harvest-result-*"))
	for _, p := range matches {
		id := strings.TrimPrefix(filepath.Base(p), "harvest-result-")
		r, ok := ReadHarvestResult(id)
		// A lost "pending" receipt is a bug to chase whenever it happened; a plain failure ages out after a week.
		if !ok || !r.NeedsAttention(now) || (r.Status == HarvestFailed && now.Sub(r.Time) > 7*24*time.Hour) {
			continue
		}
		short := id
		if len(short) > 8 {
			short = short[:8]
		}
		problems = append(problems, "harvest "+short+" "+r.Status+" since "+r.Time.Format("2006-01-02")+
			" — see "+r.Log+" (claude-code memory stats)")
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
	for _, name := range bankFiles {
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
