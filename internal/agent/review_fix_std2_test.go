package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
)

// Partial copilot text followed by a transient error is not retried: one SDK call, the partial answer kept.
func TestStd2CopilotPartialThenTransientNotRetried(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	calls := stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "half", SessionID: "s1"}, errors.New("send prompt: connection reset")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := Run(ctx, copilotRunner{}, Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard, nil)
	if err == nil {
		t.Fatal("err = nil, want the SDK failure")
	}
	if *calls != 1 {
		t.Errorf("copilotAsk calls = %d, want 1: output was already produced, a retry would re-run the task", *calls)
	}
	if res.Answer != "half" {
		t.Errorf("Answer = %q, want the partial \"half\"", res.Answer)
	}
}
