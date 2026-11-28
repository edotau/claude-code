package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
)

func extractRunEqual(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func extractRunOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func extractRunFails(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Fatal("unsafe operation succeeded")
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("want %q in %v", contains, err)
	}
}

func extractTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	out, err := gitOut(root, args...)
	extractRunOK(t, err)
	return strings.TrimSpace(out)
}

func extractRunFixture(t *testing.T) (string, ExtractOptions) {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	root := paths.MemoryStoreDir()
	now := time.Now()
	for rel, body := range map[string]string{"reports/a.md": "same fixture\n", "reports/b.md": "same fixture\n", "reports/empty.md": "", "notes/keep.md": "keep fixture\n"} {
		extractFixtureFile(t, root, rel, body, now.Add(-40*24*time.Hour))
	}
	write(t, accessLogPath(), "")
	extractRunOK(t, os.MkdirAll(filepath.Join(paths.ConfigDir(), "plans"), 0o700))
	extractTestGit(t, root, "config", "user.name", "Fixture")
	extractTestGit(t, root, "config", "user.email", "fixture@example.invalid")
	extractTestGit(t, root, "config", "maintenance.auto", "false")
	extractTestGit(t, root, "config", "gc.auto", "0")
	extractTestGit(t, root, "add", "--all")
	extractTestGit(t, root, "commit", "-m", "seed extract fixture")
	return root, ExtractOptions{Now: now, AccessLogs: []string{accessLogPath()}}
}

func extractRunJSON(t *testing.T, o ExtractRunOptions) ExtractRunReport {
	t.Helper()
	o.Format = "json"
	var out bytes.Buffer
	extractRunOK(t, RunExtract(o, &out))
	var rep ExtractRunReport
	extractRunOK(t, json.Unmarshal(out.Bytes(), &rep))
	return rep
}

func TestExtractRunLifecycle(t *testing.T) {
	root, _ := extractRunFixture(t)
	before := extractFixtureSnapshot(t, root)
	out := filepath.Join(t.TempDir(), "extract.json")
	dry := extractRunJSON(t, ExtractRunOptions{DryRun: true, Purge: true, Out: out})
	extractRunEqual(t, extractFixtureSnapshot(t, root), before)
	data, err := os.ReadFile(out)
	extractRunOK(t, err)
	var saved ExtractResult
	extractRunOK(t, json.Unmarshal(data, &saved))
	extractRunEqual(t, len(saved.Rows), len(dry.Rows))
	head := extractTestGit(t, root, "rev-parse", "HEAD")
	purged := extractRunJSON(t, ExtractRunOptions{Purge: true})
	count := 0
	for _, row := range dry.Rows {
		_, err := os.Stat(filepath.Join(root, row.Path))
		if row.Action == ExtractPurge {
			count++
			extractRunEqual(t, os.IsNotExist(err), true)
		} else {
			extractRunOK(t, err)
		}
	}
	extractRunEqual(t, count, 2)
	extractRunEqual(t, purged.Purged, ExtractTotal{Files: count, Bytes: int64(len("same fixture\n"))})
	extractRunEqual(t, extractTestGit(t, root, "rev-parse", "HEAD^"), head)
	committed, err := extractCleanHead(root)
	extractRunOK(t, err)
	again := extractRunJSON(t, ExtractRunOptions{Purge: true})
	extractRunEqual(t, again.Purged, ExtractTotal{})
	extractRunEqual(t, again.RemainingSHA256, purged.RemainingSHA256)
	extractRunEqual(t, len(again.RemainingSHA256), 64)
	extractRunEqual(t, extractTestGit(t, root, "rev-parse", "HEAD"), committed)
}

func TestExtractRunRefusals(t *testing.T) {
	root, opts := extractRunFixture(t)
	result, err := ClassifyExtract(root, opts)
	extractRunOK(t, err)
	head, err := extractCleanHead(root)
	extractRunOK(t, err)
	write(t, filepath.Join(root, "dirty.md"), "uncommitted fixture")
	_, err = purgeExtract(root, result, head)
	extractRunFails(t, err, "dirty")
	extractRunOK(t, os.Remove(filepath.Join(root, "dirty.md")))
	before := extractFixtureSnapshot(t, root)
	for i := range result.Rows {
		if result.Rows[i].Action == ExtractPurge {
			result.Rows[i].SHA256 = "changed fixture digest"
			break
		}
	}
	_, err = purgeExtract(root, result, head)
	extractRunFails(t, err, "hash changed")
	extractRunEqual(t, extractFixtureSnapshot(t, root), before)
	extractRunFails(t, RunExtract(ExtractRunOptions{DryRun: true, Out: filepath.Join(root, "extract.json")}, &bytes.Buffer{}), "outside")
	alias := filepath.Join(t.TempDir(), "alias")
	extractRunOK(t, os.Symlink(root, alias))
	extractRunFails(t, extractOutputOutsideStore(root, filepath.Join(alias, "out.json")), "outside")
	unknown, err := ClassifyExtract(root, ExtractOptions{Now: opts.Now})
	extractRunOK(t, err)
	for _, row := range unknown.Rows {
		extractRunEqual(t, row.Action == ExtractPurge, false)
	}
}

func TestExtractRunRollback(t *testing.T) {
	root, opts := extractRunFixture(t)
	result, err := ClassifyExtract(root, opts)
	extractRunOK(t, err)
	head := extractTestGit(t, root, "rev-parse", "HEAD")
	removals, err := extractRemovals(root, result, head)
	extractRunOK(t, err)
	before := extractFixtureSnapshot(t, root)
	extractRunFails(t, extractApplyRemovals(root, removals, head, "invalid-fixture-commit"), "update-ref")
	extractRunEqual(t, extractFixtureSnapshot(t, root), before)
	extractRunEqual(t, extractTestGit(t, root, "rev-parse", "HEAD"), head)
}

func extractAssertFact(t *testing.T, value, known bool, err error, wantValue, wantKnown bool) {
	t.Helper()
	extractRunOK(t, err)
	extractRunEqual(t, [2]bool{value, known}, [2]bool{wantValue, wantKnown})
}

func TestExtractGitFactsEvidence(t *testing.T) {
	root, _ := extractRunFixture(t)
	head := extractTestGit(t, root, "rev-parse", "HEAD")
	facts := ExtractGitFacts{Host: "fixture-host"}
	meta := "---\nrepository: " + root + "\nhost: fixture-host\n"
	extractTestGit(t, root, "branch", "fixture-present")
	exists, known, err := facts.WorktreeBranchExists("", []byte(meta+"branch: fixture-present\n---\n"))
	extractAssertFact(t, exists, known, err, true, true)
	exists, known, err = facts.WorktreeBranchExists("", []byte(meta+"branch: fixture-missing\n---\n"))
	extractAssertFact(t, exists, known, err, false, true)
	landed, known, err := facts.ReportResultInGit("", []byte(meta+"result_commit: "+head+"\n---\n"))
	extractAssertFact(t, landed, known, err, true, true)
	tree := extractTestGit(t, root, "rev-parse", "HEAD^{tree}")
	unlanded := extractTestGit(t, root, "commit-tree", tree, "-p", head, "-m", "unlanded fixture result")
	landed, known, err = facts.ReportResultInGit("", []byte(meta+"result_commit: "+unlanded+"\n---\n"))
	extractAssertFact(t, landed, known, err, false, false)
	for _, data := range []string{meta + "branch: fixture-missing\nhost: fixture-peer\n---\n", "no metadata, only prose", "---\nbranch: fixture-missing\n---\n", meta + "result_commit: HEAD\n---\n", "---\nrepository: " + t.TempDir() + "\nbranch: fixture-missing\n---\n"} {
		exists, known, err := facts.WorktreeBranchExists("", []byte(data))
		extractAssertFact(t, exists, known, err, false, false)
		landed, known, err := facts.ReportResultInGit("", []byte(data))
		extractAssertFact(t, landed, known, err, false, false)
	}
}

func TestExtractRunLinkedPlans(t *testing.T) {
	root, _ := extractRunFixture(t)
	plans := filepath.Join(paths.ConfigDir(), "plans")
	target := t.TempDir()
	write(t, filepath.Join(target, "topic.md"), filepath.Join(root, "reports/b.md"))
	extractRunOK(t, os.Symlink(target, filepath.Join(plans, "linked")))
	extractRunOK(t, os.Symlink(plans, filepath.Join(target, "cycle")))
	dry := extractRunJSON(t, ExtractRunOptions{DryRun: true})
	rows := extractFixtureRows(t, dry.ExtractResult)
	extractRunEqual(t, rows["reports/b.md"].Action, ExtractKeep)
	extractRunEqual(t, rows["reports/b.md"].Reason, ExtractLinked)
	extractRunEqual(t, len(rows["reports/b.md"].Signals.LinkedBy), 1)
}

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
