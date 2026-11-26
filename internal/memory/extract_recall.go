package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// extractRecallLimit is the share of served entries a batch may leave without a hit or successor.
const extractRecallLimit = 0.02

// ExtractRecallReport carries the recall check's numbers; Bytes and Scan are the store's index size and scan time.
type ExtractRecallReport struct {
	Queried, Lost           int
	BytesBefore, BytesAfter int64
	ScanBefore, ScanAfter   time.Duration
}

// String is the one line dream prints.
func (r ExtractRecallReport) String() string {
	return fmt.Sprintf("recall check: %d/%d served entries lost; store %d -> %d bytes; scan %s -> %s",
		r.Lost, r.Queried, r.BytesBefore, r.BytesAfter, r.ScanBefore, r.ScanAfter)
}

// ErrExtractNoAccessLog is the typed refusal when there is no recall history to check against.
var ErrExtractNoAccessLog = errors.New("extract recall: no access log")

// CheckExtractRecall refuses a batch that, with the ledger's 30 days of losses, passes 2% of served entries.
func CheckExtractRecall(logs []string, ledger string, sources map[string][]byte, ans ExtractRewriteAnswer, now time.Time) (ExtractRecallReport, error) {
	if len(logs) == 0 {
		return ExtractRecallReport{}, ErrExtractNoAccessLog
	}
	served, err := extractServed(logs, now)
	if err != nil {
		return ExtractRecallReport{}, err
	}
	report := extractLostHits(served, sources, ans, now)
	prior, err := extractLedgerLost(ledger, now)
	if err != nil {
		return report, err
	}
	total := report.Lost + prior
	if report.Lost > 0 && float64(total)/float64(len(served)) > extractRecallLimit {
		return report, extractChecked{fmt.Errorf("extract recall: %d of %d served entries lose their hit in 30 days (limit 2%%)", total, len(served))}
	}
	return report, nil
}

func extractLostHits(served map[string]bool, sources map[string][]byte, ans ExtractRewriteAnswer, now time.Time) ExtractRecallReport {
	succeeded := map[string]bool{}
	for _, entry := range ans.Entries {
		for _, source := range entry.Sources {
			succeeded[source] = true
		}
	}
	report := ExtractRecallReport{}
	for source, data := range sources {
		refs := newExtractFile(source, data, now, now).refs
		if !slices.ContainsFunc(refs, func(ref string) bool { return served[ref] }) {
			continue
		}
		report.Queried++
		if !succeeded[source] {
			report.Lost++
		}
	}
	return report
}

// extractLedgerLost sums the losses of batches applied in the last 30 days ("<RFC3339> <lost>" lines).
func extractLedgerLost(ledger string, now time.Time) (int, error) {
	data, err := os.ReadFile(ledger)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("extract recall ledger: %w", err)
	}
	sum := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		stamp, count, _ := strings.Cut(line, " ")
		ts, tsErr := time.Parse(time.RFC3339, stamp)
		if tsErr == nil && strings.HasPrefix(count, extractRefusedTag) {
			continue
		}
		n, convErr := strconv.Atoi(count)
		if line != "" && (tsErr != nil || convErr != nil) {
			return 0, fmt.Errorf("extract recall ledger: malformed line %q", line)
		}
		if line != "" && ts.After(now.AddDate(0, 0, -accessRotateDays)) {
			sum += n
		}
	}
	return sum, nil
}

// RecordExtractLoss appends a batch's lost-hit count; the rewrite is fsynced with its directory before it returns.
func RecordExtractLoss(ledger string, now time.Time, lost int) error {
	return appendExtractLedger(ledger, fmt.Sprintf("%s %d\n", now.UTC().Format(time.RFC3339), lost))
}

func appendExtractLedger(ledger, line string) error {
	data, err := os.ReadFile(ledger)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var kept strings.Builder
	cutoff := time.Now().AddDate(0, 0, -accessRotateDays)
	for row := range strings.SplitSeq(string(data), "\n") {
		stamp, _, _ := strings.Cut(row, " ")
		at, err := time.Parse(time.RFC3339, stamp)
		if row != "" && (err != nil || !at.Before(cutoff)) {
			kept.WriteString(row + "\n")
		}
	}
	kept.WriteString(line)
	return paths.AtomicWrite(ledger, []byte(kept.String()), 0o600)
}

// extractRefusedTag marks a ledger line "<RFC3339> refused <group hash> <reason>" for a group the checks refused.
const extractRefusedTag = "refused "

// RecordExtractRefusal notes that the checks refused the group with this source hash.
func RecordExtractRefusal(ledger string, now time.Time, hash string, reason error) error {
	why := strings.Join(strings.Fields(reason.Error()), " ")
	return appendExtractLedger(ledger, fmt.Sprintf("%s %s%s %s\n", now.UTC().Format(time.RFC3339), extractRefusedTag, hash, why))
}

// ExtractRefusals maps each refused group hash to its latest reason.
func ExtractRefusals(ledger string) (map[string]string, error) {
	data, err := os.ReadFile(ledger)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("extract recall ledger: %w", err)
	}
	out := map[string]string{}
	cutoff := time.Now().AddDate(0, 0, -accessRotateDays)
	for line := range strings.SplitSeq(string(data), "\n") {
		stamp, rest, _ := strings.Cut(line, " ")
		at, err := time.Parse(time.RFC3339, stamp)
		if body, ok := strings.CutPrefix(rest, extractRefusedTag); ok && err == nil && !at.Before(cutoff) {
			hash, reason, _ := strings.Cut(body, " ")
			out[hash] = reason
		}
	}
	return out, nil
}

// extractGroupHash fingerprints a sorted file set by each file's content (its SHA-256) or given digest string.
func extractGroupHash[V []byte | string](files map[string]V) string {
	hash := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		digest, ok := any(files[rel]).(string)
		if !ok {
			digest = textutil.SHA256Hex(any(files[rel]).([]byte))
		}
		fmt.Fprintf(hash, "%d:%s:%s\n", len(rel), rel, digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func extractServed(logs []string, now time.Time) (map[string]bool, error) {
	served := map[string]bool{}
	cutoff := now.AddDate(0, 0, -accessRotateDays)
	for _, log := range logs {
		err := readExtractAccess(log, func(ts time.Time, entry, _ string) {
			if ts.After(cutoff) && !ts.After(now) {
				served[entry] = true
			}
		})
		if err != nil {
			return nil, err
		}
	}
	return served, nil
}

// ExtractRecallMeasure returns the .md bytes under dir and the time to read them all.
func ExtractRecallMeasure(dir string) (int64, time.Duration, error) {
	start := time.Now()
	var size int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		data, err := os.ReadFile(p)
		size += int64(len(data))
		return err
	})
	return size, time.Since(start), err
}

// Verdict fails a run whose store did not get smaller or whose scan got slower.
func (r ExtractRecallReport) Verdict() error {
	if r.BytesAfter > r.BytesBefore || r.ScanAfter > 2*r.ScanBefore+10*time.Millisecond {
		return fmt.Errorf("extract recall: store grew or recall slowed (%s)", r)
	}
	return nil
}
