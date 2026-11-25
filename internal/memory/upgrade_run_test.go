package memory

import (
	"bytes"
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
