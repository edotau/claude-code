package memory

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/textutil"
)

func TestPlansSyncDecideAndArchiveMove(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	put := func(dir, rel, body string) {
		p := filepath.Join(dir, rel)
		syncMust(t, os.MkdirAll(filepath.Dir(p), 0o700))
		syncMust(t, os.WriteFile(p, []byte(body), 0o600))
	}
	h := func(s string) string { return textutil.SHA256Hex([]byte(s)) }
	peerCfg := t.TempDir()
	peer := filepath.Join(peerCfg, "plans")
	put(plansRoot(), "a.md", "A") // unchanged here, edited there: taken
	put(peer, "a.md", "A2")
	put(plansRoot(), "b.md", "B1") // edited here: sent
	put(peer, "b.md", "B0")
	put(plansRoot(), "c.md", "C") // archived into the store: removed on both sides
	put(peer, "c.md", "C")
	put(filepath.Join(cfg, "memory", "archive", "plans"), "c.md", "C")
	put(peer, "x/d.md", "D") // only the peer has it: copied, slug subdirectory included
	seen := map[string]string{"a.md": h("A"), "b.md": h("B0"), "c.md": h("C")}
	local, theirs := hashTree(plansRoot()), hashTree(peer)
	got := decidePlans(seen, local, theirs, archivedPlans(union(local, theirs)), primaryNone)
	want := plansAction{
		take: plansWrite{rels: []string{"a.md", "x/d.md"}, over: map[string]string{"a.md": h("A")}, drop: map[string]string{"c.md": h("C")}},
		send: plansWrite{rels: []string{"b.md"}, over: map[string]string{"b.md": h("B0")}, drop: map[string]string{"c.md": h("C")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decide = %+v, want %+v", got, want)
	}
	_, err := applyPlans(plansRoot(), peer, got.take, false)
	syncMust(t, err)
	after := map[string]string{"a.md": h("A2"), "b.md": h("B1"), "x/d.md": h("D")}
	if tree := hashTree(plansRoot()); !maps.Equal(tree, after) {
		t.Fatalf("local plans after apply = %v, want %v", tree, after)
	}
	from := t.TempDir() // the peer side: what sendPlans' tar carries
	put(from, "b.md", "B1")
	put(from, ".replace", string(hashList(got.send.over)))
	put(from, ".remove", string(hashList(got.send.drop)))
	t.Setenv("CLAUDE_CONFIG_DIR", peerCfg)
	_, err = ImportPlans(from, false)
	syncMust(t, err)
	if tree := hashTree(peer); !maps.Equal(tree, after) {
		t.Fatalf("peer plans after import = %v, want %v", tree, after)
	}
}

func plansPut(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	syncMust(t, os.MkdirAll(filepath.Dir(p), 0o700))
	syncMust(t, os.WriteFile(p, []byte(body), 0o600))
}

func TestDecidePlansArchivedRules(t *testing.T) {
	h := func(s string) string { return textutil.SHA256Hex([]byte(s)) }
	tests := []struct {
		name                          string
		seen, local, theirs, archived string
		wantTake, wantSend, wantDrop  []string
		wantDiffer                    int
	}{
		{"conflict pair never drops the peer's edit", h("A1") + " " + h("B1"), "", h("B1"), h("A1"), []string{"x.md"}, nil, nil, 0},
		{"settled unedited copy drops once the peer lacks it", h("S"), h("S"), "", h("Z"), nil, nil, []string{"x.md"}, 0},
		{"archived local plan is not offered to the peer", "", h("L"), "", h("Z"), nil, nil, nil, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := func(v string) map[string]string {
				if v == "" {
					return map[string]string{}
				}
				return map[string]string{"x.md": v}
			}
			seen := map[string]string{}
			if tc.seen != "" {
				seen["x.md"] = tc.seen
			}
			a := decidePlans(seen, m(tc.local), m(tc.theirs), m(tc.archived), primaryNone)
			if !reflect.DeepEqual(a.take.rels, tc.wantTake) || !reflect.DeepEqual(a.send.rels, tc.wantSend) || a.differ != tc.wantDiffer {
				t.Fatalf("take %v send %v differ %d, want %v %v %d", a.take.rels, a.send.rels, a.differ, tc.wantTake, tc.wantSend, tc.wantDiffer)
			}
			if got := slices.Sorted(maps.Keys(a.take.drop)); !reflect.DeepEqual(got, tc.wantDrop) && len(got)+len(tc.wantDrop) > 0 {
				t.Fatalf("take.drop = %v, want %v", got, tc.wantDrop)
			}
		})
	}
}

func TestPlansSentSettlesOnPackedBytes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := func(s string) string { return textutil.SHA256Hex([]byte(s)) }
	plansPut(t, plansRoot(), "x.md", "two")
	theirs := map[string]string{"x.md": h("one")}
	seen := map[string]string{"x.md": h("one")}
	a := decidePlans(seen, hashTree(plansRoot()), theirs, nil, primaryNone)
	kept, sent := map[string]bool{}, map[string]string{}
	syncMust(t, writePlansTar(io.Discard, a.send, kept, sent))
	plansPut(t, plansRoot(), "x.md", "three") // saved during the push
	plansSent(seen, a, theirs, kept, sent)
	next := decidePlans(seen, hashTree(plansRoot()), map[string]string{"x.md": h("two")}, nil, primaryNone)
	if !slices.Equal(next.send.rels, []string{"x.md"}) {
		t.Fatalf("seen = %v: the save during the push was not sent next pass (send %v)", seen, next.send.rels)
	}
}

func TestWriteAtomicKeepsAMovedFile(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	syncMust(t, err)
	defer root.Close()
	syncMust(t, root.WriteFile("x.md", []byte("saved"), 0o600))
	line, err := writeAtomic(root, "x.md", []byte("incoming"), textutil.SHA256Hex([]byte("observed")))
	syncMust(t, err)
	if b, _ := root.ReadFile("x.md"); line != "kept local x.md" || string(b) != "saved" {
		t.Fatalf("line %q, file %q: a save since the observation was overwritten", line, b)
	}
	if _, err := root.Lstat(".sync-x.md"); err == nil {
		t.Fatal("temp file left behind")
	}
	line, err = writeAtomic(root, "x.md", []byte("incoming"), textutil.SHA256Hex([]byte("saved")))
	if b, _ := root.ReadFile("x.md"); err != nil || line != "write x.md" || string(b) != "incoming" {
		t.Fatalf("matching before: line %q err %v file %q", line, err, b)
	}
}

func TestPlansStampSeesArchiveCopy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansPut(t, plansRoot(), "r/x.md", "A")
	local := hashTree(plansRoot())
	before := plansStamp(local)
	plansPut(t, filepath.Join(cfg, "memory", "archive", "plans"), "r/x.md", "A")
	if plansStamp(local) == before {
		t.Fatal("stamp unchanged after an archive copy appeared for a local plan")
	}
}

func plansList(dir string) []string { return slices.Sorted(maps.Keys(hashTree(dir))) }

func plansApplyX(t *testing.T, from string) []string {
	t.Helper()
	lines, err := applyPlans(plansRoot(), from, plansWrite{rels: []string{"r/x.md"}, over: map[string]string{}, drop: map[string]string{}}, false)
	syncMust(t, err)
	return lines
}

func archivedFixture(t *testing.T) (arch, incoming string) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	arch, incoming = filepath.Join(cfg, "memory", "archive", "plans"), t.TempDir()
	plansPut(t, arch, "r/x.md", "archived")
	return arch, incoming
}

func TestApplyPlansArchivedIdentical(t *testing.T) {
	arch, in := archivedFixture(t)
	plansPut(t, in, "r/x.md", "archived")
	if lines := plansApplyX(t, in); len(lines) != 0 || len(plansList(plansRoot())) != 0 || !slices.Equal(plansList(arch), []string{"r/x.md"}) {
		t.Fatalf("identical copy: lines %v live %v archive %v", lines, plansList(plansRoot()), plansList(arch))
	}
}

func TestApplyPlansArchivedDiffering(t *testing.T) {
	arch, in := archivedFixture(t)
	plansPut(t, in, "r/x.md", "edited")
	side := "r/x-" + textutil.SHA256Hex([]byte("edited"))[:12] + ".md"
	want := plansBeside + "r/x.md: kept beside as archive/plans/" + side
	if lines := plansApplyX(t, in); !slices.Equal(lines, []string{want}) || len(plansList(plansRoot())) != 0 || !slices.Equal(plansList(arch), []string{side, "r/x.md"}) {
		t.Fatalf("differing copy: lines %v live %v archive %v", lines, plansList(plansRoot()), plansList(arch))
	}
	if lines := plansApplyX(t, in); len(lines) != 0 || len(plansList(arch)) != 2 {
		t.Fatalf("second apply not a no-op: lines %v archive %v", lines, plansList(arch))
	}
}

func TestApplyPlansArchivedSideNameTaken(t *testing.T) {
	arch, in := archivedFixture(t)
	plansPut(t, in, "r/x.md", "edited")
	side := "r/x-" + textutil.SHA256Hex([]byte("edited"))[:12] + ".md"
	plansPut(t, arch, side, "someone else's")
	if lines := plansApplyX(t, in); len(lines) != 1 || !strings.HasPrefix(lines[0], "skipped r/x.md: side copy") {
		t.Fatalf("taken side name: lines %v", lines)
	}
	if b, _ := os.ReadFile(filepath.Join(arch, side)); string(b) != "someone else's" {
		t.Fatalf("side copy overwritten: %q", b)
	}
}

func TestImportPlansCountsSideCopy(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	plansPut(t, filepath.Join(cfg, "memory", "archive", "plans"), "r/x.md", "archived")
	from := t.TempDir()
	plansPut(t, from, "r/x.md", "edited")
	plansPut(t, from, ".replace", "")
	plansPut(t, from, ".remove", "")
	lines, err := ImportPlans(from, false)
	syncMust(t, err)
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, ", 1 differ") {
		t.Fatalf("summary %q lacks \", 1 differ\"", last)
	}
}
