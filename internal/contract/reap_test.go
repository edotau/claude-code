package contract

import (
	"os"
	"testing"
	"time"
)

func TestReapFailsLostOwnerOnceAndQueues(t *testing.T) {
	savedWorking(t)
	var queued []string
	env := Env{Lost: func(id string) bool { return id == "d-1" },
		Queue: func(c Contract) error { queued = append(queued, c.ID); return nil }}
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

func TestReapQueueErrorLeavesContractWorking(t *testing.T) {
	c := savedWorking(t)
	c.Scope.Lane = "lane-a"
	if _, err := Update("d-1", func(Contract) (Contract, error) { return c, nil }); err != nil {
		t.Fatal(err)
	}
	env := Env{Lost: func(string) bool { return true }, Queue: func(Contract) error { return os.ErrPermission }}
	if got, err := Reap(env); err == nil || len(got) != 0 {
		t.Fatalf("Reap = %v, %v; want an error and nothing reaped", got, err)
	}
	if got, _ := Load("d-1"); got.State() != StateWorking {
		t.Fatalf("state %v; want working so the next pass retries", got.State())
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
