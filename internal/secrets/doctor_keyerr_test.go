package secrets

import (
	"errors"
	"testing"
)

func TestKeyErrTextWithholdsKeyCommandStderr(t *testing.T) {
	got := keyErrText(errors.New("vault: key_command: exit status 1: leaked-secret-xyz"))
	if got != "vault: key_command: exit status 1 (stderr withheld)" {
		t.Fatalf("keyErrText = %q", got)
	}
	if got := keyErrText(errors.New("keyring: unavailable")); got != "keyring: unavailable" {
		t.Fatalf("keyErrText = %q", got)
	}
}
