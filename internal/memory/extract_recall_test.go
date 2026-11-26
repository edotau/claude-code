package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractRecallCheck(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	log := filepath.Join(dir, "access.log")
	stamp := now.Add(-time.Hour).Format(time.RFC3339)
	lines := stamp + " -- reports/a.md -- " + AccessSearch + "\n" + stamp + " -- reports/b.md -- " + AccessSearch + "\n"
	if err := os.WriteFile(log, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{"reports/a.md": []byte("a\n"), "reports/b.md": []byte("b\n")}
	keep := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "T", Text: "ab", Sources: []string{"reports/a.md", "reports/b.md"}}}, Purge: []string{}}
	ledger := filepath.Join(dir, "loss.log")
	report, err := CheckExtractRecall([]string{log}, ledger, sources, keep, now)
	if err != nil || report.Queried != 2 || report.Lost != 0 {
		t.Fatalf("kept successors: report=%+v err=%v", report, err)
	}
	drop := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "T", Text: "a", Sources: []string{"reports/a.md"}}}, Purge: []string{"reports/b.md"}}
	if report, err := CheckExtractRecall([]string{log}, ledger, sources, drop, now); err == nil || report.Lost != 1 || !strings.Contains(err.Error(), "limit 2%") {
		t.Fatalf("dropped queried entry: report=%+v err=%v", report, err)
	}
	checkExtractRecallVerdict(t)
}

func TestExtractRecallCumulativeAndNoLog(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	log, ledger := filepath.Join(dir, "access.log"), filepath.Join(dir, "loss.log")
	var lines strings.Builder
	for i := range 100 {
		fmt.Fprintf(&lines, "%s -- reports/s%d.md -- %s\n", now.Add(-time.Hour).Format(time.RFC3339), i, AccessSearch)
	}
	if err := os.WriteFile(log, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	batch := func(a, b string) (ExtractRecallReport, error) {
		sources := map[string][]byte{a: []byte("a\n"), b: []byte("b\n")}
		ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "T", Text: "x", Sources: []string{"other.md"}}}, Purge: []string{a, b}}
		return CheckExtractRecall([]string{log}, ledger, sources, ans, now)
	}
	report, err := batch("reports/s0.md", "reports/s1.md")
	if err != nil || report.Lost != 2 {
		t.Fatalf("first batch (2%% of served): report=%+v err=%v", report, err)
	}
	if err := RecordExtractLoss(ledger, now.Add(-time.Hour), report.Lost); err != nil {
		t.Fatal(err)
	}
	if err := RecordExtractRefusal(ledger, now, "group", errors.New("check refused")); err != nil {
		t.Fatal(err)
	}
	if _, err := batch("reports/s2.md", "reports/s3.md"); err == nil || !strings.Contains(err.Error(), "4 of 100") {
		t.Fatalf("second batch must pass the cumulative limit: %v", err)
	}
	if _, err := CheckExtractRecall(nil, ledger, nil, ExtractRewriteAnswer{}, now); !errors.Is(err, ErrExtractNoAccessLog) {
		t.Fatalf("no access log err = %v", err)
	}
}

func checkExtractRecallVerdict(t *testing.T) {
	t.Helper()
	if size, _, err := ExtractRecallMeasure(t.TempDir()); err != nil || size != 0 {
		t.Fatalf("empty store measure = %d, %v", size, err)
	}
	if err := (ExtractRecallReport{BytesBefore: 10, BytesAfter: 5}).Verdict(); err != nil {
		t.Fatalf("smaller store: %v", err)
	}
	if err := (ExtractRecallReport{BytesBefore: 5, BytesAfter: 10}).Verdict(); err == nil {
		t.Fatal("grown store passed")
	}
}

func TestExtractRefusalsExpireAndLedgerPrunes(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "loss.log")
	now := time.Now()
	if err := RecordExtractRefusal(ledger, now.AddDate(0, 0, -accessRotateDays-1), "old", errors.New("old recall budget")); err != nil {
		t.Fatal(err)
	}
	if rows, err := ExtractRefusals(ledger); err != nil || len(rows) != 0 {
		t.Fatalf("expired refusal still blocks a retry: %v, %v", rows, err)
	}
	if err := RecordExtractRefusal(ledger, now, "current", errors.New("current check")); err != nil {
		t.Fatal(err)
	}
	rows, err := ExtractRefusals(ledger)
	if err != nil || len(rows) != 1 || rows["current"] != "current check" {
		t.Fatalf("current refusal lost: %v, %v", rows, err)
	}
	data, err := os.ReadFile(ledger)
	if err != nil || strings.Contains(string(data), "old recall budget") {
		t.Fatalf("old ledger record was not pruned: %q, %v", data, err)
	}
}
