package memory

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPlansSyncDecideAndArchiveMove(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	put := func(dir, rel, body string) {
		p := filepath.Join(dir, rel)
		syncMust(t, os.MkdirAll(filepath.Dir(p), 0o700))
		syncMust(t, os.WriteFile(p, []byte(body), 0o600))
	}
	h := func(s string) string { return digestOf([]byte(s)) }
	peerCfg := t.TempDir()
	peer := filepath.Join(peerCfg, "plans")
	put(plansRoot(), "a.md", "A") // unchanged here, edited there: taken
	put(peer, "a.md", "A2")
	put(plansRoot(), "b.md", "B1") // edited here: sent
	put(peer, "b.md", "B0")
	put(plansRoot(), "c.md", "C") // archived into the store: removed on both sides
	put(peer, "c.md", "C")
	put(filepath.Join(cfg, "memory", "archive", "plans"), "claude/c.md", "C")
	put(peer, "x/d.md", "D") // only the peer has it: copied, slug subdirectory included
	seen := map[string]string{"a.md": h("A"), "b.md": h("B0"), "c.md": h("C")}
	local, theirs := hashTree(plansRoot()), hashTree(peer)
	got := decidePlans(seen, local, theirs, archivedPlans(union(local, theirs)))
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
