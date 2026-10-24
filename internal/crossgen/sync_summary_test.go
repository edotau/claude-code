package crossgen

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var doneLine = regexp.MustCompile(`Done: (\d+) to sync, (\d+) up to date, (\d+) skipped(.*)`)

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		m[p] = fi.ModTime().String() + "|" + fi.Mode().String()
		if b, e := os.ReadFile(p); e == nil {
			m[p] += "|" + string(b)
		}
		return nil
	})
	return m
}

func TestCheckSummaryCleanFixtureReportsZeroToSync(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	writeSource(t, claude, "skills", "demo-skill", "demo-skill")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	var out bytes.Buffer
	if code := Sync(SyncOptions{Check: true, Out: &out}); code != 0 {
		t.Fatalf("check = %d\n%s", code, out.String())
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || m[1] != "0" || m[2] == "0" || m[4] != "" {
		t.Errorf("clean summary = %q\n%s", m, out.String())
	}
	if strings.Contains(out.String(), "would be") {
		t.Errorf("old wording leaked:\n%s", out.String())
	}
}

func TestCheckSummaryDriftedFixtureCountsPending(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	writeSource(t, claude, "skills", "new-skill", "new-skill")
	writeFile(t, sourcePath(claude, "agents", "demo-agent"), "---\nname: demo-agent\ndescription: changed.\n---\n\nnew\n")
	var out bytes.Buffer
	if code := Sync(SyncOptions{Check: true, Out: &out}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || m[1] == "0" {
		t.Errorf("drifted summary = %q\n%s", m, out.String())
	}
}

func TestCheckSummaryShowsPruneCount(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "keeper", "keeper")
	writeSource(t, claude, "agents", "doomed", "doomed")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	if err := os.Remove(sourcePath(claude, "agents", "doomed")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Sync(SyncOptions{Check: true, Out: &out}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	m := doneLine.FindStringSubmatch(out.String())
	if m == nil || !regexp.MustCompile(`^, \d+ to prune$`).MatchString(m[4]) {
		t.Errorf("prune suffix = %q\n%s", m, out.String())
	}
}

func TestRealSyncSummaryWordingUnchanged(t *testing.T) {
	_, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	var out bytes.Buffer
	if code := Sync(SyncOptions{Out: &out}); code != 0 {
		t.Fatalf("sync = %d", code)
	}
	if !regexp.MustCompile(`Done: \d+ synced, \d+ skipped`).MatchString(out.String()) {
		t.Errorf("real-run summary:\n%s", out.String())
	}
	if strings.Contains(out.String(), "to sync") {
		t.Errorf("dry-run wording in real run:\n%s", out.String())
	}
}

func TestCheckWritesNothing(t *testing.T) {
	home, claude := isolate(t)
	writeSource(t, claude, "agents", "demo-agent", "demo-agent")
	if code := Sync(SyncOptions{Out: io.Discard}); code != 0 {
		t.Fatal(code)
	}
	writeSource(t, claude, "skills", "new-skill", "new-skill")
	before := snapshot(t, home)
	if code := Sync(SyncOptions{Check: true, Out: io.Discard}); code != 1 {
		t.Fatalf("check = %d, want 1", code)
	}
	after := snapshot(t, home)
	if len(before) != len(after) {
		t.Errorf("file count %d -> %d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("changed: %s", p)
		}
	}
}
