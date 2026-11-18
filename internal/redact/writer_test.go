package redact

import (
	"bytes"
	"errors"
	"io"
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

// failOnce fails its first write only, like a full pipe that later drains.
type failOnce struct{ calls int }

func (f *failOnce) Write(p []byte) (int, error) {
	if f.calls++; f.calls == 1 {
		return 0, errFake
	}
	return len(p), nil
}

var errFake = errors.New("fake write failure")

// overlapPairs holds needle pairs whose matches interact: prefix, suffix, infix and equal-length overlap.
var overlapPairs = [][2]string{
	{"FakeToken1234", "FakeToken1234LongSuffix5678"},
	{"Suffix5678-abc", "PrefixLong-Suffix5678-abc"},
	{"Middle-123456", "Head-Middle-123456-Tail"},
	{"Overlap-AAAA1234", "AAAA1234-Overlap"},
}

// overlapInputs builds texts where the pair's values abut, nest, overlap, repeat, stop short or sit in a mask.
func overlapInputs(a, b string) []string {
	return []string{a, b, "x " + a + " y " + b + " z", a + b, b + a, a + a[len(a)/2:], b[:len(b)-1],
		"pre " + b[:len(b)-3] + " post", "[redacted]***" + a, "[redac" + b, `{"k":"` + a + `\n` + b + `"}`}
}

func streamed(t *testing.T, open func(io.Writer) io.WriteCloser, in string, cut int) []byte {
	var sink chunkWriter
	w := open(&sink)
	_, _ = w.Write([]byte(in[:cut]))
	_, _ = w.Write([]byte(in[cut:]))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.Join(sink.chunks, nil)
}

// The stream masks exactly what the whole-buffer pass masks (plain and JSON), at every split of every input.
func TestWriterMatchesWholeBufferAtEverySplit(t *testing.T) {
	for _, pair := range overlapPairs {
		StubValues(t, pair[:])
		for _, in := range overlapInputs(pair[0], pair[1]) {
			plain, encoded := []byte(in), []byte(in)
			Shared().MaskValues(plain)
			ValueBytes(encoded)
			for cut := 0; cut <= len(in); cut++ {
				if got := streamed(t, Writer, in, cut); !bytes.Equal(got, plain) {
					t.Fatalf("pair %q input %q cut %d: got %q, want %q", pair[0], in, cut, got, plain)
				}
				if got := streamed(t, JSONWriter, in, cut); !bytes.Equal(got, encoded) {
					t.Fatalf("JSON pair %q input %q cut %d: got %q, want %q", pair[0], in, cut, got, encoded)
				}
			}
		}
	}
}

// Text that cannot start a value reaches w at once however long the longest value is.
func TestWriterHoldsOnlyAPossibleValueStart(t *testing.T) {
	StubValues(t, []string{"Zq" + strings.Repeat("FAKE0123456789", 700)})
	var sink chunkWriter
	w := Writer(&sink)
	plain := strings.Repeat("ordinary output line\n", 50)
	_, _ = w.Write([]byte(plain + "Zq"))
	if got := len(bytes.Join(sink.chunks, nil)); got != len(plain) {
		t.Fatalf("delivered %d of %d plain bytes; only the 2-byte value start may wait", got, len(plain))
	}
}

// A failed write to w is reported again by Close, so a caller that checks only Close still sees it.
func TestWriterCloseReportsEarlierWriteError(t *testing.T) {
	StubValues(t, []string{"FAKE-SECRET-0123456789abcdef"})
	w := Writer(&failOnce{})
	_, _ = w.Write([]byte("abc\n"))
	if err := w.Close(); !errors.Is(err, errFake) {
		t.Fatalf("Close = %v, want the write failure", err)
	}
}
