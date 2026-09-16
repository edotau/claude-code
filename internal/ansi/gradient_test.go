package ansi

import (
	"math"
	"testing"
)

func TestGradAtEndpoints(t *testing.T) {
	stops := []RGB{{0, 0, 0}, {255, 255, 255}}
	if got := GradAt(stops, 0); got != (RGB{0, 0, 0}) {
		t.Errorf("GradAt frac=0 = %v, want black", got)
	}
	if got := GradAt(stops, 1); got != (RGB{255, 255, 255}) {
		t.Errorf("GradAt frac=1 = %v, want white", got)
	}
	// Out of range clamps rather than indexing past the ramp.
	if got := GradAt(stops, -3); got != (RGB{0, 0, 0}) {
		t.Errorf("GradAt frac=-3 = %v, want black", got)
	}
	if got := GradAt(stops, 9); got != (RGB{255, 255, 255}) {
		t.Errorf("GradAt frac=9 = %v, want white", got)
	}
	// single stop → flat color at any fraction
	flat := []RGB{{10, 20, 30}}
	if got := GradAt(flat, 0.5); got != (RGB{10, 20, 30}) {
		t.Errorf("GradAt single-stop = %v, want flat", got)
	}
	// no stops → neutral gray, never panics
	if got := GradAt(nil, 0.5); got != (RGB{150, 150, 160}) {
		t.Errorf("GradAt no-stops = %v, want neutral gray", got)
	}
}

func TestMixAndTowardWhite(t *testing.T) {
	if got := Mix(RGB{0, 0, 0}, RGB{255, 255, 255}, 0.5); got != (RGB{128, 128, 128}) {
		t.Errorf("Mix midpoint = %v, want 128 (round-half-away-from-zero)", got)
	}
	if got := Mix(RGB{10, 20, 30}, RGB{200, 200, 200}, 0); got != (RGB{10, 20, 30}) {
		t.Errorf("Mix w=0 = %v, want the left stop", got)
	}
	if got := Mix(RGB{10, 20, 30}, RGB{200, 200, 200}, 1); got != (RGB{200, 200, 200}) {
		t.Errorf("Mix w=1 = %v, want the right stop", got)
	}
	if got := TowardWhite(RGB{0, 100, 200}, 0.5); got != (RGB{128, 178, 228}) {
		t.Errorf("TowardWhite 50%% = %v, want {128 178 228}", got)
	}
	if got := TowardWhite(RGB{7, 8, 9}, 1); got != (RGB{255, 255, 255}) {
		t.Errorf("TowardWhite w=1 = %v, want white", got)
	}
}

// legacySpan is the span selector the cli package carried in its own sampler before both callers were
// folded onto GradAt: a linear scan comparing t against each boundary, where GradAt divides. The two
// forms disagree the moment a float lands a hair either side of a boundary, and the symptom would be a
// silently repainted sweep — nothing downstream compares an escape against the ramp it came from.
func legacySpan(n int, t float64) int {
	step := 100 / float64(n-1)
	k := 0
	for k < n-2 && t >= float64(k+1)*step {
		k++
	}
	return k
}

// gradAtSpan mirrors GradAt's own selector so a sweep can compare the two decisions directly.
func gradAtSpan(n int, t float64) int {
	step := 100 / float64(n-1)
	k := int(t / step)
	if k > n-2 {
		k = n - 2
	}
	return k
}

func TestGradAtSpanMatchesTheLegacyScan(t *testing.T) {
	for n := 2; n <= 6; n++ {
		stops := make([]RGB, n)
		for i := range stops {
			stops[i] = RGB{i * 37 % 256, 255 - i*29%256, i * 61 % 256}
		}
		check := func(frac float64) {
			clamped := math.Max(0, math.Min(100, frac*100))
			want := legacySpan(n, clamped)
			if got := gradAtSpan(n, clamped); got != want {
				t.Fatalf("n=%d frac=%.17g: span %d, legacy scan says %d", n, frac, got, want)
			}
			// The span index is the only thing that differed; prove the sampled color agrees too.
			a, b := stops[want], stops[want+1]
			step := 100 / float64(n-1)
			if w := GradAt(stops, frac); w != Mix(a, b, smooth((clamped-float64(want)*step)/step)) {
				t.Fatalf("n=%d frac=%.17g: GradAt = %v, legacy path = %v",
					n, frac, w, Mix(a, b, smooth((clamped-float64(want)*step)/step)))
			}
		}
		// Every exact span boundary and its immediate float neighbours — where the two forms can part.
		for k := 0; k <= n-1; k++ {
			b := float64(k) / float64(n-1)
			for _, f := range []float64{b, math.Nextafter(b, 0), math.Nextafter(b, 1)} {
				check(f)
			}
		}
		for _, f := range []float64{-1, -0.0001, 0, 1, 1.0001, 2} {
			check(f)
		}
		const steps = 20001
		for i := 0; i < steps; i++ {
			check(float64(i) / float64(steps-1))
		}
	}
}
