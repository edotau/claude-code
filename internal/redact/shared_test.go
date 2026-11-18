package redact

import (
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/edotau/claude-code/internal/nonet"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCredentialNamesSkipsLabeledCaches(t *testing.T) {
	vaulttest.Use(t)
	const phrase, token = "plain-words-phrase", "FAKE-tok-0123456789abcdef"
	if err := vault.Store("SOME_API_KEY", token); err != nil {
		t.Fatal("store")
	}
	err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutSealed(key, "model-discovery/v1/x", `{"route":"`+phrase+`","url":"https://h.example/serving-endpoints"}`, "cache", time.Now())
	})
	if err != nil {
		t.Fatal("seal")
	}
	names, err := credentialNames()
	if err != nil || !slices.Equal(names, []string{"SOME_API_KEY"}) {
		t.Fatalf("names %v %v", len(names), err)
	}
	secrets, err := vault.ReadSecrets(names)
	if err != nil {
		t.Fatal("read")
	}
	const addr = "https://h.example/serving-endpoints"
	m := FromValues([]string{secrets["SOME_API_KEY"], `{"url":"` + addr + `","tok":"` + token + `x"}`})
	got := m.String(phrase + " " + addr + " " + token + "x")
	if !strings.Contains(got, phrase+" "+addr+" "+marker) || strings.Contains(got, token) {
		t.Fatal("phrase or address masked, or token left")
	}
}
