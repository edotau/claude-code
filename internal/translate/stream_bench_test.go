package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// BenchmarkStream drives StreamChat over a real NVIDIA (OpenAI-compatible) chat-completions SSE
// capture (internal/translate/testdata/stream_openai.sse): text deltas, tool-call deltas, [DONE].
func BenchmarkStream(b *testing.B) {
	raw, err := os.ReadFile("testdata/stream_openai.sse")
	if err != nil {
		b.Fatal(err)
	}
	// Correctness check once, outside the timed loop: the output hash is logged so a before/after
	// run can be diffed by eye; StreamChat itself must still produce non-empty, error-free output.
	var golden bytes.Buffer
	if err := StreamChat(&golden, nil, bytes.NewReader(raw), "bench-model"); err != nil {
		b.Fatalf("StreamChat: %v", err)
	}
	if golden.Len() == 0 {
		b.Fatal("StreamChat produced no output")
	}
	sum := sha256.Sum256(golden.Bytes())
	b.Logf("output sha256: %s (%d bytes)", hex.EncodeToString(sum[:]), golden.Len())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := StreamChat(io.Discard, nil, bytes.NewReader(raw), "bench-model"); err != nil {
			b.Fatal(err)
		}
	}
}
