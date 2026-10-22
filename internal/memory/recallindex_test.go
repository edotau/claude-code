package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type recallIndexFixture struct {
	root, source, archive string
	stamp                 exeStamp
}

func newRecallIndexFixture(t testing.TB) recallIndexFixture {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	recallIndexWrite(t, filepath.Join(MigrateStateDir(), "20260101-000000.journal.json"),
		`{"phase":"complete","manifest":"/m/done.json"}`, time.Now().Add(-time.Hour))
	if !CutOver() {
		t.Fatal("fixture did not cut over to the memory store")
	}
	stamp, ok := currentExeStamp()
	if !ok {
		t.Fatal("cannot stat the test executable")
	}
	f := recallIndexFixture{
		root:    root,
		source:  filepath.Join(Dir(root), "decisionLog.md"),
		archive: filepath.Join(Dir(root), "archive", "sessionHistory-old.md"),
		stamp:   stamp,
	}
	files := map[string]string{
		f.source: "# Decisions\n\n## router retry\nrouter retry latency\n" +
			"```\n## fenced marker\n- fenced marker\n```\n\n## the and of\nwith the and\n",
		filepath.Join(Dir(root), "conventions.md"):     "# Conventions\n\n- commit small\nreview focused changes\n",
		filepath.Join(HomeBankDir(), "conventions.md"): "# Global\n\n- router retry\nbounded latency\n",
		f.archive: "# Archive\n\n## archive storm\nrouter retry during outage\n",
		filepath.Join(Dir(root), "recall.md"): "---\nname: native recall\n" +
			"description: router context\n---\nNative recall preserves retry details.\n",
		filepath.Join(Dir(root), "bank-topic.md"): "---\nname: excludedbanktoken\n" +
			"description: excludedbanktoken\nharness_kind: bank\n---\nexcludedbanktoken\n",
		filepath.Join(KnowledgeDir(), "invariant.md"): "---\nname: strippedmetadata\n---\n" +
			"# knowledge invariant\nRouter retry requires bounded latency.\n",
	}
	for path, body := range files {
		recallIndexWrite(t, path, body, time.Now().Add(-time.Hour))
	}
	return f
}

func recallIndexWrite(t testing.TB, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func (f recallIndexFixture) search(t testing.TB, query string) []SearchHit {
	t.Helper()
	got := SearchBanksTerms(f.root, query, contentTerms(query), 0)
	if want := oracleSearch(f.root, query, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("SearchBanksTerms(%q) differs from oracle:\ngot:  %#v\nwant: %#v", query, got, want)
	}
	return got
}

func (f recallIndexFixture) index(t testing.TB) map[fileKey]indexed {
	t.Helper()
	index := readRecallIndex(filepath.Join(StateDir(), recallIndexFile), f.stamp)
	if len(index) == 0 {
		t.Fatal("no persisted index entries for the current executable stamp")
	}
	return index
}

func (f recallIndexFixture) persisted(t testing.TB) map[fileKey]indexed {
	t.Helper()
	index := f.index(t)
	for _, file := range corpusFiles(f.root) {
		info, err := os.Stat(file.path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		entry, ok := index[fileKey{file.path, file.mode}]
		if !ok || entry.size != info.Size() || entry.mtimeNs != info.ModTime().UnixNano() {
			t.Fatalf("missing or stale persisted entry for %s (mode %d)", file.path, file.mode)
		}
		if !entry.seg.valid() {
			t.Fatalf("invalid persisted segment for %s", file.path)
		}
	}
	return index
}

func TestRecallIndexWarmEqualsCold(t *testing.T) {
	f := newRecallIndexFixture(t)
	path := filepath.Join(StateDir(), recallIndexFile)
	queries := []string{"router", "router router retry", "commit small", "fenced marker",
		"native recall", "knowledge invariant", "archive storm", "the and of", "excludedbanktoken"}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			cold := f.search(t, query)
			f.persisted(t)
			warm := f.search(t, query)
			if !reflect.DeepEqual(warm, cold) {
				t.Fatalf("warm results differ from cold:\nwarm: %#v\ncold: %#v", warm, cold)
			}
		})
	}
}

func TestRecallIndexStatInvalidation(t *testing.T) {
	f := newRecallIndexFixture(t)
	before := f.search(t, "router galaxy retry")
	index := f.persisted(t)
	key := fileKey{f.source, segEntries}
	old := index[key]
	data, err := os.ReadFile(f.source)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.ReplaceAll(string(data), "router", "galaxy")
	if changed == string(data) || len(changed) != len(data) {
		t.Fatal("fixture edit must change content without changing size")
	}
	recallIndexWrite(t, f.source, changed, time.Unix(0, old.mtimeNs).Add(time.Minute))
	info, err := os.Stat(f.source)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != old.size || info.ModTime().UnixNano() == old.mtimeNs {
		t.Fatal("fixture edit did not preserve size and change mtime")
	}
	after := f.search(t, "router galaxy retry")
	if reflect.DeepEqual(before, after) {
		t.Fatal("same-size source edit did not change search results")
	}
	updated := f.persisted(t)[key]
	if reflect.DeepEqual(updated.seg, old.seg) {
		t.Fatal("same-size source edit did not replace the persisted segment")
	}
}

func TestRecallIndexRacyNotPersisted(t *testing.T) {
	f := newRecallIndexFixture(t)
	now := time.Now()
	if err := os.Chtimes(f.source, now, now); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		hits := f.search(t, "fenced marker")
		if len(hits) == 0 {
			t.Fatal("racy source was not searched")
		}
		index := f.index(t)
		if _, ok := index[fileKey{f.source, segEntries}]; ok {
			t.Fatal("racy source segment was persisted")
		}
		if _, ok := index[fileKey{f.archive, segEntries}]; !ok {
			t.Fatal("backdated archive segment was not persisted")
		}
	}
}

func TestRecallIndexStampMismatchIgnored(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	index := f.persisted(t)
	for key, entry := range index {
		entry.seg = encodeSegment([]string{"## poisoned router\nrouter router retry"})
		index[key] = entry
	}
	wrong := exeStamp{f.stamp.size + 1, f.stamp.mtimeNs + 1}
	path := filepath.Join(StateDir(), recallIndexFile)
	if err := writeRecallIndex(path, wrong, index); err != nil {
		t.Fatal(err)
	}
	if len(readRecallIndex(path, wrong)) != len(index) {
		t.Fatal("fixture did not persist the mismatched executable stamp")
	}
	if len(readRecallIndex(path, f.stamp)) != 0 {
		t.Fatal("index with mismatched executable stamp was accepted")
	}
	f.search(t, "router retry")
	f.persisted(t)
	if len(readRecallIndex(path, wrong)) != 0 {
		t.Fatal("rewritten index still carries the wrong executable stamp")
	}
}

func TestRecallIndexCorruptFailsOpen(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "router retry")
	f.persisted(t)
	path := filepath.Join(StateDir(), recallIndexFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"truncated-header", data[:12]},
		{"truncated-entry", data[:len(data)-1]},
		{"garbage", []byte(strings.Repeat("not a recall index", 8))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(path, tt.data, 0o600); err != nil {
				t.Fatal(err)
			}
			if len(readRecallIndex(path, f.stamp)) != 0 {
				t.Fatal("corrupt index was accepted")
			}
			f.search(t, "router retry")
			f.persisted(t)
		})
	}
}

func TestRecallIndexPrunesMissing(t *testing.T) {
	f := newRecallIndexFixture(t)
	f.search(t, "archive storm")
	index := f.persisted(t)
	key := fileKey{f.archive, segEntries}
	if _, ok := index[key]; !ok {
		t.Fatal("archive segment was not persisted before deletion")
	}
	if err := os.Remove(f.archive); err != nil {
		t.Fatal(err)
	}
	// A changed surviving source triggers the write that prunes deleted entries.
	recallIndexWrite(t, f.source, "# Decisions\n\n## router retry\nupdated router policy\n",
		time.Now().Add(-time.Hour))
	if hits := f.search(t, "archive storm"); len(hits) != 0 {
		t.Fatalf("deleted archive still returned hits: %#v", hits)
	}
	f.search(t, "updated router policy")
	if _, ok := f.persisted(t)[key]; ok {
		t.Fatal("deleted archive segment was not pruned")
	}
}

func TestRecallIndexConcurrentWriters(t *testing.T) {
	f := newRecallIndexFixture(t)
	const query = "router router retry native recall archive storm"
	want := oracleSearch(f.root, query, 0)
	if len(want) == 0 {
		t.Fatal("concurrent search fixture has no hits")
	}
	terms := contentTerms(query)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			for range 4 {
				got := SearchBanksTerms(f.root, query, terms, 0)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("concurrent search differs from oracle:\ngot:  %#v\nwant: %#v", got, want)
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()
	f.search(t, query)
	f.persisted(t)
	entries, err := os.ReadDir(StateDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() != recallIndexFile {
			t.Errorf("unexpected file after concurrent writes: %s", entry.Name())
		}
	}
}

func BenchmarkSearchBanksTermsWarm(b *testing.B) {
	f := newRecallIndexFixture(b)
	const query = "router router retry native recall archive storm"
	f.search(b, query)
	f.persisted(b)
	terms := contentTerms(query)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		SearchBanksTerms(f.root, query, terms, 0)
	}
}
