package vault

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestMasterKeyStatusRejectsCorruptSecrets(t *testing.T) {
	for _, malformed := range []bool{true, false} {
		t.Run(fmt.Sprintf("malformed-json=%t", malformed), func(t *testing.T) {
			phase1bRing(t)
			phase1bPut(t, "A_GOOD", "healthy-value")
			phase1bPut(t, "Z_BAD", "damaged-value")
			if count, source, err := MasterKeyStatus(); count != 2 || source != "keyring" || err != nil {
				t.Fatalf("healthy status = %d, %q, %v", count, source, err)
			}
			corruptHealthSecret(t, malformed)
			count, source, err := MasterKeyStatus()
			if count != 2 || source != "keyring" || err == nil {
				t.Fatalf("corrupt status = %d, %q, %v; want 2, keyring, error", count, source, err)
			}
		})
	}
}

func TestMasterKeyStatusEmptyAndUnreadable(t *testing.T) {
	phase1bRing(t)
	if count, source, err := MasterKeyStatus(); count != 0 || source != "not needed" || err != nil {
		t.Fatalf("empty vault: MasterKeyStatus = (%d, %q, %v), want (0, \"not needed\", nil)", count, source, err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "schema", strconv.Itoa(schemaVersion+1)) }); err != nil {
		t.Fatal(err)
	}
	if count, source, err := MasterKeyStatus(); count != 0 || source != "" || err == nil {
		t.Fatalf("unreadable vault: MasterKeyStatus = (%d, %q, %v), want (0, \"\", err)", count, source, err)
	}
}

func corruptHealthSecret(t *testing.T, malformed bool) {
	t.Helper()
	if err := Update(func(tx *Tx) error {
		if malformed {
			return tx.Put(Secrets, "Z_BAD", "{")
		}
		r, _, err := tx.Record("Z_BAD")
		if err != nil {
			return err
		}
		r.Sealed[len(r.Sealed)-1] ^= 1
		return tx.PutRecord("Z_BAD", r)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLooksLikeSecret(t *testing.T) {
	cases := map[string]bool{
		"sk-abcdefghij0123456789":    true,
		"ghp_abcdefghij0123456789":   true,
		"${ANTHROPIC_API_KEY}":       false,
		"$ANTHROPIC_API_KEY":         false,
		"/home/user/.ssh/token-file": false,
		"":                           false,
		"YOUR_API_KEY_HERE":          false,
	}
	for v, want := range cases {
		if got := LooksLikeSecret(v); got != want {
			t.Errorf("LooksLikeSecret(%q) = %v, want %v", v, got, want)
		}
	}
}

// secretPattern (raw-text scan) catches every GitHub family at 20+ body characters and not shorter.
func TestSecretPatternGitHubFamilies(t *testing.T) {
	for _, p := range []string{"gho_", "ghp_", "ghu_", "ghs_", "ghr_", "github_pat_"} {
		if !secretPattern.MatchString("x = " + p + strings.Repeat("a", 20)) {
			t.Errorf("%s with a 20-char body must be caught", p)
		}
		if secretPattern.MatchString("x = " + p + strings.Repeat("a", 19)) {
			t.Errorf("%s with a 19-char body must not be caught", p)
		}
	}
}
