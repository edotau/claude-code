package desktop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

// captureStderr returns what fn wrote to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(done) }()
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	<-done
	return buf.String()
}

func TestDesktopModelsFilterAndDisplayNames(t *testing.T) {
	old := gatewayModels
	t.Cleanup(func() { gatewayModels = old })
	gatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{
			{ID: "claude-sonnet-4-6", Name: "Sonnet Display", Gateway: true},
			{ID: "gpt-5", Name: "not Claude", Gateway: true},
			{ID: "claude-sonnet-4-6", Name: "duplicate", Gateway: true},
		}, nil
	}
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks,
		Options: providers.Options{Wire: providers.WireAIGateway},
		BaseURL: "https://example.com/ai-gateway/anthropic", Protocol: providers.ProtocolAnthropic}
	reg := &providers.Registry{}
	got, err := ModelList(context.Background(), reg, p)
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" || got[0].LabelOverride != "Sonnet Display" {
		t.Fatalf("models = %+v, %v", got, err)
	}
	gatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return nil, errors.New("upstream error with secret")
	}
	p.Models = map[string]string{"sonnet": "claude-sonnet-4-6[1m]", "opus": "claude-sonnet-4-6", "haiku": "gpt-5"}
	stderr := captureStderr(t, func() { got, err = ModelList(context.Background(), reg, p) })
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" || got[0].LabelOverride == "claude-sonnet-4-6" {
		t.Fatalf("fallback models = %+v, %v", got, err)
	}
	if strings.Contains(stderr, "secret") {
		t.Fatal("catalog warning leaked upstream error")
	}
}

func TestDesktopModelsAllResolveOnProfileRoute(t *testing.T) {
	old := gatewayModels
	t.Cleanup(func() { gatewayModels = old })
	gatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{{ID: "claude-sonnet-4-6", Name: "S"}, {ID: "other:claude-opus-4-6", Name: "O"}}, nil
	}
	dev := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Options: providers.Options{Wire: providers.WireAIGateway},
		BaseURL: "https://example.com/ai-gateway/anthropic", Protocol: providers.ProtocolAnthropic}
	other := &providers.Provider{Name: "other"}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": dev, "other": other}}
	got, err := ModelList(context.Background(), reg, dev)
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" {
		t.Fatalf("models = %+v, %v", got, err)
	}
	for _, m := range got {
		if tgt := providers.ParseTarget(reg, m.Name, dev); tgt.Provider.Name != "dev" {
			t.Fatalf("%s routes to %s", m.Name, tgt.Provider.Name)
		}
	}
}
