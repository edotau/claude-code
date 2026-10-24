package statusline

import (
	"reflect"
	"testing"
)

// The default segment order is model, agents, then the rate limits ahead of the context bar.
func TestDefaultConfigOrder(t *testing.T) {
	hermetic(t)
	cfg := loadConfig()
	want := []string{"model", "agents", "provider", "router", "limits", "context", "tokens", "duration", "session"}
	if !reflect.DeepEqual(cfg.Order, want) {
		t.Errorf("Order = %v, want %v", cfg.Order, want)
	}
	if !cfg.seg("limits", false) {
		t.Error("segments.limits should default to true")
	}
	if cfg.PathMax != 24 {
		t.Errorf("PathMax = %d, want 24", cfg.PathMax)
	}
}
