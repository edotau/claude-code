package browser

import (
	"errors"
	"testing"
)

type failingRenderWriter struct {
	writes, failAt int
	first, later   error
}

func (w *failingRenderWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, w.first
	}
	if w.writes > w.failAt {
		return 0, w.later
	}
	return len(p), nil
}

func TestRenderTextReturnsFirstWriteError(t *testing.T) {
	r := Report{Default: "chrome", Browsers: []Browser{
		{Name: "chrome", Profiles: []Profile{{Name: "Default"}, {Name: "Other"}}},
		{Name: "firefox"},
	}}
	for _, failAt := range []int{1, 2, 3, 4, 5} {
		t.Run(itoa(failAt), func(t *testing.T) {
			first := errors.New("first write failed")
			w := &failingRenderWriter{failAt: failAt, first: first, later: errors.New("later write failed")}
			if err := RenderText(w, r); !errors.Is(err, first) {
				t.Fatalf("write %d: error=%v, want first writer error", failAt, err)
			}
			if w.writes != failAt {
				t.Fatalf("continued after failure at write %d: %d writes", failAt, w.writes)
			}
		})
	}
}
