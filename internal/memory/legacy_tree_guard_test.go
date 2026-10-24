package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInitRefusesLegacyTreeWhenStoreExistsWithoutJournal: a live store beside a missing journal is not a fresh box.
func TestInitRefusesLegacyTreeWhenStoreExistsWithoutJournal(t *testing.T) {
	cfg, _ := migrateConfig(t)
	if err := StoreRepo().Ensure(); err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t, "repo")

	if dir, err := Init(root); err == nil && !inStore(dir) {
		t.Errorf("Init wrote %s beside a live store %s", dir, StoreDir())
	}
	if _, serr := os.Stat(filepath.Join(cfg, "docs", "memory", "bank", "repo")); serr == nil {
		t.Error("legacy docs/memory/bank/repo was recreated beside a live store")
	}
}

// TestApplyInlineOriginDestOutsideStoreWritesNothing: an origin item must not mkdir/write outside StoreDir().
func TestApplyInlineOriginDestOutsideStoreWritesNothing(t *testing.T) {
	cfg, _ := migrateConfig(t)
	outside := filepath.Join(cfg, "docs", "memory", "evil-slug")
	inline, err := json.Marshal(Origin{Version: 1, Key: "evil-slug", Origin: filepath.Join(t.TempDir(), "evil")})
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Schema: ManifestSchema, Created: time.Now(), ConfigDir: cfg, Store: StoreDir(),
		Items: []MigrateItem{{
			ID: "inline000001", Kind: KindOriginItem, Dest: outside, Inline: inline,
			Bytes: int64(len(inline)), SHA256: sha256Hex(inline), Action: ActionCopy, Reason: "origin",
		}},
	}
	manifestPath, err := WriteManifest(m)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	_, _ = Apply(manifestPath, &out) // error or conflict are both fine; the escape is not
	if _, serr := os.Stat(outside); serr == nil {
		t.Errorf("Apply created %s outside the store for an inline origin item:\n%s", outside, out.String())
	}
}

func inStore(p string) bool { _, ok := InsideStore(p); return ok }
