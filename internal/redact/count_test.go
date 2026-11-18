package redact

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"

	_ "github.com/edotau/claude-code/internal/nonet"
)

// countPieces mixes values that nest, overlap or span lines with every shape that can reach across a line break.
var countPieces = []string{"FakeToken1234", "FakeToken1234LongSuffix5678", "Overlap-AAAA1234-Overlap", "Pem\nLine-1234",
	"[redacted]****", "bearer \n", "Bearer " + strings.Repeat("Ab1_", 12), "-----BEGIN PRIVATE KEY-----\nabc\n",
	"-----END PRIVATE KEY-----", "eyJ" + strings.Repeat("a", 20) + "." + strings.Repeat("b", 20),
	"sk-" + strings.Repeat("a", 200), "AKIA" + strings.Repeat("B", 16), "\n", "\n", " ", "x", ". "}

// The windowed count equals Mask on the whole input, for random text with matches planted across window edges.
func TestCounterMatchesMask(t *testing.T) {
	m := FromValues([]string{"FakeToken1234", "FakeToken1234LongSuffix5678", "Overlap-AAAA1234", "AAAA1234-Overlap", "Pem\nLine-1234"})
	c := &Counter{matcher: m, window: 64, piece: 16}
	r := rand.New(rand.NewPCG(3, 4))
	cut, cases := 0, 1500
	for i := range cases {
		var in []byte
		for range 8 + r.IntN(40) {
			in = append(in, countPieces[r.IntN(len(countPieces))]...)
		}
		want := m.Mask(bytes.Clone(in))
		bufs := &countBuffers{}
		got, err := c.count(bytes.NewReader(in), bufs)
		if err != nil || got != want {
			t.Fatalf("case %d: count %d, Mask %d, error %v, input %q", i, got, want, err, in)
		}
		if cap(bufs.data) < len(in) {
			cut++
		}
	}
	if cut < cases/4 {
		t.Fatalf("only %d of %d inputs were cut into windows", cut, cases)
	}
}
