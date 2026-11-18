package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// jsonShape lists each token's kind in order, so a changed type, a lost field or broken nesting differs.
func jsonShape(t *testing.T, b []byte) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var kinds []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return strings.Join(kinds, " ")
		}
		if err != nil {
			t.Fatalf("%q does not parse: %v", b, err)
		}
		kinds = append(kinds, fmt.Sprintf("%T", tok))
	}
}

// A value in an encoded event is masked without breaking the JSON: same token kinds, every line parses.
func TestValueBytesKeepsJSONValid(t *testing.T) {
	values := []string{"12345678901e2", "nABCDEF-1234xyz", `xy12345","k2`, `trail-ABCD12\`, "FAKE-SECRET-0123456789abcdef"}
	StubValues(t, values)
	events := []string{`{"n":12345678901e2,"m":-12345678901e2}`, `{"text":"x\nABCDEF-1234xyz"}`, `{"k":"xy12345","k2":"v"}`,
		`{"t":"trail-ABCD12\"end"}`, `{"a":["FAKE-SECRET-0123456789abcdef",{"b":"say \"FAKE-SECRET-0123456789abcdef\""}]}`}
	for _, ev := range events {
		b := []byte(ev)
		ValueBytes(b)
		if !json.Valid(b) || jsonShape(t, b) != jsonShape(t, []byte(ev)) {
			t.Errorf("%s masked to %s: invalid or reshaped", ev, b)
		}
		for _, v := range values {
			if bytes.Contains(b, []byte(v)) {
				t.Errorf("%s masked to %s still holds %q", ev, b, v)
			}
		}
	}
}
