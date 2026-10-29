package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/edotau/claude-code/internal/copilotsdk"
)

// A copilot failure that is not a quota handoff still returns the partial text and the session id.
func TestStd2CopilotNonHandoffErrorKeepsPartial(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "half an answer", SessionID: "sess-9"}, errors.New("send prompt: connection reset")
	})
	res, err := copilotRunner{}.Run(context.Background(), Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard)
	if err == nil {
		t.Fatal("err = nil, want the SDK failure")
	}
	if res.Handoff != "" {
		t.Fatalf("Handoff = %q: this must be the NON-quota path", res.Handoff)
	}
	if res.Answer != "half an answer" {
		t.Errorf("Answer = %q, want the partial text \"half an answer\"", res.Answer)
	}
	if res.SessionID != "sess-9" {
		t.Errorf("SessionID = %q, want \"sess-9\"", res.SessionID)
	}
}

// Copilot reports no token usage, so a successful run's envelope has usage null and fallback null.
func TestStd2CopilotResultJSONUsageNull(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	stubAsk(t, func(int) (copilotsdk.Result, error) {
		return copilotsdk.Result{Text: "hello", SessionID: "sess-1", Model: "m"}, nil
	})
	res, err := Run(context.Background(), copilotRunner{}, Request{Prompt: "p", Provider: "personal", Direct: true}, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(NewEnvelope(res, nil, "id-1"))
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if v, ok := m["usage"]; !ok || v != nil {
		t.Errorf("usage = %v (present=%v), want explicit null: copilot reports none", v, ok)
	}
	if v, ok := m["fallback"]; !ok || v != nil {
		t.Errorf("fallback = %v, want null when the requested agent answered", m["fallback"])
	}
}
