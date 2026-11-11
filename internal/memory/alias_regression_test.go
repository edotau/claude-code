package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAliasRejectsIncompleteProjectLinks(t *testing.T) {
	e := newAliasEnv(t)
	link := seedAliasFold(t, e)
	broken := filepath.Join(e.cfg, "projects", "unreadable")
	if err := os.Symlink("unreadable", broken); err != nil {
		t.Fatal(err)
	}
	before := aliasTreeHash(t, StoreDir())
	if _, err := alias(t, e, false); err == nil {
		t.Error("alias accepted an incomplete project link scan")
	}
	if got := aliasTreeHash(t, StoreDir()); got != before {
		t.Error("failed project link scan changed or removed the source store")
	}
	if got, err := os.Readlink(link); err != nil || got != e.db {
		t.Fatalf("source link = %q, %v; want %q unchanged", got, err, e.db)
	}
}

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

// Folding through a secondary checkout must not orphan the source's primary checkout.
func TestPersistenceAliasFoldViaAliasRetainsPrimary(t *testing.T) {
	e := newAliasEnv(t)
	secondary := gitRepo(t, canonTemp(t, t.TempDir()), "secondary")
	if err := addOriginAlias(e.db, secondary); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.db, "topic.md"), "source knowledge\n")
	if _, err := AliasRepo(AliasOptions{Repo: secondary, Into: e.ka}); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{e.a, e.b, secondary} {
		if got := Dir(root); got != e.da {
			t.Errorf("Dir(%q) = %q, want folded store %q", root, got, e.da)
		}
	}
	if pathExists(e.db) {
		t.Error("source store survived the fold")
	}
	if got := aliasRead(t, filepath.Join(e.da, "topic.md")); got != "source knowledge\n" {
		t.Errorf("folded topic = %q", got)
	}
}

// Directories produce real read errors even when the test runs as root.
func TestPersistenceAliasDryRunRefusesReadErrors(t *testing.T) {
	for _, where := range []string{"destination", "archive"} {
		t.Run(where, func(t *testing.T) {
			e := newAliasEnv(t)
			name := "activeContext.md"
			write(t, filepath.Join(e.db, name), "source\n")
			blocked := filepath.Join(e.da, name)
			if where == "archive" {
				write(t, blocked, "target\n")
				blocked = filepath.Join(e.da, "archive", "merged-"+e.kb+"-"+name)
			}
			if err := os.MkdirAll(blocked, 0o700); err != nil {
				t.Fatal(err)
			}
			before := aliasTreeHash(t, StoreDir())
			_, err := alias(t, e, true)
			var readErr *os.PathError
			if !errors.As(err, &readErr) {
				t.Fatalf("dry-run with unreadable %s = %v, want read error", where, err)
			}
			if readErr.Path != blocked {
				t.Errorf("error path = %q, want %q", readErr.Path, blocked)
			}
			if after := aliasTreeHash(t, StoreDir()); after != before {
				t.Error("dry-run changed the store while refusing a read error")
			}
		})
	}
}

// Given a stray file in projects/, When the plan scans project links, Then it is skipped and the real link is still found.
func TestAliasPlanSkipsStrayFileInProjects(t *testing.T) {
	e := newAliasEnv(t)
	link := filepath.Join(e.cfg, "projects", "p", "memory")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(e.db, link); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.cfg, "projects", ".DS_Store"), "x")
	res, err := alias(t, e, true)
	if err != nil {
		t.Fatalf("dry-run alias with a stray projects/.DS_Store: %v", err)
	}
	if res.Source != e.kb {
		t.Errorf("source = %q, want %q", res.Source, e.kb)
	}
}
