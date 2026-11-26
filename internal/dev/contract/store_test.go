package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/edotau/claude-code/internal/platform/schemas"
)

var gatePass = TermRecord{Term: TermGate, Verdict: VerdictPass, By: ByGo, Evidence: "exit 0"}

// savedWorking saves a working contract d-1 under a scratch config dir.
func savedWorking(t *testing.T) Contract {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	grace := reapGrace
	reapGrace = 0
	t.Cleanup(func() { reapGrace = grace })
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
	if _, err := Update("d-1", func(c Contract) (Contract, error) { return c.WithRecord(gatePass) }); err != nil {
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

func TestLegacyUnfulfilledContractsMigrateWithoutLosingEvidence(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, state := range []State{StateVerified, StateClosed} {
		c, raw, p := legacyUnfulfilled(t, state)
		got, err := Load(c.ID)
		if err != nil {
			t.Fatal(err)
		}
		oldRecords, _ := json.Marshal(c.Records)
		newRecords, _ := json.Marshal(got.Records)
		if got.State() != StateFailed || got.Failed != ReasonTerm || !bytes.Equal(oldRecords, newRecords) {
			t.Fatalf("legacy evidence lost or false success retained: %+v", got)
		}
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("load changed original legacy bytes", err)
		}
		if err := FailUnreadable(c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(p + ".corrupt"); !os.IsNotExist(err) {
			t.Fatal("readable legacy contract was marked corrupt")
		}
	}
}

func legacyUnfulfilled(t *testing.T, state State) (Contract, []byte, string) {
	t.Helper()
	c := inState(t, state)
	c.ID, c.Holders, c.TermPolicy = string(state), []string{string(state)}, 0
	c.Records = append(c.Records, TermRecord{Term: TermLeaves, Verdict: VerdictFail, By: ByGo, Evidence: "cancelled-child"})
	if c.Result != nil {
		c.Result.Unmet = c.Unmet()
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	p, _ := Path(c.ID)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return c, raw, p
}

func TestFailUnreadableNeverReplacesFutureFormats(t *testing.T) {
	c := savedWorking(t)
	for _, field := range []string{"schema", "term_policy"} {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		doc[field] = 100
		raw, err = json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := Path(c.ID)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(c.ID); !errors.Is(err, schemas.ErrNewer) {
			t.Fatalf("future %s: %v", field, err)
		}
		if err := FailUnreadable(c.ID); err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatalf("future %s replaced: %v", field, err)
		}
	}
}
