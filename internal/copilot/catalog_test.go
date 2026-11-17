package copilot

import (
	"testing"

	"github.com/edotau/claude-code/internal/providers"
)

func BenchmarkCopilotUsable(b *testing.B) {
	for _, ready := range []bool{false, true} {
		name := "unavailable"
		if ready {
			name = "ready"
		}
		b.Run(name, func(b *testing.B) {
			infos := make([]providers.ModelInfo, 64)
			for i := range infos {
				infos[i] = providers.ModelInfo{ID: "model", Task: "llm/v1/chat"}
				if ready {
					infos[i].Ready = "READY"
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if copilotUsable(infos) != ready {
					b.Fatal("unexpected catalog usability")
				}
			}
		})
	}
}
