package redact

import (
	"bytes"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

type chunkWriter struct{ chunks [][]byte }

func (c *chunkWriter) Write(p []byte) (int, error) {
	c.chunks = append(c.chunks, bytes.Clone(p))
	return len(p), nil
}

// A value split across two writes at any offset is masked, and every other byte arrives unchanged and in order.
func TestWriterMasksValueAtEverySplit(t *testing.T) {
	value := "FAKE-SECRET-0123456789abcdef"
	StubValues(t, []string{value})
	in := "lead " + value + " tail"
	want := "lead " + "[redacted]" + strings.Repeat("*", len(value)-len("[redacted]")) + " tail"
	for cut := 0; cut <= len(in); cut++ {
		var sink chunkWriter
		w := Writer(&sink)
		src := []byte(in)
		for _, part := range [][]byte{src[:cut], src[cut:]} {
			if n, err := w.Write(part); err != nil || n != len(part) {
				t.Fatalf("cut %d: Write = %d, %v", cut, n, err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := string(bytes.Join(sink.chunks, nil)); got != want {
			t.Fatalf("cut %d: got %q, want %q", cut, got, want)
		}
		if string(src) != in {
			t.Fatalf("cut %d: Write modified the caller's bytes", cut)
		}
	}
}

// With no vault values the writer holds nothing back: each write reaches w at once.
func TestWriterPassesThroughWithoutValues(t *testing.T) {
	StubValues(t, nil)
	var sink chunkWriter
	w := Writer(&sink)
	_, _ = w.Write([]byte("abc"))
	if len(sink.chunks) != 1 || string(sink.chunks[0]) != "abc" {
		t.Fatalf("chunks = %q, want one immediate write", sink.chunks)
	}
}
