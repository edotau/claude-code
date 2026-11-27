package sdk

import (
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/driver/copilot"
)

// Runtime registration reaches SDK validation without launching a CLI.
func TestSDKRuntimeLinked(t *testing.T) {
	if copilot.SDKAsk == nil || copilot.SDKListModels == nil {
		t.Fatal("SDK init did not register both runtime functions")
	}
	_, err := copilot.SDKAsk(t.Context(), copilot.Options{}, "hi", nil)
	if err == nil || !strings.Contains(err.Error(), "copilot CLI path is empty") {
		t.Fatalf("Ask did not reach SDK validation: %v", err)
	}
	_, err = copilot.SDKListModels(t.Context(), copilot.Options{})
	if err == nil || !strings.Contains(err.Error(), "copilot CLI path is empty") {
		t.Fatalf("ListModels did not reach SDK validation: %v", err)
	}
}
