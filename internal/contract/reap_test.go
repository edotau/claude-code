package contract

import (
	"os"
	"testing"
	"time"
)

func TestReapFailsLostOwnerOnce(t *testing.T) {
	savedWorking(t)
	env := Env{Lost: func(id string) bool { return id == "d-1" }}
	got, err := Reap(env)
	if err != nil || len(got) != 1 || got[0].Action != "failed" {
		t.Fatalf("Reap = %v, %v; want one failed", got, err)
	}
	c, _ := Load("d-1")
	if c.State() != StateFailed || c.Failed != ReasonOwnerDead || c.Result == nil || len(c.Result.Unmet) == 0 {
		t.Fatalf("contract = %v %v %+v; want failed owner-dead with unmet filled", c.State(), c.Failed, c.Result)
	}
	if again, _ := Reap(env); len(again) != 0 {
		t.Fatalf("second Reap acted on %v; want nothing", again)
	}
}

func TestReapReassignsOnce(t *testing.T) {
	savedWorking(t)
	env := Env{Lost: func(string) bool { return true }, Successor: func(Contract) string { return "d-2" }}
	got, err := Reap(env)
	if err != nil || len(got) != 1 || got[0].Action != "reassigned" {
		t.Fatalf("Reap = %v, %v; want one reassigned", got, err)
	}
	got, err = Reap(env)
	if err != nil || len(got) != 1 || got[0].Action != "failed" {
		t.Fatalf("second Reap = %v, %v; want failed after one re-assign", got, err)
	}
}

func TestSummarizeCountsAndStaleVerified(t *testing.T) {
	savedWorking(t)
	s, err := Summarize(time.Now().Add(2 * time.Hour))
	if err != nil || s.ByState[StateWorking] != 1 || s.Oldest != "d-1" || len(s.StaleVerified) != 0 {
		t.Fatalf("Summarize = %+v, %v; want one working, oldest d-1", s, err)
	}
}

func TestReapSparesAFreshContractAndFailUnreadableStubs(t *testing.T) {
	savedWorking(t)
	reapGrace = time.Hour
	if got, _ := Reap(Env{Lost: func(string) bool { return true }}); len(got) != 0 {
		t.Fatalf("Reap acted on a fresh contract: %v", got)
	}
	p, _ := Path("d-1")
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := FailUnreadable("d-1"); err != nil {
		t.Fatal(err)
	}
	if c, err := Load("d-1"); err != nil || c.State() != StateFailed || c.Failed != ReasonUnreadable {
		t.Fatalf("after FailUnreadable: %v %v, %v; want failed unreadable", c.State(), c.Failed, err)
	}
}

// TestReapSuccessorRunsOutsideTheLock: Successor may itself Update the contract, so the re-send holds no lock.
func TestReapSuccessorRunsOutsideTheLock(t *testing.T) {
	savedWorking(t)
	done := make(chan error, 1)
	env := Env{Lost: func(string) bool { return true }, Successor: func(c Contract) string {
		go func() {
			_, err := Update(c.ID, func(c Contract) (Contract, error) { return c, nil })
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Update in Successor: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Update inside Successor blocked: the contract is locked")
		}
		return "d-2"
	}}
	if got, err := Reap(env); err != nil || len(got) != 1 || got[0].Action != "reassigned" {
		t.Fatalf("Reap = %v, %v; want one reassigned", got, err)
	}
}
