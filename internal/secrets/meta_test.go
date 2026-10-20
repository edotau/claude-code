package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/keyring"
)

// withKeyringLookup swaps the keyringLookup seam for the test's duration, restoring it on cleanup.
func withKeyringLookup(t *testing.T, fn func(name string) (string, bool, error)) {
	t.Helper()
	saved := keyringLookup
	keyringLookup = fn
	t.Cleanup(func() { keyringLookup = saved })
}

// LoadMeta's one-time import must never overwrite a meta file that already exists, and a name held
// in neither the keyring nor the file plane must not be recorded.
func TestLoadMetaImportIdempotentAndSkipsUnknownNames(t *testing.T) {
	setConfigDir(t)
	withKeyringLookup(t, func(name string) (string, bool, error) { return "", false, nil })
	if err := setFile("FILE_ONLY", "v"); err != nil {
		t.Fatal(err)
	}

	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["FILE_ONLY"]; !ok {
		t.Fatal("FILE_ONLY must be recorded from the file plane")
	}
	if _, ok := m["NEVER_ANYWHERE"]; ok {
		t.Fatal("a name held in neither plane must not be recorded")
	}

	// Mutate the file plane after the import; a second LoadMeta must not re-import (file untouched).
	if err := setFile("NEW_AFTER_IMPORT", "v2"); err != nil {
		t.Fatal(err)
	}
	m2, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m2["NEW_AFTER_IMPORT"]; ok {
		t.Fatal("LoadMeta re-ran the import instead of reading the already-written file")
	}
}

// An import taken while the keyring does not answer must not be persisted; the next LoadMeta, once
// the seam answers normally, must run the import again and persist it.
func TestLoadMetaNotPersistedWhenKeyringDown(t *testing.T) {
	setConfigDir(t)
	withKeyringLookup(t, func(name string) (string, bool, error) { return "", false, keyring.ErrNoAnswer })
	if err := setFile("DOWN_CASE", "v"); err != nil {
		t.Fatal(err)
	}

	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["DOWN_CASE"]; !ok {
		t.Fatal("LoadMeta must still return the file-plane row for this call, even though it is not persisted")
	}
	if _, err := os.Stat(MetaFile()); !os.IsNotExist(err) {
		t.Fatalf("meta file must not exist after an incomplete import, stat err = %v", err)
	}

	if err := editMeta(func(creds map[string]Meta) bool { return false }); err == nil {
		t.Fatal("editMeta must error while the prior import is incomplete")
	}

	// Now the seam answers; LoadMeta must retry the import and persist it.
	withKeyringLookup(t, func(name string) (string, bool, error) { return "", false, nil })
	if _, err := LoadMeta(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(MetaFile()); err != nil {
		t.Fatalf("meta file must exist once the import completes: %v", err)
	}
}

// Store must never write a stored value's raw bytes into the meta file, and the file must be 0600.
func TestStoreNeverLeaksRawBytesIntoMetaFile(t *testing.T) {
	setConfigDir(t)
	secret := "aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStT12" // 40 chars, file plane
	if err := setFile("LEAK_CHECK_FILE", secret); err != nil {
		t.Fatal(err)
	}
	if err := upsertMeta("LEAK_CHECK_FILE", "file", secret); err != nil {
		t.Fatal(err)
	}

	secret2 := "zZyYxXwWvVuUtTsSrRqQpPoOnNmMlLkKjJiIhHgG98" // keyring plane, via the seam
	withKeyringLookup(t, func(name string) (string, bool, error) {
		if name == "LEAK_CHECK_KEYRING" {
			return secret2, true, nil
		}
		return "", false, nil
	})
	if err := upsertMeta("LEAK_CHECK_KEYRING", "keyring", secret2); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(MetaFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), secret2) {
		t.Fatal("meta file must never contain a stored value's raw bytes")
	}
	fi, err := os.Stat(MetaFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %04o, want 0600", perm)
	}
}

// A meta file written by a newer binary (version 2) must error on load and must be left untouched.
func TestLoadMetaRejectsNewerVersion(t *testing.T) {
	setConfigDir(t)
	future := `{"version":2,"credentials":{"X":{"plane":"file"}}}`
	if err := os.MkdirAll(filepath.Dir(MetaFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MetaFile(), []byte(future), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeta(); err == nil {
		t.Fatal("LoadMeta must error on a version newer than this binary writes")
	}
	b, err := os.ReadFile(MetaFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != future {
		t.Fatalf("LoadMeta must leave an unreadable file untouched, got %q", string(b))
	}
}

// Store must clear any prior VerifyStatus/VerifiedAt on the same name; Delete must remove its row.
func TestStoreClearsVerifyStatusAndDeleteRemovesRow(t *testing.T) {
	setConfigDir(t)
	noKeyring(t)
	if err := setFile("VERIFY_ME", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := upsertMeta("VERIFY_ME", "file", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := RecordProbe("VERIFY_ME", "verified", time.Now()); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if m["VERIFY_ME"].VerifyStatus != "verified" {
		t.Fatalf("VerifyStatus = %q, want verified", m["VERIFY_ME"].VerifyStatus)
	}

	if _, err := Store("VERIFY_ME", "v2", true); err != nil {
		t.Fatal(err)
	}
	m, err = LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got := m["VERIFY_ME"]; got.VerifyStatus != "" || !got.VerifiedAt.IsZero() {
		t.Fatalf("Store must clear VerifyStatus/VerifiedAt, got %+v", got)
	}

	if err := Delete("VERIFY_ME"); err != nil {
		t.Fatal(err)
	}
	m, err = LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m["VERIFY_ME"]; ok {
		t.Fatal("Delete must remove the row entirely")
	}
}

// A value made only of short dash-separated segments must never be persisted whole as its prefix.
func TestMetaPrefixNeverPersistsWholeValue(t *testing.T) {
	setConfigDir(t)
	for _, v := range []string{"abc-def-", "ab-cd-ef-gh-", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"} {
		if _, err := Store("SHORT_SEG", v, true); err != nil {
			t.Fatal(err)
		}
		m, err := LoadMeta()
		if err != nil {
			t.Fatal(err)
		}
		if p := m["SHORT_SEG"].Prefix; len(p) > len(v)/3 {
			t.Fatalf("prefix %q persisted for %q: over a third of the value", p, v)
		}
	}
	if p := metaPrefix("sk-ant-oat01-abcdefghijklmnopqrstuvwxyz"); p != "sk-ant-oat01-" {
		t.Fatalf("metaPrefix dropped a genuine vendor prefix: %q", p)
	}
}
