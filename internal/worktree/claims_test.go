package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestScanClaimsReportsLiveLedgerErrors(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(ClaimsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(ClaimsDir(), "101-open.jsonl")
	if err := os.Symlink(unreadable, unreadable); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ClaimsDir(), "102-scan.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ClaimsDir(), "103-readable.jsonl"), []byte("{\"path\":\"dirty.go\",\"agent_id\":\"worker\",\"ts\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	alive := func(int) bool { return true }
	err := ScanClaims(alive, func(pid int, slug string, c Claim) {
		got = append(got, Label(pid, slug, c.Agent)+":"+c.Path)
	})
	for _, want := range []error{syscall.ELOOP, syscall.EISDIR} {
		if !errors.Is(err, want) {
			t.Errorf("ScanClaims error = %v, want live ledger failure %v", err, want)
		}
	}
	label := "leaf worker of session readable (pid 103)"
	if !reflect.DeepEqual(got, []string{label + ":dirty.go"}) {
		t.Errorf("ScanClaims claims = %v, want readable owner's dirty.go claim", got)
	}
	holders, err := Holders(alive, []string{"dirty.go"})
	if !reflect.DeepEqual(holders["dirty.go"], []string{label}) {
		t.Errorf("Holders = %v, want readable owner's claim despite ledger errors", holders)
	}
	if !errors.Is(err, syscall.ELOOP) || !errors.Is(err, syscall.EISDIR) {
		t.Errorf("Holders error = %v, want the live ledger failures", err)
	}
}
