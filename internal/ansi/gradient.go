package ansi

import "math"

// smooth is the smoothstep easing x*x*(3-2x), clamped to [0,1].
func smooth(x float64) float64 {
	x = math.Max(0, math.Min(1, x))
	return x * x * (3 - 2*x)
}

// TowardWhite lightens rgb by fraction w toward white.
func TowardWhite(rgb RGB, w float64) RGB {
	return RGB{
		int(math.Round(float64(rgb[0]) + (255-float64(rgb[0]))*w)),
		int(math.Round(float64(rgb[1]) + (255-float64(rgb[1]))*w)),
		int(math.Round(float64(rgb[2]) + (255-float64(rgb[2]))*w)),
	}
}

// Mix linearly interpolates a→b by fraction w.
func Mix(a, b RGB, w float64) RGB {
	return RGB{
		int(math.Round(float64(a[0]) + (float64(b[0])-float64(a[0]))*w)),
		int(math.Round(float64(a[1]) + (float64(b[1])-float64(a[1]))*w)),
		int(math.Round(float64(a[2]) + (float64(b[2])-float64(a[2]))*w)),
	}
}

// GradAt samples the gradient at frac∈[0,1] across stops, smoothstep-eased per span.
// 0 stops → a neutral gray; 1 stop → that flat color.
func GradAt(stops []RGB, frac float64) RGB {
	n := len(stops)
	if n == 0 {
		return RGB{150, 150, 160}
	}
	if n == 1 {
		return stops[0]
	}
	// Uniform stop grid: arithmetic span index, no per-rune allocation.
	t := math.Max(0, math.Min(100, frac*100))
	step := 100 / float64(n-1)
	k := int(t / step)
	if k > n-2 {
		k = n - 2
	}
	lt := smooth((t - float64(k)*step) / step)
	return Mix(stops[k], stops[k+1], lt)
}
