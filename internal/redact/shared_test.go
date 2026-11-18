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

// A fresh down-latch skips the vault read and an old one does not; a good read removes the latch.
func TestSharedWithinLatchSkipsRead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	reads, fail := 0, true
	stub := func() { // a new stub drops the process cache, as a new hook process starts without one
		StubSource(t, func() ([]string, error) {
			reads++
			if fail {
				return nil, vault.ErrKeyringNoAnswer
			}
			return nil, nil
		})
	}
	stub()
	now := time.Unix(1_700_000_000, 0)
	prev := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = prev })

	if _, cause, announce := SharedWithin(time.Second); reads != 1 || cause == "" || !announce {
		t.Fatalf("first: reads %d cause %q announce %v", reads, cause, announce)
	}
	now = now.Add(latchTTL - time.Second)
	stub()
	if _, cause, announce := SharedWithin(time.Second); reads != 1 || cause != downPhrases["noanswer"] || announce {
		t.Fatalf("fresh latch: reads %d cause %q announce %v, want the read skipped", reads, cause, announce)
	}
	now = now.Add(2 * time.Second)
	stub()
	if _, _, announce := SharedWithin(time.Second); reads != 2 || !announce {
		t.Fatalf("old latch: reads %d announce %v, want a new read and a renewed latch", reads, announce)
	}
	now, fail = now.Add(latchTTL+time.Second), false
	stub()
	if _, cause, _ := SharedWithin(time.Second); cause != "" {
		t.Fatalf("good read: cause %q", cause)
	}
	if _, _, found := readLatch(); found {
		t.Error("a successful read left the latch")
	}
}
