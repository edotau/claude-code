package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type extractFactsFixture struct {
	missing map[string]bool
	landed  map[string]bool
	calls   []string
	err     error
}

func (f *extractFactsFixture) WorktreeBranchExists(rel string, data []byte) (bool, bool, error) {
	f.calls = append(f.calls, rel)
	missing, known := f.missing[rel]
	return !missing, known, f.err
}

func (f *extractFactsFixture) ReportResultInGit(rel string, data []byte) (bool, bool, error) {
	f.calls = append(f.calls, rel)
	landed, known := f.landed[rel]
	return landed, known, f.err
}

func extractFixtureFile(t *testing.T, root, rel, body string, mod time.Time) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func extractFixtureRows(t *testing.T, result ExtractResult) map[string]ExtractRow {
	t.Helper()
	rows := map[string]ExtractRow{}
	for _, row := range result.Rows {
		if _, ok := rows[row.Path]; ok {
			t.Fatalf("duplicate row for %s", row.Path)
		}
		rows[row.Path] = row
	}
	return rows
}

func extractFixtureSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[p] = fmt.Sprintf("%s %v %s", info.ModTime().Format(time.RFC3339Nano), info.Mode(), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClassifyExtractRejectsInvalidInputs(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	extractFixtureFile(t, root, "reports/run.md", "run with a result", now.AddDate(0, 0, -20))
	factErr := errors.New("facts unavailable")
	badLogs := t.TempDir()
	extractFixtureFile(t, badLogs, "home.log", "", now)
	if err := os.Mkdir(filepath.Join(badLogs, "home.log.1"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		root string
		opts ExtractOptions
		want string
	}{
		{"clock", root, ExtractOptions{}, "Now"},
		{"store", filepath.Join(root, "absent"), ExtractOptions{Now: now}, "store"},
		{"links", root, ExtractOptions{Now: now, LinkSources: []string{filepath.Join(root, "absent")}}, "link source"},
		{"logs", root, ExtractOptions{Now: now, AccessLogs: []string{filepath.Join(root, "absent")}}, "access log"},
		{"logs-directory", root, ExtractOptions{Now: now, AccessLogs: []string{root}}, "access log"},
		{"logs-generation", root, ExtractOptions{Now: now, AccessLogs: []string{filepath.Join(badLogs, "home.log")}}, "access log"},
		{"facts", root, ExtractOptions{Now: now, Facts: &extractFactsFixture{err: factErr}}, factErr.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyExtract(tc.root, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(got.Rows) != 0 || len(got.Folders) != 0 {
				t.Fatalf("got %+v, %v; want no partial plan and error containing %q", got, err, tc.want)
			}
		})
	}
}

func extractValueBodies() map[string]string {
	return map[string]string{
		"reports/empty.md": "", "reports/whitespace.md": " \n\t",
		"reports/copy-a.md": "same exact bytes\n", "reports/copy-b.md": "same exact bytes\n",
		"reports/landed.md": "completed run result", "reports/unlanded.md": "not in git",
		"reports/unknown.md": "unidentified run", "worktrees/gone.md": "- Branch: gone\n",
		"worktrees/alive.md": "- Branch: alive\n", "worktrees/unknown.md": "branch unknown",
		"worktrees/pinned-gone.md":  "---\nmetadata:\n  pinned: true\n---\npinned gone branch",
		"notes/top-pin.md":          "---\npinned: true\n---\npinned at top level",
		"reports/copy-of-linked.md": "linked duplicate content", "reports/z-linked-copy.md": "linked duplicate content",
		"reports/linked-empty.md": "", "reports/from-bank.md": "linked by bank",
		"reports/from-catalog.md": "linked by catalog", "reports/from-plan.md": "linked by plan",
		"reports/from-external.md": "linked by external plan", "reports/from-index.md": "index is not a value source",
		"reports/young.md": "recent run", "reports/boundary.md": "exactly seven days", "reports/future.md": "future run",
		"progress.md":     "- [bank](reports/from-bank.md)\n",
		"MEMORY.md":       "[copy](reports/z-linked-copy.md) [empty](reports/linked-empty.md)\n",
		"catalog.md":      "[catalog](reports/from-catalog.md#details)\n",
		"index.md":        "[index](reports/from-index.md)\n",
		"plans/active.md": "Use `reports/from-plan.md`.\n", "reports/attachment.txt": "not markdown",
		".git/objects/ignored.md": "git internals", "locks/ignored.md": "not memory", ".hidden/ignored.md": "hidden metadata",
	}
}

func extractValueReasons() map[ExtractReason][]string {
	return map[ExtractReason][]string{
		ExtractEmpty:           {"reports/empty.md"},
		ExtractDuplicate:       {"reports/copy-b.md", "reports/copy-of-linked.md"},
		ExtractDuplicateSource: {"reports/copy-a.md"},
		ExtractMissingBranch:   {"worktrees/gone.md"}, ExtractResultInGit: {"reports/landed.md"},
		ExtractLowValue:  {"reports/whitespace.md", "reports/unlanded.md", "reports/unknown.md", "reports/boundary.md", "worktrees/alive.md", "worktrees/unknown.md", "reports/from-index.md"},
		ExtractPinned:    {"worktrees/pinned-gone.md", "notes/top-pin.md"},
		ExtractLinked:    {"reports/z-linked-copy.md", "reports/linked-empty.md", "reports/from-bank.md", "reports/from-catalog.md", "reports/from-plan.md", "reports/from-external.md"},
		ExtractYoung:     {"reports/young.md", "reports/future.md"},
		ExtractProtected: {"progress.md", "MEMORY.md", "catalog.md", "index.md", "plans/active.md", "reports/attachment.txt"},
	}
}

func assertExtractValueRows(t *testing.T, rows map[string]ExtractRow, bodies map[string]string) {
	t.Helper()
	expected := 0
	for reason, paths := range extractValueReasons() {
		for _, rel := range paths {
			expected++
			row := rows[rel]
			action := map[ExtractReason]ExtractAction{ExtractEmpty: ExtractPurge, ExtractDuplicate: ExtractPurge,
				ExtractMissingBranch: ExtractPurge, ExtractResultInGit: ExtractPurge, ExtractLowValue: ExtractRewrite}[reason]
			if action == "" {
				action = ExtractKeep
			}
			got := []any{row.Reason, row.Action, row.Size}
			want := []any{reason, action, int64(len(bodies[rel]))}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s = %+v, want %v", rel, row, want)
			}
		}
	}
	if len(rows) != expected {
		t.Errorf("rows=%d want %d", len(rows), expected)
	}
}

// Given old dead files and unknown git facts, only proven death purges; high value overrides every death rule.
func TestClassifyExtractDeadFilesAndValue(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root, planDir := t.TempDir(), t.TempDir()
	bodies := extractValueBodies()
	for rel, body := range bodies {
		extractFixtureFile(t, root, rel, body, now.AddDate(0, 0, -20))
	}
	for rel, age := range map[string]time.Duration{"reports/young.md": time.Hour, "reports/boundary.md": 7 * 24 * time.Hour, "reports/future.md": -time.Hour} {
		extractFixtureFile(t, root, rel, bodies[rel], now.Add(-age))
	}
	extractFixtureFile(t, planDir, "plan.md", "[result](/original/store/reports/from-external.md)\n", now)
	facts := &extractFactsFixture{missing: map[string]bool{"worktrees/gone.md": true, "worktrees/alive.md": false},
		landed: map[string]bool{"reports/landed.md": true, "reports/unlanded.md": false}}
	before := extractFixtureSnapshot(t, root)
	got, err := ClassifyExtract(root, ExtractOptions{Now: now, Facts: facts, AccessLogs: []string{extractEmptyLog(t, now)}, LinkSources: []string{filepath.Join(planDir, "plan.md")}, StoreAliases: []string{"/original/store"}})
	if err != nil {
		t.Fatal(err)
	}
	rows := extractFixtureRows(t, got)
	assertExtractValueRows(t, rows, bodies)
	assertExtractEvidence(t, rows)
	assertExtractFactCalls(t, facts.calls)
	if !reflect.DeepEqual(before, extractFixtureSnapshot(t, root)) {
		t.Error("classifier changed store bytes, modes or mtimes")
	}
	withoutFacts, err := ClassifyExtract(root, ExtractOptions{Now: now, AccessLogs: []string{extractEmptyLog(t, now)}})
	if err != nil {
		t.Fatal(err)
	}
	r := extractFixtureRows(t, withoutFacts)["worktrees/gone.md"]
	if values := []any{r.Action, r.Reason}; !reflect.DeepEqual(values, []any{ExtractRewrite, ExtractLowValue}) {
		t.Error("absent facts must not prove branch missing")
	}
}

func assertExtractEvidence(t *testing.T, rows map[string]ExtractRow) {
	t.Helper()
	got := []any{rows["reports/future.md"].Age, rows["reports/boundary.md"].Age,
		rows["reports/copy-of-linked.md"].DuplicateOf, rows["reports/copy-b.md"].DuplicateOf,
		rows["worktrees/unknown.md"].Signals.BranchKnown, rows["worktrees/alive.md"].Signals.BranchExists,
		rows["reports/landed.md"].Signals.ResultKnown, rows["reports/young.md"].Signals.RecallKnown}
	want := []any{time.Duration(0), 7 * 24 * time.Hour, "reports/z-linked-copy.md", "reports/copy-a.md", false, true, true, true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("evidence=%v want %v", got, want)
	}
}

func seedExtractRecallLogs(t *testing.T, logs string, now time.Time) string {
	t.Helper()
	old := now.AddDate(0, 0, -60)
	log := "home.log"
	active := accessLines(now.Add(-time.Hour), []string{contentRef("- common")}, accessRecall) +
		accessLines(now.Add(-time.Hour), []string{"aabbccdd"}, accessRecall) +
		accessLines(now.Add(-time.Hour), []string{"55667788"}, AccessSearch) +
		accessLines(now.Add(time.Hour), []string{"99aabbcc"}, accessRecall) +
		sessionLines(now.Add(-time.Hour), []string{"11223344"}, accessRecall, "fixture-session") + "malformed line\n"
	rotated := accessLines(now.AddDate(0, 0, -1), []string{"aabbccdd"}, accessRecall) +
		accessLines(now.Add(-time.Hour), []string{"aabbccdd"}, accessRecall) +
		accessLines(now.AddDate(0, 0, -40), []string{"ddeeff00"}, accessRecall)
	extractFixtureFile(t, logs, log, active, now)
	extractFixtureFile(t, logs, log+".1", rotated, old)
	return log
}

func TestClassifyExtractRecallLog(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root, logs := t.TempDir(), t.TempDir()
	old := now.AddDate(0, 0, -60)
	for name, id := range map[string]string{"recalled": "aabbccdd", "session-only": "11223344", "search-only": "55667788", "future": "99aabbcc", "cold": "ddeeff00"} {
		body := fmt.Sprintf("---\nname: %s\ndescription: useful\n---\n## Topic <!-- id:%s -->\n- common\n", name, id)
		extractFixtureFile(t, root, "notes/"+name+".md", body, old)
	}
	log := seedExtractRecallLogs(t, logs, now)
	before := extractFixtureSnapshot(t, logs)
	got, err := ClassifyExtract(root, ExtractOptions{Now: now, AccessLogs: []string{filepath.Join(logs, log)}})
	if err != nil {
		t.Fatal(err)
	}
	rows := extractFixtureRows(t, got)
	hot := rows["notes/recalled.md"]
	if got := []any{hot.Action, hot.Reason, hot.Signals.RecallDays, hot.Signals.RecalledRecently, hot.Signals.LastRecalled}; !reflect.DeepEqual(got, []any{ExtractKeep, ExtractRecalled, 2, true, now.Add(-time.Hour)}) {
		t.Errorf("recall attribution = %+v", hot)
	}
	for _, name := range []string{"session-only", "search-only", "future", "cold"} {
		row := rows["notes/"+name+".md"]
		if got := []any{row.Action, row.Signals.RecallKnown, row.Signals.RecalledRecently}; !reflect.DeepEqual(got, []any{ExtractRewrite, true, false}) {
			t.Errorf("non-recall, future or out-of-window access credited: %+v", row)
		}
	}
	if rows["notes/cold.md"].Signals.RecallDays != 1 || !reflect.DeepEqual(before, extractFixtureSnapshot(t, logs)) {
		t.Error("cold access evidence lost or access logs modified")
	}
}

func seedExtractHandoffs(t *testing.T, root string, now time.Time) int64 {
	t.Helper()
	var bytes int64
	for i := range 62 {
		body := fmt.Sprintf("handoff %d: distinct facts", i)
		age := 20
		if i >= 51 {
			age = 60
		}
		if i == 61 {
			age = 1
		}
		extractFixtureFile(t, root, fmt.Sprintf("handoffs/%03d.md", i), body, now.AddDate(0, 0, -age))
		bytes += int64(len(body))
	}
	return bytes
}

func TestClassifyExtractBudgetsAndGroups(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	bytes := seedExtractHandoffs(t, root, now)
	opts := ExtractOptions{Now: now}
	got, err := ClassifyExtract(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ClassifyExtract(root, opts)
	if !reflect.DeepEqual([]any{got, err}, []any{again, nil}) {
		t.Fatal("classifier must be deterministic", err)
	}
	idx := slices.IndexFunc(got.Folders, func(f ExtractFolderTotal) bool { return f.Folder == "handoffs" })
	if idx < 0 {
		t.Fatal("missing handoffs totals")
	}
	folder := got.Folders[idx]
	if values := []any{folder.OverBudget, folder.RewriteExcess, folder.Files, folder.Bytes, folder.Actions[ExtractKeep].Files, folder.Actions[ExtractRewrite].Files, len(folder.Groups)}; !reflect.DeepEqual(values, []any{2, 2, 62, bytes, 1, 61, 2}) {
		t.Fatalf("totals = %+v", folder)
	}
	if folder.Groups[0].YoungestAge <= folder.Groups[1].YoungestAge {
		t.Errorf("oldest group must lead: %+v", folder.Groups)
	}
	assertExtractGroups(t, folder, bytes)
}

func assertExtractGroups(t *testing.T, folder ExtractFolderTotal, bytes int64) {
	t.Helper()
	var groupBytes int64
	for _, group := range folder.Groups {
		groupBytes += group.Bytes
		if slices.Contains(group.Paths, "handoffs/061.md") {
			t.Error("high-value file entered a rewrite group")
		}
	}
	if groupBytes+folder.Actions[ExtractKeep].Bytes != bytes {
		t.Error("candidate groups and keep bytes must account for the whole folder")
	}
}

func TestClassifyExtractDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires privileges on Windows")
	}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root, outside := t.TempDir(), t.TempDir()
	extractFixtureFile(t, outside, "decoy.md", "outside the store", now)
	for _, tc := range []struct{ rel, target string }{{"escape.md", filepath.Join(outside, "decoy.md")}, {"reports", outside}} {
		if err := os.Symlink(tc.target, filepath.Join(root, tc.rel)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ClassifyExtract(root, ExtractOptions{Now: now})
	if err != nil || len(got.Rows) != 0 {
		t.Fatalf("followed symlink or failed: %+v, %v", got, err)
	}
}

func TestClassifyExtractLinkForms(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	parent := t.TempDir()
	root := filepath.Join(parent, "store")
	forms := map[string]string{
		"tilde": "[x](~/.claude/memory/reports/%s.md)",
		"home":  "$HOME/.claude/memory/reports/%s.md", "braced-home": "${HOME}/.claude/memory/reports/%s.md",
		"repo": "memory/reports/%s.md", "parent": "../memory/reports/%s.md",
		"bold": "**reports/%s.md**", "strike": "~~reports/%s.md~~",
		"bang": "`reports/%s.md`!", "line": "reports/%s.md:91", "brace": "{reports/%s.md}",
		"relative": "../store/reports/%s.md", "absolute": "[x](/original/store/reports/%s.md)",
	}
	var links strings.Builder
	for name, form := range forms {
		extractFixtureFile(t, root, "reports/"+name+".md", "", now.AddDate(0, 0, -20))
		fmt.Fprintf(&links, form+"\n", name)
	}
	source := filepath.Join(parent, "plans", "plan.md")
	extractFixtureFile(t, parent, "plans/plan.md", links.String(), now)
	got, err := ClassifyExtract(root, ExtractOptions{Now: now, LinkSources: []string{source}, StoreAliases: []string{"/original/store"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got.Rows {
		if values := []any{row.Action, row.Reason, row.Signals.LinkedBy}; !reflect.DeepEqual(values, []any{ExtractKeep, ExtractLinked, []string{source}}) {
			t.Errorf("missed link to %+v", row)
		}
	}
	if len(got.Rows) != len(forms) {
		t.Fatalf("rows=%d want %d", len(got.Rows), len(forms))
	}
}

func TestClassifyExtractPurgeBudget(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	for i := range extractFolderBudgets["handoffs"] + 1 {
		extractFixtureFile(t, root, fmt.Sprintf("handoffs/%03d.md", i), "same handoff", now.AddDate(0, 0, -20))
	}
	got, err := ClassifyExtract(root, ExtractOptions{Now: now, AccessLogs: []string{extractEmptyLog(t, now)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, folder := range got.Folders {
		if folder.Folder == "handoffs" {
			values := []int{folder.OverBudget, folder.RewriteExcess, folder.Actions[ExtractKeep].Files, folder.Actions[ExtractPurge].Files}
			if !slices.Equal(values, []int{1, 0, 1, extractFolderBudgets["handoffs"]}) {
				t.Errorf("purges did not eliminate rewrite trigger: %+v", folder)
			}
		}
	}
}

func TestClassifyExtractLargeFile(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	body := strings.Repeat("x", importMaxFile+1)
	extractFixtureFile(t, root, "archive/large.md", body, now.AddDate(0, 0, -20))
	got, err := ClassifyExtract(root, ExtractOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("got %d rows", len(got.Rows))
	}
	row := got.Rows[0]
	if values := []any{row.Size, row.Action, row.Reason}; !reflect.DeepEqual(values, []any{int64(len(body)), ExtractRewrite, ExtractLowValue}) {
		t.Errorf("large file = %+v", row)
	}
}

func extractEmptyLog(t *testing.T, now time.Time) string {
	t.Helper()
	dir := t.TempDir()
	extractFixtureFile(t, dir, "home.log", "", now)
	return filepath.Join(dir, "home.log")
}

func assertExtractFactCalls(t *testing.T, calls []string) {
	t.Helper()
	for _, rel := range []string{"worktrees/gone.md", "worktrees/alive.md", "worktrees/unknown.md", "reports/landed.md", "reports/unknown.md"} {
		if !slices.Contains(calls, rel) {
			t.Errorf("git facts not requested for %s", rel)
		}
	}
	for _, rel := range []string{"worktrees/pinned-gone.md", "reports/young.md", "reports/z-linked-copy.md"} {
		if slices.Contains(calls, rel) {
			t.Errorf("unnecessary facts requested for protected %s", rel)
		}
	}
}

func TestClassifyExtractUnknownRecall(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	for name, body := range map[string]string{"empty": "", "copy-a": "identical", "copy-b": "identical", "gone": "branch gone"} {
		extractFixtureFile(t, root, "worktrees/"+name+".md", body, now.AddDate(0, 0, -20))
	}
	facts := &extractFactsFixture{missing: map[string]bool{"worktrees/gone.md": true}}
	got, err := ClassifyExtract(root, ExtractOptions{Now: now, Facts: facts})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got.Rows {
		if row.Action != ExtractRewrite {
			t.Errorf("unknown recall must not permit purge: %+v", row)
		}
	}
	rows := extractFixtureRows(t, got)
	if rows["worktrees/gone.md"].Reason != ExtractRecallUnknown {
		t.Error("missing unknown-recall reason")
	}
}
