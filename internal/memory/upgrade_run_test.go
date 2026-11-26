package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

var upgradeStore = map[string]string{
	layoutFile:                    "flat 3\n",
	"progress.md":                 "# Progress\n\n## What Works\n- a\n\n## Known Issues\n- x\n\n## What Works\n- b\n",
	"decisionLog.md":              "# Log\n\n## 2026-11-01 — first\nbody\n\n## undated\nbody\n",
	"archive/progress-2026-10.md": "# Progress\n\n## Done\n- old\n",
	"archive/2026-10-21-gemini-history-snapshot.md": "# Snapshot\n\n- x\n",
}

func upgradeFixture(t *testing.T) string {
	t.Helper()
	isolatedConfig(t)
	initStore(t)
	root := paths.MemoryStoreDir()
	rels := []string{".gitignore"}
	for rel, body := range upgradeStore {
		write(t, filepath.Join(root, rel), body)
		rels = append(rels, rel)
	}
	if _, _, err := StoreRepo().Commit(rels, "seed", ""); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestUpgradeDryRunWritesNothing(t *testing.T) {
	root := upgradeFixture(t)
	rep, err := MemoryUpgrade(UpgradeOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	rep.Print(&out)
	for _, want := range []string{"flat 3 -> 4", "progress.md: merged 1", "decisionLog.md: dated 1, identified 2",
		"skipped archive/2026-10-21-gemini-history-snapshot.md: no source key"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, out.String())
		}
	}
	if !rep.Partial() {
		t.Error("a skipped file must make the run partial")
	}
	if st, err := StoreRepo().Status(); err != nil || len(st) != 0 {
		t.Errorf("dry run left %v (err %v)", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, layoutFile)); string(b) != "flat 3\n" {
		t.Errorf("stamp changed to %q", b)
	}
	if _, err := os.Stat(upgradeProgressPath()); err == nil {
		t.Error("dry run wrote upgrade.json")
	}
}

func TestUpgradeRunCommitsOnce(t *testing.T) {
	root := upgradeFixture(t)
	before, _ := StoreRepo().headSHA()
	rep, err := MemoryUpgrade(UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := StoreRepo().headSHA(); after == before || rep.Steps[0].Commit != after {
		t.Errorf("head %s -> %s, report %q", before, after, rep.Steps[0].Commit)
	}
	if b, _ := os.ReadFile(filepath.Join(root, layoutFile)); string(b) != "flat 4\n" {
		t.Errorf("stamp %q", b)
	}
	if st, _ := StoreRepo().Status(); len(st) != 0 {
		t.Errorf("tree dirty after run: %v", st)
	}
	again, err := MemoryUpgrade(UpgradeOptions{})
	if err != nil || len(again.Steps) != 0 || again.From != again.To {
		t.Errorf("second run = %+v, %v; want nothing to do", again, err)
	}
}

func TestUpgradeRefusesDirtyStore(t *testing.T) {
	root := upgradeFixture(t)
	write(t, filepath.Join(root, "progress.md"), "# Progress\n\nedited\n")
	if _, err := MemoryUpgrade(UpgradeOptions{}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("dirty store: err = %v", err)
	}
}

const goodBatch = `{"ops":[{"op":"add","target":"progress","section":"Known Issues","text":"placed by the fallback"}]}`

func fakeCodeMode(t *testing.T, answer string) *int {
	t.Helper()
	calls := 0
	old := CodeModeRun
	CodeModeRun = func(_ context.Context, _, _, _, _ string) (string, string, error) {
		calls++
		return answer, "fake-pi", nil
	}
	t.Cleanup(func() { CodeModeRun = old })
	return &calls
}

func TestTypedWriteFallsBackToCodeModeOnce(t *testing.T) {
	upgradeFixture(t)
	calls := fakeCodeMode(t, goodBatch)
	var fb CodeFallback
	if ans, who, err := fb.Run(context.Background(), "p", t.TempDir(), BatchSchema()); err != nil || who != "fake-pi" || ans != goodBatch {
		t.Fatalf("first run = %q, %q, %v", ans, who, err)
	}
	if _, _, err := fb.Run(context.Background(), "p", t.TempDir(), BatchSchema()); err == nil || *calls != 1 {
		t.Fatalf("second run err = %v after %d dispatches, want a refusal after 1", err, *calls)
	}
	bad := fakeCodeMode(t, `{"ops":[{"op":"nope"}]}`)
	if _, _, err := new(CodeFallback).Run(context.Background(), "p", t.TempDir(), BatchSchema()); err == nil || *bad != 1 {
		t.Fatalf("an invalid answer must be refused (err %v)", err)
	}
}

func TestUpgradeResidueFallsBackOnce(t *testing.T) {
	t.Setenv("HARNESS_KEYRING", "off")
	upgradeFixture(t)
	calls := fakeCodeMode(t, goodBatch)
	rep, err := MemoryUpgrade(UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(paths.MemoryStoreDir(), "progress.md")); !strings.Contains(string(b), "placed by the fallback") {
		t.Errorf("the residue batch was not written:\n%s", b)
	}
	if st, _ := StoreRepo().Status(); len(st) != 0 {
		t.Errorf("a successful residue batch left the store dirty: %v", st)
	}
	if *calls != 1 || !strings.HasPrefix(rep.Resolved, "batch applied by fake-pi") {
		t.Errorf("dispatches %d, receipt %q: want one dispatch naming the model", *calls, rep.Resolved)
	}
}

func TestUpgradeResidueRefusedLeavesStoreUntouched(t *testing.T) {
	t.Setenv("HARNESS_KEYRING", "off")
	for name, answer := range map[string]string{
		"invalid":     `{"ops":[{"op":"bogus"}]}`,
		"apply fails": `{"ops":[{"op":"close","id":"z00000000","reason":"done"}]}`,
	} {
		upgradeFixture(t)
		calls := fakeCodeMode(t, answer)
		rep, err := MemoryUpgrade(UpgradeOptions{Model: "astra"})
		if err != nil {
			t.Fatal(err)
		}
		head, _ := StoreRepo().headSHA()
		if st, _ := StoreRepo().Status(); *calls != 1 || len(st) != 0 || !rep.Partial() ||
			!strings.Contains(rep.Resolved, "failed") || !strings.Contains(rep.Resolved, "fake-pi") {
			t.Errorf("%s: calls %d, dirty %v, skipped %v, receipt %q", name, *calls, st, rep.Skipped, rep.Resolved)
		}
		if head == "" {
			t.Errorf("%s: no head", name)
		}
	}
}

func TestUpgradeModelAndNoResolve(t *testing.T) {
	t.Setenv("HARNESS_KEYRING", "off")
	for in, want := range map[string]string{"": "opus", "astra": "astra", "gpt-6-astra": "gpt-6-astra"} {
		if got, err := FallbackModel(in); err != nil || got != want {
			t.Errorf("FallbackModel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := FallbackModel("no-such-model"); err == nil {
		t.Error("an id outside the catalog must be refused")
	}
}

func TestUpgradeDryRunAndNoResolveDoNotDispatch(t *testing.T) {
	t.Setenv("HARNESS_KEYRING", "off")
	upgradeFixture(t)
	calls := fakeCodeMode(t, goodBatch)
	if rep, err := MemoryUpgrade(UpgradeOptions{DryRun: true}); err != nil || *calls != 0 || !strings.Contains(rep.Resolved, "model opus") {
		t.Errorf("dry run dispatched %d, receipt %q, err %v", *calls, rep.Resolved, err)
	}
	if rep, err := MemoryUpgrade(UpgradeOptions{NoResolve: true}); err != nil || *calls != 0 || rep.Resolved != "" {
		t.Errorf("--no-resolve dispatched %d, receipt %q, err %v", *calls, rep.Resolved, err)
	}
}

// standInStoreCopy swaps the git clone for a temp dir, for tests whose store is not a repo.
func standInStoreCopy(t *testing.T) {
	t.Helper()
	old := StoreCopy
	StoreCopy = func(context.Context) (string, func(), error) { return t.TempDir(), func() {}, nil }
	t.Cleanup(func() { StoreCopy = old })
}

func TestRefusedTellsCommitErrorsFromRefusals(t *testing.T) {
	after := &AfterCommitError{errors.New("git commit failed")}
	for err, want := range map[error]bool{
		errors.New("1 of 1 ops failed"):      true,
		after:                                false,
		fmt.Errorf("dream batch: %w", after): false,
		withFailedOps(after, []OpResult{{Outcome: DreamFailed}}): false,
		&ConflictError{Op: 0, Ref: "r"}:                          true,
	} {
		if got := refused(err); got != want {
			t.Errorf("refused(%v) = %v, want %v", err, got, want)
		}
	}
}

func TestUpgradeResidueCoversDuplicateIDsAndCatalogModels(t *testing.T) {
	calls := fakeCodeMode(t, goodBatch)
	got := resolveResidue(UpgradeOptions{DryRun: true}, UpgradeReport{DupIDs: []string{"abc: a.md, b.md"}})
	if *calls != 0 || !strings.Contains(got, "duplicate id abc: a.md, b.md") {
		t.Errorf("dup-only residue receipt %q after %d dispatches", got, *calls)
	}
	if _, err := FallbackModel("claude-opus-9-9"); err == nil {
		t.Error("a well-formed Claude id outside the catalog must be refused")
	}
}
