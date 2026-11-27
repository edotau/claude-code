package copilot

import (
	"errors"
	"testing"
)

func TestSDKRuntimeNotLinked(t *testing.T) {
	ask, list := SDKAsk, SDKListModels
	SDKAsk, SDKListModels = nil, nil
	t.Cleanup(func() { SDKAsk, SDKListModels = ask, list })
	if _, err := copilotAsk(t.Context(), Options{}, "hi", nil); !errors.Is(err, errNoSDK) {
		t.Fatalf("Ask without SDK = %v, want %v", err, errNoSDK)
	}
	if _, err := sdkListModels(t.Context(), Options{}); !errors.Is(err, errNoSDK) {
		t.Fatalf("ListModels without SDK = %v, want %v", err, errNoSDK)
	}
}
