package serving

import (
	"fmt"
	"strconv"
	"testing"
)

func benchServed(n int) Served {
	s := Served{Profile: "bench"}
	for i := range n {
		s.Models = append(s.Models, ServedModel{ID: fmt.Sprintf("databricks-model-%03d", i), Ready: "READY"})
	}
	return s.indexed()
}

func BenchmarkFindWire(b *testing.B) {
	sizes := []int{60, 500}
	for _, n := range sizes {
		s := benchServed(n)
		keys := []string{fmt.Sprintf("model-%03d", 0), fmt.Sprintf("model-%03d", n/2), fmt.Sprintf("model-%03d", n-1), "absent"}
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			for i := range b.N {
				s.FindWire(keys[i%len(keys)])
			}
		})
	}
}

func TestFindWireIndexMatchesScan(t *testing.T) {
	s := Served{Models: []ServedModel{{ID: "databricks-a", Task: "1"}, {ID: "a", Task: "2"}, {ID: "b"}, {ID: "databricks-b"}, {ID: "a", Task: "3"}}}
	ix := s.indexed()
	for _, k := range []string{"a", "b", "databricks-a", "databricks-b", "c", ""} {
		want, wok := s.FindWire(k)
		got, gok := ix.FindWire(k)
		if want.ID != got.ID || want.Task != got.Task || wok != gok {
			t.Errorf("%q: scan %v/%v, index %v/%v", k, want, wok, got, gok)
		}
	}
}
