package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memstore"
)

// Given both stores rotated a same-named monthly archive with different entries, When folding, Then entries merge.
func TestAliasFoldsSameNamedSessionArchives(t *testing.T) {
	e := newAliasEnv(t)
	rel := filepath.Join("archive", "sessionHistory-2026-11.md")
	write(t, filepath.Join(e.da, rel), "# Hist\n\n## 2026-11-01 — t <!-- id:33333333 -->\nx\n")
	write(t, filepath.Join(e.db, rel), "# Hist\n\n## 2026-11-02 — s <!-- id:44444444 -->\ny\n")

	if _, err := alias(t, e, false); err != nil {
		t.Fatalf("AliasRepo: %v", err)
	}
	got := aliasRead(t, filepath.Join(e.da, rel))
	if !strings.Contains(got, "id:33333333") || !strings.Contains(got, "id:44444444") {
		t.Errorf("archive not merged:\n%s", got)
	}
}

// Given the final receipt write fails, When Update otherwise succeeds, Then the write error is returned.
func TestUpdateReturnsReceiptWriteError(t *testing.T) {
	isolatedConfig(t)
	root := repoRoot(t, "receipt-failure")
	if _, err := Init(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resultPath("receipt-failure"), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(context.Context, string, string) (string, error) { return "fixture", nil }
	err := Update(UpdateOptions{CWD: root, Session: "receipt-failure", Run: run})
	if err == nil || !strings.Contains(err.Error(), "record update result") {
		t.Fatalf("Update = %v, want receipt write error", err)
	}
}

// Given the home bank already holds the id and the local removal fails, When promoting, Then the home copy survives.
func TestPromoteHomeKeepsExistingCopyOnLocalFailure(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	id := seedConventionBlock(t, root)
	local := filepath.Join(memstore.Dir(root), "conventions.md")
	data, _ := os.ReadFile(local)
	write(t, filepath.Join(memstore.HomeBankDir(), "conventions.md"), string(data))
	if err := os.Chmod(memstore.Dir(root), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(memstore.Dir(root), 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	if _, err := Promote(root, id, "home", false, time.Now()); err == nil {
		t.Fatal("Promote succeeded despite unwritable local bank")
	}
	home, _ := os.ReadFile(filepath.Join(memstore.HomeBankDir(), "conventions.md"))
	if n := strings.Count(string(home), "<!-- id:"+id+" -->"); n != 1 {
		t.Errorf("home holds the block %d times, want the original 1", n)
	}
}

// Given a newer block quotes an older block's marker, When promoting the older id, Then only the older block moves.
func TestPromoteIgnoresQuotedMarker(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := memstore.Dir(root)
	write(t, filepath.Join(dir, "decisionLog.md"), "# Decisions\n\n"+
		"## 2026-11-02 — B <!-- id:bbbb2222 -->\n```\nsupersedes <!-- id:aaaa1111 -->\n```\n\n"+
		"## 2026-11-01 — A <!-- id:aaaa1111 -->\nold body\n")

	if _, err := Promote(root, "aaaa1111", "home", false, time.Now()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	local, _ := os.ReadFile(filepath.Join(dir, "decisionLog.md"))
	if !strings.Contains(string(local), "id:bbbb2222") {
		t.Errorf("quoting block B was removed:\n%s", local)
	}
	home, _ := os.ReadFile(filepath.Join(memstore.HomeBankDir(), "decisionLog.md"))
	if strings.Contains(string(home), "id:bbbb2222") || !strings.Contains(string(home), "old body") {
		t.Errorf("home got the wrong block:\n%s", home)
	}
}

// Given a block whose marker is wrapped below stacked newer markers, When promoting its oldest id, Then the whole block moves.
func TestPromoteFindsWrappedStackedMarker(t *testing.T) {
	tempConfig(t)
	root := repoRoot(t, "proj")
	dir := memstore.Dir(root)
	write(t, filepath.Join(dir, "conventions.md"), "# C\n\n- keep me <!-- id:bbbb2222 -->\n\n"+
		"- long rule that wraps\n  onto a second line <!-- id:cccc3333 -->\n  <!-- id:aaaa1111 -->\n")

	if _, err := Promote(root, "aaaa1111", "home", false, time.Now()); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	local, _ := os.ReadFile(filepath.Join(dir, "conventions.md"))
	if strings.Contains(string(local), "long rule") || !strings.Contains(string(local), "keep me") {
		t.Errorf("source bank not updated correctly:\n%s", local)
	}
	home, _ := os.ReadFile(filepath.Join(memstore.HomeBankDir(), "conventions.md"))
	if !strings.Contains(string(home), "onto a second line") {
		t.Errorf("home missing the wrapped block:\n%s", home)
	}
}
