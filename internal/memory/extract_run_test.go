package memory

import (
	"bytes"
	"encoding/json"
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
