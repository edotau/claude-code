package agent

import (
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/dev/plans"
)

// the init wires plans.DispatchHealth to DispatchHealth; an unrecorded task reads healthy with no attempts.
func TestAutofillHealthWired(t *testing.T) {
	got := plans.DispatchHealth(plans.TaskKey{Text: "autofill-health-wired-unrecorded", PlanTask: "x 0.0"})
	if got.Attempts != 0 || got.Degraded && got.Reason == "" {
		t.Fatalf("got %+v, want a healthy or reasoned state", got)
	}
}

// a record saved by the real writer (indented JSON) is found by its batch task id, so a crash is not counted again.
func TestAutofillBatchRecordFindsSavedRecord(t *testing.T) {
	setConfigDir(t)
	since := time.Now().Add(-time.Minute)
	rec := ClaimRecord("claude", RunOptions{}, time.Now())
	rec.TaskID, rec.Status = "autofill-abc-1.1", "error"
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	if status, ok := plans.BatchRecord(rec.TaskID, since); !ok || status != "error" {
		t.Fatalf("BatchRecord = %q, %t; want error, true", status, ok)
	}
	if _, ok := plans.BatchRecord("autofill-other-1.1", since); ok {
		t.Fatal("another batch id matched")
	}
}
