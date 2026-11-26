package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
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
	active := accessLines(now.Add(-time.Hour), []string{contentRef("- common")}, accessRecall, "") +
		accessLines(now.Add(-time.Hour), []string{"aabbccdd"}, accessRecall, "") +
		accessLines(now.Add(-time.Hour), []string{"55667788"}, AccessSearch, "") +
		accessLines(now.Add(time.Hour), []string{"99aabbcc"}, accessRecall, "") +
		accessLines(now.Add(-time.Hour), []string{"11223344"}, accessRecall, "fixture-session") + "malformed line\n"
	rotated := accessLines(now.AddDate(0, 0, -1), []string{"aabbccdd"}, accessRecall, "") +
		accessLines(now.Add(-time.Hour), []string{"aabbccdd"}, accessRecall, "") +
		accessLines(now.AddDate(0, 0, -40), []string{"ddeeff00"}, accessRecall, "")
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

func extractRewriteFixture(t *testing.T) (string, ExtractGroup) {
	t.Helper()
	root := t.TempDir()
	group := ExtractGroup{Action: ExtractRewrite}
	for i := range 5 {
		rel := fmt.Sprintf("handoffs/%03d.md", i)
		extractFixtureFile(t, root, rel, fmt.Sprintf("handoff %d: decision and open task", i), time.Now())
		group.Paths = append(group.Paths, rel)
	}
	return root, group
}

func extractRewriteFake(t *testing.T, root string, group ExtractGroup, answer []byte, calls *int) func(context.Context, string, string, string, string) (string, string, error) {
	t.Helper()
	defaultModel, err := FallbackModel("")
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, prompt, workdir, schema, model string) (string, string, error) {
		*calls++
		if got, want := []any{workdir == root, filepath.IsAbs(workdir), strings.HasPrefix(filepath.Base(workdir), "memory-extract-"), schema, model},
			[]any{false, true, true, ExtractRewriteSchema(), defaultModel}; !reflect.DeepEqual(got, want) {
			t.Errorf("dispatch workdir=%q schema=%q model=%q", workdir, schema, model)
		}
		for i, rel := range group.Paths {
			if got := []bool{strings.Contains(prompt, rel), strings.Contains(prompt, fmt.Sprintf("handoff %d: decision and open task", i))}; !reflect.DeepEqual(got, []bool{true, true}) {
				t.Errorf("prompt missing %s content", rel)
			}
		}
		if !strings.Contains(prompt, "owner's quotes, decisions, and open tasks verbatim") {
			t.Error("prompt does not preserve protected words")
		}
		return string(answer), "fake-pi", nil
	}
}

func TestExtractRewriteConsolidatesHandoffs(t *testing.T) {
	root, group := extractRewriteFixture(t)
	before := extractFixtureSnapshot(t, root)
	answer, err := json.Marshal(ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Handoffs", Text: "Five decisions and open tasks", Sources: group.Paths}}, Purge: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	old := CodeModeRun
	defer func() { CodeModeRun = old }()
	calls := 0
	CodeModeRun = extractRewriteFake(t, root, group, answer, &calls)
	got, err := RewriteExtractGroup(context.Background(), root, group, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(got.Entries) != 1 {
		t.Fatalf("rewrite=%+v calls=%d err=%v", got, calls, err)
	}
	if !slices.Equal(got.Entries[0].Sources, group.Paths) {
		t.Errorf("sources=%v, want %v", got.Entries[0].Sources, group.Paths)
	}
	if !reflect.DeepEqual(before, extractFixtureSnapshot(t, root)) {
		t.Error("rewrite changed the store")
	}
}

func TestExtractRewriteRejectsOutsideSource(t *testing.T) {
	root, group := extractRewriteFixture(t)
	before := extractFixtureSnapshot(t, root)
	old := CodeModeRun
	defer func() { CodeModeRun = old }()
	calls := 0
	CodeModeRun = func(context.Context, string, string, string, string) (string, string, error) {
		calls++
		return `{"entries":[{"title":"bad","text":"bad","sources":["handoffs/outside.md"]}],"purge":[]}`, "fake-pi", nil
	}
	got, err := RewriteExtractGroup(context.Background(), root, group, "")
	if err == nil || !strings.Contains(err.Error(), "outside the group") || calls != 1 || len(got.Entries) != 0 {
		t.Fatalf("rewrite=%+v calls=%d err=%v", got, calls, err)
	}
	if !reflect.DeepEqual(before, extractFixtureSnapshot(t, root)) {
		t.Error("invalid rewrite changed the store")
	}
}

func TestExtractRewriteRejectsInvalidCoverage(t *testing.T) {
	for _, tc := range []struct{ name, answer, want string }{
		{"repeated", `{"entries":[{"title":"a","text":"b","sources":["handoffs/a.md"]}],"purge":["handoffs/a.md","handoffs/b.md"]}`, "repeated"},
		{"missing", `{"entries":[{"title":"a","text":"b","sources":["handoffs/a.md"]}],"purge":[]}`, "unaccounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed := map[string]bool{"handoffs/a.md": true, "handoffs/b.md": true}
			got, err := decodeExtractRewrite(tc.answer, allowed)
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(got.Entries) != 0 {
				t.Fatalf("rewrite=%+v err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestExtractRewriteRejectsInvalidGroups(t *testing.T) {
	root, group := extractRewriteFixture(t)
	old := CodeModeRun
	defer func() { CodeModeRun = old }()
	calls := 0
	CodeModeRun = func(context.Context, string, string, string, string) (string, string, error) {
		calls++
		return "", "fake-pi", nil
	}
	for _, tc := range []struct {
		name  string
		group ExtractGroup
		model string
		want  string
	}{
		{"action", ExtractGroup{Action: ExtractPurge, Paths: group.Paths}, "", "rewrite group"},
		{"empty", ExtractGroup{Action: ExtractRewrite}, "", "rewrite group"},
		{"traversal", ExtractGroup{Action: ExtractRewrite, Paths: []string{"../outside.md"}}, "", "invalid group path"},
		{"duplicate", ExtractGroup{Action: ExtractRewrite, Paths: []string{group.Paths[0], group.Paths[0]}}, "", "invalid group path"},
		{"missing", ExtractGroup{Action: ExtractRewrite, Paths: []string{"handoffs/missing.md"}}, "", "extract rewrite read"},
		{"model", group, "unknown-model", "not in the catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RewriteExtractGroup(context.Background(), root, tc.group, tc.model)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v, want %q", err, tc.want)
			}
		})
	}
	CodeModeRun = nil
	if _, err := RewriteExtractGroup(context.Background(), root, group, ""); err == nil || !strings.Contains(err.Error(), "no code-mode agent") {
		t.Errorf("nil runner err=%v", err)
	}
	if calls != 0 {
		t.Errorf("invalid groups dispatched %d times", calls)
	}
}

// extractQuietRepo stops git's background maintenance, which races tree comparisons (git run drops GIT_* env).
func extractQuietRepo(t *testing.T, repo storeRepo) {
	t.Helper()
	for _, kv := range [][2]string{{"maintenance.auto", "false"}, {"gc.auto", "0"}} {
		if _, err := repo.run("config", kv[0], kv[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func extractApplyFixture(t *testing.T) (storeRepo, ExtractGroup, string, map[string][]byte) {
	t.Helper()
	repo := storeRepo{Dir: gitRepo(t, t.TempDir(), "memory")}
	extractQuietRepo(t, repo)
	group := ExtractGroup{Action: ExtractRewrite}
	original := make(map[string][]byte)
	for i := range 5 {
		rel := fmt.Sprintf("handoffs/%03d.md", i)
		body := fmt.Sprintf("handoff %d: decision and open task\n", i)
		extractFixtureFile(t, repo.Dir, rel, body, time.Now())
		original[rel] = []byte(body)
		group.Paths = append(group.Paths, rel)
	}
	idx := "# Index\n\n[first](../handoffs/000.md)\n\nhandoffs/001.md\n"
	extractFixtureFile(t, repo.Dir, "notes/idx.md", idx, time.Now())
	original["notes/idx.md"] = []byte(idx)
	rels := append(slices.Clone(group.Paths), "notes/idx.md")
	if _, committed, err := repo.Commit(rels, "seed", ""); err != nil || !committed {
		t.Fatalf("seed commit: committed=%v err=%v", committed, err)
	}
	return repo, group, idx, original
}

func TestExtractApplyCommitAndRevert(t *testing.T) {
	repo, group, idx, original := extractApplyFixture(t)
	before, err := repo.run("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Five Handoffs", Text: "Preserved decisions and tasks", Sources: group.Paths}}, Purge: []string{}}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	sha, err := ApplyExtractRewrite(repo, group, ans, now)
	if err != nil || sha == "" {
		t.Fatalf("apply sha=%q err=%v", sha, err)
	}
	assertExtractApplied(t, repo, group, idx, before)
	if _, err := repo.run("revert", "--no-edit", sha); err != nil {
		t.Fatal(err)
	}
	assertExtractOriginal(t, repo, original)
}

func assertExtractOriginal(t *testing.T, repo storeRepo, original map[string][]byte) {
	t.Helper()
	for rel, want := range original {
		got, err := os.ReadFile(filepath.Join(repo.Dir, rel))
		if err != nil || string(got) != string(want) {
			t.Errorf("reverted %s = %q, err=%v", rel, got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "notes/extract-2026-11-26-five-handoffs.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unexpected note: %v", err)
	}
}

func assertExtractApplied(t *testing.T, repo storeRepo, group ExtractGroup, idx, before string) {
	t.Helper()
	note := "notes/extract-2026-11-26-five-handoffs.md"
	data, err := os.ReadFile(filepath.Join(repo.Dir, note))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range group.Paths {
		if !strings.Contains(string(data), source) {
			t.Errorf("note omitted %s", source)
		}
	}
	assertExtractSourcesRemoved(t, repo, group.Paths)
	assertExtractLink(t, repo, idx)
	after, err := repo.run("rev-list", "--count", "HEAD")
	prior, priorErr := strconv.Atoi(before)
	current, currentErr := strconv.Atoi(after)
	if err != nil || priorErr != nil || currentErr != nil || current != prior+1 {
		t.Fatalf("commit counts %q -> %q: %v", before, after, err)
	}
}

func assertExtractLink(t *testing.T, repo storeRepo, idx string) {
	t.Helper()
	linked, err := os.ReadFile(filepath.Join(repo.Dir, "notes/idx.md"))
	if err != nil || !strings.Contains(string(linked), "extract-2026-11-26-five-handoffs.md") || strings.Contains(string(linked), "handoffs/") || string(linked) == idx {
		t.Fatalf("idx=%q err=%v", linked, err)
	}
}

func assertExtractSourcesRemoved(t *testing.T, repo storeRepo, sources []string) {
	t.Helper()
	for _, source := range sources {
		if _, err := os.Stat(filepath.Join(repo.Dir, source)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("source %s remains: %v", source, err)
		}
	}
}

func TestExtractApplyMissingSourceLeavesTree(t *testing.T) {
	repo, group, _, _ := extractApplyFixture(t)
	missing := filepath.Join(repo.Dir, group.Paths[4])
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	before := extractFixtureSnapshot(t, repo.Dir)
	countBefore, err := repo.run("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Five Handoffs", Text: "Preserved", Sources: group.Paths}}, Purge: []string{}}
	if sha, err := ApplyExtractRewrite(repo, group, ans, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)); err == nil || sha != "" {
		t.Fatalf("missing source sha=%q err=%v", sha, err)
	}
	if !reflect.DeepEqual(before, extractFixtureSnapshot(t, repo.Dir)) {
		t.Fatal("failed apply changed the tree")
	}
	if count, err := repo.run("rev-list", "--count", "HEAD"); err != nil || count != countBefore {
		t.Fatalf("failed apply commit count=%q err=%v", count, err)
	}
}

func TestExtractApplyCommitFailureRollsBack(t *testing.T) {
	repo, group, _, original := extractApplyFixture(t)
	ref, err := repo.run("symbolic-ref", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(repo.Dir, ".git", ref+".lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lock)
	countBefore, err := repo.run("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{
		{Title: "Five Handoffs", Text: "Preserved", Sources: group.Paths},
		{Title: "Shared Handoffs", Text: "Also preserved", Sources: group.Paths[:2]},
	}, Purge: []string{}}
	if sha, err := ApplyExtractRewrite(repo, group, ans, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)); err == nil || sha != "" {
		t.Fatalf("failed commit sha=%q err=%v", sha, err)
	}
	if status, err := repo.Status(); err != nil || len(status) != 0 {
		t.Fatalf("failed shared-source apply left dirty tree: %v, %v", status, err)
	}
	assertExtractOriginal(t, repo, original)
	if count, err := repo.run("rev-list", "--count", "HEAD"); err != nil || count != countBefore {
		t.Fatalf("failed commit count=%q err=%v", count, err)
	}
	if staged, err := repo.run("diff", "--cached", "--name-only"); err != nil || staged != "" {
		t.Fatalf("failed commit left index staged: %q, %v", staged, err)
	}
}

func TestExtractApplyCollisionsAndPurge(t *testing.T) {
	repo, group, _, _ := extractApplyFixture(t)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	base := "notes/extract-2026-11-26-handoffs.md"
	extractFixtureFile(t, repo.Dir, base, "existing note\n", now)
	if _, committed, err := repo.Commit([]string{base}, "seed collision", ""); err != nil || !committed {
		t.Fatalf("seed collision: committed=%v err=%v", committed, err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{
		{Title: "Handoffs", Text: "First", Sources: group.Paths[:2]},
		{Title: "Handoffs", Text: "Second", Sources: group.Paths[2:4]},
	}, Purge: group.Paths[4:]}
	if _, err := ApplyExtractRewrite(repo, group, ans, now); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{base: "existing note\n", "notes/extract-2026-11-26-handoffs-2.md": "First", "notes/extract-2026-11-26-handoffs-3.md": "Second"} {
		assertExtractNoteContains(t, repo, rel, want)
	}
	assertExtractSourcesRemoved(t, repo, group.Paths)
	message, err := repo.run("log", "-1", "--format=%B")
	if err != nil || !strings.Contains(message, group.Paths[4]+" -> (purged)") {
		t.Fatalf("purge mapping missing: %q, err=%v", message, err)
	}
}

func assertExtractNoteContains(t *testing.T, repo storeRepo, rel, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(repo.Dir, rel))
	if err != nil || !strings.Contains(string(got), want) {
		t.Errorf("note %s = %q, err=%v", rel, got, err)
	}
}

func TestExtractApplyStagedIndexUntouched(t *testing.T) {
	repo, group, _, original := extractApplyFixture(t)
	extractFixtureFile(t, repo.Dir, "notes/staged.md", "staged\n", time.Now())
	if _, err := repo.run("add", "--", "notes/staged.md"); err != nil {
		t.Fatal(err)
	}
	stagedBefore, err := repo.run("diff", "--cached", "--name-only")
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Five Handoffs", Text: "Preserved", Sources: group.Paths}}, Purge: []string{}}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if sha, err := ApplyExtractRewrite(repo, group, ans, now); err == nil || sha != "" {
		t.Fatalf("staged index sha=%q err=%v", sha, err)
	}
	assertExtractOriginal(t, repo, original)
	if staged, err := repo.run("diff", "--cached", "--name-only"); err != nil || staged != stagedBefore {
		t.Fatalf("staged index changed from %q to %q: %v", stagedBefore, staged, err)
	}
}

func TestExtractApplySameDirectoryPaths(t *testing.T) {
	repo, group, _, _ := extractApplyFixture(t)
	idx := "handoffs/idx.md"
	extractFixtureFile(t, repo.Dir, idx, "[relative](000.md)\nhandoffs/001.md\n", time.Now())
	if _, committed, err := repo.Commit([]string{idx}, "seed handoff index", ""); err != nil || !committed {
		t.Fatalf("seed index: committed=%v err=%v", committed, err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Five Handoffs", Text: "Preserved", Sources: group.Paths}}, Purge: []string{}}
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	if _, err := ApplyExtractRewrite(repo, group, ans, now); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo.Dir, idx))
	if err != nil {
		t.Fatal(err)
	}
	want := "[relative](../notes/extract-2026-11-26-five-handoffs.md)\nnotes/extract-2026-11-26-five-handoffs.md\n"
	if string(got) != want {
		t.Errorf("same-directory links = %q, want %q", got, want)
	}
}

func TestExtractLossCarryAndOwnerWords(t *testing.T) {
	const source = "reports/run.md"
	const sha = "a1b2c3d4e5f6"
	const quote = `owner: "keep the exact words"`
	sources := map[string][]byte{source: []byte("commit " + sha + "\n" + quote + "\n")}
	answer := func(text string) ExtractRewriteAnswer {
		return ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Run", Text: text, Sources: []string{source}}}, Purge: []string{}}
	}
	for _, tc := range []struct {
		name, text, want string
		live             func(string) bool
	}{
		{"sha dropped", quote, sha, nil},
		{"quote paraphrased", sha + ` owner: "similar words"`, "keep the exact words", nil},
		{"both retained", sha + "\n" + quote, "", nil},
		{"dead sha", quote, "", func(string) bool { return false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckExtractLoss(sources, answer(tc.text), tc.live)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("loss check error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExtractLossProtectedAndSearchTerms(t *testing.T) {
	const source = "reports/run.md"
	items := []string{"z1234abcd", "2026-11-25", "/Users/edotau/notes.md", "~/work/todo.md", "/tmp/run.md", "  > owner words", "> owner words", "DECISION: keep old path", "- [ ] fix it", "TODO check", "UNRESOLVED: reason", "NEXT: inspect", "`ApplyExtractRewrite`"}
	for _, item := range items {
		t.Run(item, func(t *testing.T) {
			sources := map[string][]byte{source: []byte(item + "\n")}
			ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Run", Text: "summary", Sources: []string{source}}}, Purge: []string{}}
			if err := CheckExtractLoss(sources, ans, nil); err == nil || !strings.Contains(err.Error(), source) || !strings.Contains(err.Error(), strings.Trim(item, "`")) {
				t.Fatalf("dropped item error = %v", err)
			}
			ans.Entries[0].Text = item
			if err := CheckExtractLoss(sources, ans, nil); err != nil {
				t.Fatalf("retained item: %v", err)
			}
			ans.Entries = []ExtractEntry{{Title: "Other", Text: item, Sources: []string{"other.md"}}}
			ans.Purge = []string{source}
			if err := CheckExtractLoss(sources, ans, nil); err == nil || !strings.Contains(err.Error(), source) {
				t.Fatalf("purged item error = %v", err)
			}
		})
	}
}

func TestExtractLossLiveSHAAndSingleQuote(t *testing.T) {
	repo, _, _, _ := extractApplyFixture(t)
	live, err := extractLiveSHA(repo)
	extractRunOK(t, err)
	if _, err := extractLiveSHA(storeRepo{Dir: t.TempDir()}); err == nil {
		t.Fatal("an unreadable object inventory must not pass the loss check")
	}
	if !live("a1b2c3d4e5f6") || live("deadbee") {
		t.Fatalf("live digit=%v unresolved letters=%v, want true false", live("a1b2c3d4e5f6"), live("deadbee"))
	}
	const source = "reports/run.md"
	sources := map[string][]byte{source: []byte("owner said 'keep the exact words'\n")}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Run", Text: "similar words", Sources: []string{source}}}, Purge: []string{}}
	if err := CheckExtractLoss(sources, ans, nil); err == nil || !strings.Contains(err.Error(), "keep the exact words") {
		t.Fatalf("paraphrased single quote error = %v", err)
	}
	ans.Entries[0].Text = "'keep the exact words'"
	if err := CheckExtractLoss(sources, ans, nil); err != nil {
		t.Fatalf("retained single quote: %v", err)
	}
}

func TestExtractApplyRefusesUncommittedSourceEdit(t *testing.T) {
	repo, group, _, _ := extractApplyFixture(t)
	extractFixtureFile(t, repo.Dir, group.Paths[0], "hand edit, never committed\n", time.Now())
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Handoffs", Text: "x", Sources: group.Paths}}, Purge: []string{}}
	if got, err := ApplyExtractRewrite(repo, group, ans, time.Now()); got != "" || err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("apply sha=%q err=%v", got, err)
	}
	if data, err := os.ReadFile(filepath.Join(repo.Dir, group.Paths[0])); err != nil || !strings.Contains(string(data), "hand edit") {
		t.Fatalf("hand edit lost: %q %v", data, err)
	}
}

func TestExtractLossApplyRefusalLeavesTree(t *testing.T) {
	repo, group, _, _ := extractApplyFixture(t)
	const sha = "a1b2c3d4e5f6"
	rel := group.Paths[0]
	extractFixtureFile(t, repo.Dir, rel, "live commit "+sha+"\n", time.Now())
	if _, committed, err := repo.Commit([]string{rel}, "sha source", ""); err != nil || !committed {
		t.Fatalf("commit source: committed=%v err=%v", committed, err)
	}
	before := extractFixtureSnapshot(t, repo.Dir)
	count, err := repo.run("rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Handoffs", Text: "Preserved", Sources: group.Paths}}, Purge: []string{}}
	if got, err := ApplyExtractRewrite(repo, group, ans, time.Now()); got != "" || err == nil || !strings.Contains(err.Error(), sha) {
		t.Fatalf("apply sha=%q err=%v", got, err)
	}
	if !reflect.DeepEqual(before, extractFixtureSnapshot(t, repo.Dir)) {
		t.Fatal("refused apply changed tree")
	}
	if after, err := repo.run("rev-list", "--count", "HEAD"); err != nil || after != count {
		t.Fatalf("commit count %q -> %q: %v", count, after, err)
	}
}

func TestExtractLossMarkdownAndTimestamps(t *testing.T) {
	const source = "reports/run.md"
	for _, tc := range []struct{ name, body, item string }{
		{"fence", "```go\ncode\n```\ntext `ApplyExtractRewrite`\n", "ApplyExtractRewrite"},
		{"timestamp", "at 2026-11-25T12:00:00Z\n", "2026-11-25"},
		{"file URL", "file:///Users/edotau/notes.md\n", "/Users/edotau/notes.md"},
		{"bold path", "**/Users/edotau/notes.md**\n", "/Users/edotau/notes.md"},
		{"italic path", "_/Users/edotau/notes.md_\n", "/Users/edotau/notes.md"},
		{"strike path", "~~/Users/edotau/notes.md~~\n", "/Users/edotau/notes.md"},
		{"strike home path", "~~~/work/todo.md~~\n", "~/work/todo.md"},
		{"midline strike", "~~see /Users/edotau/notes.md~~\n", "/Users/edotau/notes.md"},
		{"midline italic", "_see /Users/edotau/notes.md_\n", "/Users/edotau/notes.md"},
		{"wrapper trim order", "_/Users/edotau/notes.md._\n", "/Users/edotau/notes.md"},
		{"italic id", "_aabbccdd_\n", "aabbccdd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Run", Text: "summary", Sources: []string{source}}}, Purge: []string{}}
			err := CheckExtractLoss(map[string][]byte{source: []byte(tc.body)}, ans, nil)
			if err == nil || !strings.Contains(err.Error(), tc.item) {
				t.Fatalf("dropped item error = %v, want %q", err, tc.item)
			}
			ans.Entries[0].Text = tc.item
			if err := CheckExtractLoss(map[string][]byte{source: []byte(tc.body)}, ans, nil); err != nil {
				t.Fatalf("retained item: %v", err)
			}
		})
	}
}

func TestExtractSharedSourceValidatesAndAppliesOnce(t *testing.T) {
	allowed := map[string]bool{"handoffs/a.md": true, "handoffs/b.md": true}
	shared := `{"entries":[{"title":"a","text":"x","sources":["handoffs/a.md","handoffs/b.md"]},{"title":"b","text":"y","sources":["handoffs/a.md"]}],"purge":[]}`
	if _, err := decodeExtractRewrite(shared, allowed); err != nil {
		t.Fatalf("shared source refused: %v", err)
	}
	uncovered := `{"entries":[{"title":"a","text":"x","sources":["handoffs/a.md"]},{"title":"b","text":"y","sources":["handoffs/a.md"]}],"purge":[]}`
	if _, err := decodeExtractRewrite(uncovered, allowed); err == nil || !strings.Contains(err.Error(), "unaccounted") {
		t.Fatalf("uncovered source err=%v", err)
	}
	repo, group, _, original := extractApplyFixture(t)
	before, err := repo.run("rev-parse", "HEAD^{tree}")
	extractRunOK(t, err)
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{
		{Title: "One", Text: "decision and open task", Sources: group.Paths},
		{Title: "Two", Text: "decision and open task", Sources: group.Paths[:2]},
	}, Purge: []string{}}
	sha, err := ApplyExtractRewrite(repo, group, ans, time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC))
	if err != nil || sha == "" {
		t.Fatalf("apply sha=%q err=%v", sha, err)
	}
	assertExtractSourcesRemoved(t, repo, group.Paths)
	for _, name := range []string{"one", "two"} {
		assertExtractNoteContains(t, repo, "notes/extract-2026-11-26-"+name+".md", "decision and open task")
	}
	// Shared source links and commit mappings use the last citing entry.
	linked, err := os.ReadFile(filepath.Join(repo.Dir, "notes/idx.md"))
	if err != nil || strings.Contains(string(linked), "handoffs/") || strings.Contains(string(linked), "extract-2026-11-26-one.md") || !strings.Contains(string(linked), "extract-2026-11-26-two.md") {
		t.Fatalf("shared source links=%q err=%v", linked, err)
	}
	message, err := repo.run("log", "-1", "--format=%B")
	extractRunOK(t, err)
	for _, source := range group.Paths[:2] {
		if !strings.Contains(message, source+" -> notes/extract-2026-11-26-two.md") {
			t.Errorf("shared source mapping missing from %q", message)
		}
	}
	if _, err := repo.run("revert", "--no-edit", sha); err != nil {
		t.Fatal(err)
	}
	assertExtractOriginal(t, repo, original)
	if after, err := repo.run("rev-parse", "HEAD^{tree}"); err != nil || after != before {
		t.Fatalf("revert tree=%q want %q err=%v", after, before, err)
	}
	if status, err := repo.Status(); err != nil || len(status) != 0 {
		t.Fatalf("revert left dirty tree: %v, %v", status, err)
	}
}

func TestExtractLossSeesUnionOfEntries(t *testing.T) {
	const source = "reports/run.md"
	sources := map[string][]byte{source: []byte("see 2026-11-26 and `term-x`\n")}
	split := ExtractRewriteAnswer{Entries: []ExtractEntry{
		{Title: "a", Text: "date 2026-11-26", Sources: []string{source}},
		{Title: "b", Text: "search `term-x`", Sources: []string{source}},
	}, Purge: []string{}}
	// Validate too, so reverting shared-source acceptance fails this test.
	encoded, err := json.Marshal(split)
	if err != nil {
		t.Fatal(err)
	}
	split, err = decodeExtractRewrite(string(encoded), map[string]bool{source: true})
	if err != nil {
		t.Fatalf("split facts refused: %v", err)
	}
	if err := CheckExtractLoss(sources, split, func(string) bool { return false }); err != nil {
		t.Fatalf("union should keep both facts: %v", err)
	}
	split.Entries = split.Entries[:1]
	if err := CheckExtractLoss(sources, split, func(string) bool { return false }); err == nil {
		t.Fatal("dropped fact not refused")
	}
}

func TestExtractSharedSourceConflicts(t *testing.T) {
	allowed := map[string]bool{"handoffs/a.md": true, "handoffs/b.md": true}
	for _, tc := range []struct{ name, answer string }{
		{"repeated-citation", `{"entries":[{"title":"a","text":"x","sources":["handoffs/a.md","handoffs/a.md"]},{"title":"b","text":"y","sources":["handoffs/b.md"]}],"purge":[]}`},
		{"shared-and-purged", `{"entries":[{"title":"a","text":"x","sources":["handoffs/a.md"]},{"title":"b","text":"y","sources":["handoffs/a.md"]}],"purge":["handoffs/a.md","handoffs/b.md"]}`},
		{"repeated-purge", `{"entries":[{"title":"a","text":"x","sources":["handoffs/a.md"]},{"title":"b","text":"y","sources":["handoffs/a.md"]}],"purge":["handoffs/b.md","handoffs/b.md"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeExtractRewrite(tc.answer, allowed); err == nil || !strings.Contains(err.Error(), "repeated") {
				t.Fatalf("source conflict err=%v", err)
			}
		})
	}
}

func TestExtractSharedSourceRecallCountsUnion(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	log, ledger := filepath.Join(dir, "access.log"), filepath.Join(dir, "loss.log")
	var lines strings.Builder
	// The fixture includes repeated accesses and a shared source's ID alias.
	for _, source := range []string{"reports/a.md", "reports/a.md", "abc12345", "reports/b.md", "reports/c.md"} {
		fmt.Fprintf(&lines, "%s -- %s -- %s\n", now.Add(-time.Hour).Format(time.RFC3339), source, AccessSearch)
	}
	if err := os.WriteFile(log, []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	sources := map[string][]byte{
		"reports/a.md":        []byte("---\nid: abc12345\n---\na\n"),
		"reports/b.md":        []byte("b\n"),
		"reports/c.md":        []byte("c\n"),
		"reports/unserved.md": []byte("not recalled\n"),
	}
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{
		{Title: "a", Text: "a", Sources: []string{"reports/a.md", "reports/b.md"}},
		{Title: "b", Text: "b", Sources: []string{"reports/a.md", "reports/c.md"}},
	}, Purge: []string{"reports/unserved.md"}}
	report, err := CheckExtractRecall([]string{log}, ledger, sources, ans, now)
	if err != nil || report.Queried != 3 || report.Lost != 0 {
		t.Fatalf("shared union: report=%+v err=%v", report, err)
	}
	ans.Entries[1].Sources = []string{"reports/a.md"}
	ans.Purge = append(ans.Purge, "reports/c.md")
	report, err = CheckExtractRecall([]string{log}, ledger, sources, ans, now)
	if err == nil || !strings.Contains(err.Error(), "limit 2%") || report.Queried != 3 || report.Lost != 1 {
		t.Fatalf("purged distinct source: report=%+v err=%v", report, err)
	}
	// A source retained by only the later entry must not be skipped either.
	ans.Entries[0].Sources = []string{"reports/b.md"}
	ans.Entries[1].Sources = []string{"reports/a.md", "reports/c.md"}
	ans.Purge = []string{"reports/unserved.md"}
	report, err = CheckExtractRecall([]string{log}, ledger, sources, ans, now)
	if err != nil || report.Queried != 3 || report.Lost != 0 {
		t.Fatalf("later successor: report=%+v err=%v", report, err)
	}
}

// dreamExtractLossy answers every prompt with entries that drop their first three files, so the 6.5 loss check refuses.
func dreamExtractLossy(t *testing.T, calls *int) {
	t.Helper()
	old := CodeModeRun
	CodeModeRun = func(_ context.Context, prompt, _, _, _ string) (string, string, error) {
		*calls++
		_, data, _ := strings.Cut(prompt, "Files: ")
		var files []extractPromptFile
		if err := json.Unmarshal([]byte(data), &files); err != nil {
			t.Fatal(err)
		}
		entry, purge := ExtractEntry{Title: "Handoffs"}, []string{}
		for i, f := range files {
			if i < 3 {
				purge = append(purge, f.Path)
				continue
			}
			entry.Text += f.Text + "\n"
			entry.Sources = append(entry.Sources, f.Path)
		}
		answer, err := json.Marshal(ExtractRewriteAnswer{Entries: []ExtractEntry{entry}, Purge: purge})
		return string(answer), "fake-pi", err
	}
	t.Cleanup(func() { CodeModeRun = old })
}

func dreamExtractLossyStore(t *testing.T) (root string, calls *int) {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	root = paths.MemoryStoreDir()
	extractQuietRepo(t, storeRepo{Dir: root})
	dreamSeedHandoffs(t, root)
	var log strings.Builder
	for i := range 62 {
		fmt.Fprintf(&log, "%s -- handoffs/%03d.md -- %s\n", time.Now().UTC().Format(time.RFC3339), i, AccessSearch)
	}
	if err := os.MkdirAll(filepath.Dir(accessLogPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accessLogPath(), []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	calls = new(int)
	dreamExtractLossy(t, calls)
	return root, calls
}

func TestDreamExtractSkipsRefusedGroupUntilSourceChanges(t *testing.T) {
	root, calls := dreamExtractLossyStore(t)
	var first strings.Builder
	dreamExtract(dreamOptions{Out: &first}, nil)
	if *calls != 1 || !strings.Contains(first.String(), "extract refused:") {
		t.Fatalf("first run: calls=%d out=%q", *calls, first.String())
	}
	var second strings.Builder
	dreamExtract(dreamOptions{Out: &second}, nil)
	if !strings.Contains(second.String(), "extract skipped handoffs group (") || !strings.Contains(second.String(), "): refused earlier: extract recall:") {
		t.Fatalf("second run must skip the refused group: %q", second.String())
	}
	before := *calls
	data, err := os.ReadFile(filepath.Join(root, "handoffs/001.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "handoffs/001.md"), append(data, "more\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	var third strings.Builder
	dreamExtract(dreamOptions{Out: &third}, nil)
	if *calls <= before {
		t.Fatalf("a changed source must re-admit its group: out=%q", third.String())
	}
}

func TestDreamExtractBusyRefusalIsNotRecorded(t *testing.T) {
	_, calls := dreamExtractLossyStore(t)
	t.Setenv(DetachedEnv, "")
	old := updateLockWait
	updateLockWait = 50 * time.Millisecond
	t.Cleanup(func() { updateLockWait = old })
	var out strings.Builder
	_, err := paths.TryFileLock(lockPathOf()+".lock", time.Second, func() error {
		dreamExtract(dreamOptions{Out: &out}, nil)
		return nil
	})
	if err != nil || *calls != 1 || !strings.Contains(out.String(), ErrBankBusy.Error()) {
		t.Fatalf("busy run: err=%v calls=%d out=%q", err, *calls, out.String())
	}
	refused, err := ExtractRefusals(filepath.Join(paths.MemoryStateDir(), "extract-loss.log"))
	if err != nil || len(refused) != 0 {
		t.Fatalf("a busy refusal must not be recorded: %v %v", refused, err)
	}
}

func TestDreamExtractTemporaryFailureIsNotRecorded(t *testing.T) {
	root, calls := dreamExtractLossyStore(t)
	dreamExtractFake(t, calls)
	run := CodeModeRun
	var edited string
	CodeModeRun = func(ctx context.Context, prompt, workdir, schema, model string) (string, string, error) {
		answer, who, err := run(ctx, prompt, workdir, schema, model)
		_, data, _ := strings.Cut(prompt, "Files: ")
		var files []extractPromptFile
		extractRunOK(t, json.Unmarshal([]byte(data), &files))
		edited = filepath.Join(root, files[0].Path)
		write(t, edited, "an uncommitted owner edit\n")
		return answer, who, err
	}
	var out strings.Builder
	dreamExtract(dreamOptions{Out: &out}, nil)
	if *calls != 1 || !strings.Contains(out.String(), "sources changed while the model ran") {
		t.Fatalf("temporary failure: calls=%d out=%q", *calls, out.String())
	}
	refused, err := ExtractRefusals(filepath.Join(paths.MemoryStateDir(), "extract-loss.log"))
	if err != nil || len(refused) != 0 {
		t.Fatalf("a temporary failure must not block a future attempt: %v %v", refused, err)
	}
	if got := mustRead(t, edited); got != "an uncommitted owner edit\n" {
		t.Fatalf("owner edit changed: %q", got)
	}
}

func TestExtractApplyRefusesEditedLink(t *testing.T) {
	repo, group, _, original := extractApplyFixture(t)
	file := filepath.Join(repo.Dir, "notes/idx.md")
	write(t, file, "owner edit: handoffs/000.md\n")
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Handoffs", Text: "Preserved", Sources: group.Paths}}, Purge: []string{}}
	if _, err := ApplyExtractRewrite(repo, group, ans, time.Now()); err == nil || !strings.Contains(err.Error(), "uncommitted source edits") {
		t.Fatalf("edited link accepted: %v", err)
	}
	if got := mustRead(t, file); got != "owner edit: handoffs/000.md\n" {
		t.Fatalf("owner edit overwritten: %q", got)
	}
	for _, rel := range group.Paths {
		if got := mustRead(t, filepath.Join(repo.Dir, rel)); got != string(original[rel]) {
			t.Fatalf("source changed: %s", rel)
		}
	}
}

func TestExtractRollbackKeepsLaterEdit(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	extractRunOK(t, err)
	defer root.Close()
	file := filepath.Join(dir, "note.md")
	write(t, file, "before\n")
	extractRunOK(t, os.Chmod(file, 0o664))
	beforeMode, err := os.Stat(file)
	extractRunOK(t, err)
	written := map[string]appliedWrite{}
	extractRunOK(t, extractWriteChanges(root, map[string][]byte{"note.md": []byte("batch\n")}, map[string][]byte{"note.md": []byte("before\n")}, written))
	mode, err := os.Stat(file)
	if err != nil || mode.Mode().Perm() != beforeMode.Mode().Perm() {
		t.Fatalf("extract narrowed file mode: %v, %v", mode, err)
	}
	write(t, file, "owner edit\n")
	err = extractApplyRollback(written, errors.New("commit failed"))
	if refused(err) || mustRead(t, file) != "owner edit\n" {
		t.Fatalf("rollback overwrote an owner edit or reported a retryable failure: %v", err)
	}
}

func TestExtractRepointKeepsOtherPaths(t *testing.T) {
	body := "1000.md x/handoffs/000.md handoffs/000.md.old [rel](000.md) [full](handoffs/000.md#entry) see handoffs/000.md. ./handoffs/000.md [dot](./000.md)"
	want := "1000.md x/handoffs/000.md handoffs/000.md.old [rel](../notes/new.md) [full](notes/new.md#entry) see notes/new.md. ./notes/new.md [dot](./../notes/new.md)"
	if got := extractRepoint("handoffs/idx.md", body, map[string]string{"handoffs/000.md": "notes/new.md"}); got != want {
		t.Fatalf("repoint = %q, want %q", got, want)
	}
}

func TestDreamExtractFailedApplyRefundsLoss(t *testing.T) {
	isolatedConfig(t)
	repo, group, _, _ := extractApplyFixture(t)
	var log strings.Builder
	now := time.Now()
	for i := range 100 {
		rel := fmt.Sprintf("reports/%03d.md", i)
		if i == 0 {
			rel = group.Paths[0]
		}
		fmt.Fprintf(&log, "%s -- %s -- %s\n", now.UTC().Format(time.RFC3339), rel, AccessSearch)
	}
	write(t, accessLogPath(), log.String())
	ledger := filepath.Join(paths.MemoryStateDir(), "extract-loss.log")
	extractRunOK(t, RecordExtractLoss(ledger, now, 1))
	before := mustRead(t, ledger)
	ref, err := repo.run("symbolic-ref", "HEAD")
	extractRunOK(t, err)
	write(t, filepath.Join(repo.Dir, ".git", ref+".lock"), "")
	seen, err := dreamReadSources(repo.Dir, group.Paths)
	extractRunOK(t, err)
	ans := ExtractRewriteAnswer{Entries: []ExtractEntry{{Title: "Handoffs", Text: "Preserved", Sources: group.Paths[1:]}}, Purge: group.Paths[:1]}
	_, err = dreamApplyChecked(repo.Dir, group, ans, seen, ExtractOptions{AccessLogs: []string{accessLogPath()}})
	if err == nil || !refused(err) || mustRead(t, ledger) != before {
		t.Fatalf("failed apply kept a debit: %v, ledger=%q", err, mustRead(t, ledger))
	}
}
