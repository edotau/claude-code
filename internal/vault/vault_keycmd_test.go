package vault

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keyCommandHelperArgv builds a key_command argv that re-execs this test binary as
// TestKeyCommandHelperProcess instead of a POSIX shell script, so key_command tests run
// unmodified on every OS (review's LOW #3).
func keyCommandHelperArgv(extra ...string) []string {
	executable, err := os.Executable()
	if err != nil {
		panic(err)
	}
	argv := []string{executable, "-test.run=^TestKeyCommandHelperProcess$", "-test.timeout=10s"}
	return append(argv, extra...)
}

// TestKeyCommandHelperProcess is not a real test: run as a key_command subprocess (guarded by
// VAULT_KEY_COMMAND_HELPER), it prints the configured key value and, if VAULT_KEY_COMMAND_ARGS_FILE
// is set, records its own non-test-flag arguments for the caller to inspect.
func TestKeyCommandHelperProcess(t *testing.T) {
	if os.Getenv("VAULT_KEY_COMMAND_HELPER") != "1" {
		return
	}
	var extra []string
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-test.") {
			extra = append(extra, a)
		}
	}
	if argsFile := os.Getenv("VAULT_KEY_COMMAND_ARGS_FILE"); argsFile != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "%d\n", len(extra))
		for _, a := range extra {
			fmt.Fprintf(&b, "%s\n", a)
		}
		if err := os.WriteFile(argsFile, []byte(b.String()), 0600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stdout, os.Getenv("VAULT_KEY_COMMAND_VALUE"))
	os.Exit(0)
}

// TestResolveKeyTriesCommandAfterUnavailableKeyring reproduces the review's HIGH #3 scenario: an
// explicitly configured key_command must still unlock the vault when the keyring could not
// positively establish absence (ErrUnavailable — no tool, locked session, or Windows), not only
// on a clean miss.
func TestResolveKeyTriesCommandAfterUnavailableKeyring(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	key, err := EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("VAULT_KEY_COMMAND_HELPER", "1")
	t.Setenv("VAULT_KEY_COMMAND_VALUE", key.raw)
	argv, err := json.Marshal(keyCommandHelperArgv())
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error { return tx.Put(Meta, "key_command", string(argv)) }); err != nil {
		t.Fatal(err)
	}
	invalidateKey()
	ring.mu.Lock()
	ring.err = ErrKeyringUnavailable
	ring.mu.Unlock()

	got, source, err := MasterKey()
	if err != nil || got == nil || source != "command" {
		t.Fatalf("MasterKey = (%v, %q, %v), want the configured command to unlock an unavailable keyring", got, source, err)
	}
	if got.Kid != key.Kid {
		t.Fatalf("command-resolved kid %q, want %q", got.Kid, key.Kid)
	}
}

// TestResolveKeyUnavailableNoCommandIsLockedWithActionableMessage is the other half of #3: with no
// key_command configured, an unavailable keyring must still surface as ErrLocked with the recovery
// instructions, not the raw keyring error.
func TestResolveKeyUnavailableNoCommandIsLockedWithActionableMessage(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	invalidateKey()
	ring.mu.Lock()
	ring.err = ErrKeyringUnavailable
	ring.mu.Unlock()

	_, _, err := MasterKey()
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("MasterKey = %v, want ErrLocked", err)
	}
	if !strings.Contains(err.Error(), "HARNESS_VAULT_KEY") || !strings.Contains(err.Error(), "key_command") {
		t.Errorf("locked error omits recovery instructions: %v", err)
	}
}

// TestKeyCommandArgvJSON stores meta/key_command as a validated JSON argv array (R6): no shell is
// ever involved, so an argument containing shell metacharacters, quotes, and a Windows-style
// backslash path all run as exactly the arguments given — never re-tokenized.
func TestKeyCommandArgvJSON(t *testing.T) {
	phase1bRing(t)
	// Kid identifies the root alone (independent of vaultID); this test only compares Kid.
	key, raw, err := randomKey("")
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("VAULT_KEY_COMMAND_HELPER", "1")
	t.Setenv("VAULT_KEY_COMMAND_VALUE", raw)
	t.Setenv("VAULT_KEY_COMMAND_ARGS_FILE", argsFile)
	extra := []string{"literal;value", `has "quotes"`, `C:\Windows\style\path.exe`}
	argv := keyCommandHelperArgv(extra...)
	encoded, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(func(tx *Tx) error {
		return tx.Put(Meta, "key_command", string(encoded))
	}); err != nil {
		t.Fatal(err)
	}
	got, source, err := MasterKey()
	if err != nil || got == nil || source != "command" {
		t.Fatalf("MasterKey argv command = (%v, %q, %v)", got, source, err)
	}
	if got.Kid != key.Kid {
		t.Fatalf("key command returned kid %q, want %q", got.Kid, key.Kid)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\n%s\n%s\n%s\n", len(extra), extra[0], extra[1], extra[2])
	if string(args) != want {
		t.Fatalf("argv capture %q, want %q (each argument literal, no shell re-tokenizing)", args, want)
	}
}

// TestKeyCommandArgvRejectsBadConfig covers the validation half of R6: a non-array value and an
// empty array must both be rejected with a clear error, never silently tried as a shell string.
func TestKeyCommandArgvRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
	}{
		{"not_json", "helper --label vault"},
		{"json_string_not_array", `"helper"`},
		{"empty_array", `[]`},
		{"empty_executable", `["", "x"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseKeyCommandArgv(tc.raw); err == nil {
				t.Fatalf("parseKeyCommandArgv(%q) succeeded, want an error", tc.raw)
			}
		})
	}
}

// TestResolveKeyFallsBackToPerKidAccount reproduces the review's MEDIUM #1 scenario: with a
// committed kid and an intact per-kid account, a missing or malformed convenience pointer must
// still resolve the key by consulting that per-kid account directly, never ErrLocked.
func TestResolveKeyFallsBackToPerKidAccount(t *testing.T) {
	for _, tc := range []string{"pointer_deleted", "pointer_garbage"} {
		t.Run(tc, func(t *testing.T) {
			ring := phase1bRing(t)
			phase1bPut(t, "TOKEN", "value")
			acct := "vault-key/" + phase1bMeta(t, "vault_id")
			invalidateKey()
			ring.mu.Lock()
			if tc == "pointer_deleted" {
				delete(ring.values, acct)
			} else {
				ring.values[acct] = "not-a-valid-key"
			}
			ring.mu.Unlock()
			phase1bRead(t, "TOKEN", "value")
		})
	}
}

// TestResolveKeyMissingEverythingIsLockedNeverBootstraps is the other half of MEDIUM #1: a kid
// recorded with neither a per-kid account nor a pointer must surface ErrLocked, never a freshly
// generated key.
func TestResolveKeyMissingEverythingIsLockedNeverBootstraps(t *testing.T) {
	ring := phase1bRing(t)
	phase1bPut(t, "TOKEN", "value")
	acct := "vault-key/" + phase1bMeta(t, "vault_id")
	kid := phase1bMeta(t, "kid")
	invalidateKey()
	ring.mu.Lock()
	delete(ring.values, acct)
	delete(ring.values, acct+"."+kid)
	ring.mu.Unlock()
	key, err := EnsureKey()
	if key != nil || !errors.Is(err, ErrLocked) {
		t.Fatalf("EnsureKey = (%v, %v), want nil, ErrLocked", key, err)
	}
}

// TestParseKeyCommandArgvAllowsEmptyArgument covers the review's LOW #2: a valid argv may contain
// an empty argument after the executable at index 0 — only the executable itself must be nonempty.
func TestParseKeyCommandArgvAllowsEmptyArgument(t *testing.T) {
	want := []string{"helper", "--label", ""}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseKeyCommandArgv(string(raw))
	if err != nil {
		t.Fatalf("parseKeyCommandArgv(%q) = %v, want the exact argv round-tripped", raw, err)
	}
	if len(got) != len(want) {
		t.Fatalf("parseKeyCommandArgv = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseKeyCommandArgv = %#v, want %#v", got, want)
		}
	}
}
