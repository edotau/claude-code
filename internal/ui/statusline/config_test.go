package statusline

import (
	"reflect"
	"testing"
)

// The default order puts the context bar right after the model, then the rate limits; the agent count is on.
func TestDefaultConfigOrder(t *testing.T) {
	hermetic(t)
	cfg := loadConfig()
	want := []string{"provider", "model", "proxy", "context", "limits", "total", "duration", "session"}
	if !reflect.DeepEqual(cfg.Order, want) {
		t.Errorf("Order = %v, want %v", cfg.Order, want)
	}
	if !cfg.seg("limits", false) {
		t.Error("segments.limits should default to true")
	}
	if !cfg.seg("agents", false) {
		t.Error("segments.agents should default to true")
	}
	if cfg.GradientEase != 0.45 {
		t.Errorf("GradientEase = %v, want 0.45", cfg.GradientEase)
	}
	if cfg.PathMax != 24 {
		t.Errorf("PathMax = %d, want 24", cfg.PathMax)
	}
}

// The provider segment is on by default; the statusline shows which provider the session is on.
func TestDefaultConfigProviderOn(t *testing.T) {
	hermetic(t)
	if !loadConfig().seg("provider", false) {
		t.Error("segments.provider should default to true")
	}
}
