package contract

import (
	"os"
	"reflect"
	"testing"
)

var gatePass = TermRecord{Term: TermGate, Verdict: VerdictPass, By: ByGo, Evidence: "exit 0"}

// savedWorking saves a working contract d-1 under a scratch config dir.
func savedWorking(t *testing.T) Contract {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	c, err := opened(t, RoleOwner, 1, 2).Start("d-1")
	if err == nil {
		err = Save(c)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSaveLoadRoundTrip(t *testing.T) {
	c := savedWorking(t)
	if err := Record("d-1", gatePass); err != nil {
		t.Fatal(err)
	}
	got, err := Load("d-1")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := c.WithRecord(gatePass); !reflect.DeepEqual(got, want) {
		t.Fatalf("loaded %+v, want %+v", got, want)
	}
}

func TestSaveModeAndPending(t *testing.T) {
	savedWorking(t)
	p, _ := Path("d-1")
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file %v, %v; want mode 0600", fi, err)
	}
	if pend, err := Pending(); err != nil || len(pend) != 1 || pend[0].ID != "d-1" {
		t.Fatalf("Pending = %v, %v; want the one working contract", pend, err)
	}
}
