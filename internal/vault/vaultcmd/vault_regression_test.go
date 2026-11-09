package vaultcmd

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func TestImportRejectsNullDocument(t *testing.T) {
	vaulttest.Use(t)
	seedTestDoc(t)
	vaulttest.Put(t, vault.Overlay, "settings", `{"theme":"dark"}`)
	vaulttest.SetRecord(t, "codex", "model", "keep-model")
	orig := exportTestDoc(t)
	before := vaultTestState(t)
	if err := Import(orig, []byte(" \nnull\t")); err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Errorf("Import(null) = %v, want JSON object error", err)
	}
	if got := exportTestDoc(t); !bytes.Equal(got, orig) {
		t.Error("null document changed exported pins, env, records, overlays or key_command")
	}
	if got := vaultTestState(t); !reflect.DeepEqual(got, before) {
		t.Error("null document changed a vault bucket")
	}
}

func TestVaultSetTailscaleRollsBackSecondWrite(t *testing.T) {
	for _, prior := range []string{"", "original-client-id"} {
		t.Run(map[bool]string{false: "new", true: "replace"}[prior != ""], func(t *testing.T) {
			vaulttest.UseFreshKey(t)
			vaulttest.Secret(t, "TAILSCALE_API_KEY", "unrelated-api-key")
			if prior != "" {
				vaulttest.Secret(t, "TAILSCALE_OAUTH_CLIENT_ID", prior)
			}
			blockSecretWrite(t, "TAILSCALE_OAUTH_CLIENT_SECRET")
			vaultTestStdin(t, "replacement-id\nreplacement-secret\n")
			code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
				return Run([]string{"set", "--tailscale"})
			})
			if code != 1 || !strings.Contains(stderr, bolterrors.ErrIncompatibleValue.Error()) {
				t.Fatalf("second credential write: exit %d, stdout=%q, stderr=%q", code, stdout, stderr)
			}
			assertVaultSecretUnchanged(t, "TAILSCALE_OAUTH_CLIENT_ID", prior)
			assertVaultSecretUnchanged(t, "TAILSCALE_API_KEY", "unrelated-api-key")
		})
	}
}

func assertVaultSecretUnchanged(t *testing.T, name, want string) {
	t.Helper()
	got, found, err := vault.ReadSecret(name)
	if err != nil || found != (want != "") || got != want {
		t.Errorf("failed OAuth write changed %s: found=%v, err=%v", name, found, err)
	}
}

// A nested bucket is readable as absent but makes bbolt reject a credential write at that exact key.
func blockSecretWrite(t *testing.T, name string) {
	t.Helper()
	db, err := bbolt.Open(vault.Path(), 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.Bucket([]byte(vault.Secrets)).CreateBucket([]byte(name))
		return err
	})
	closeErr := db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
