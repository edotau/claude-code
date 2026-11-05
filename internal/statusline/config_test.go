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
}

// The provider segment is on by default; the statusline shows which provider the session is on.
func TestDefaultConfigProviderOn(t *testing.T) {
	hermetic(t)
	if !loadConfig().seg("provider", false) {
		t.Error("segments.provider should default to true")
	}
}
