package memory

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestDreamBenchRotationOrder(t *testing.T) {
	ranking := []string{"internal/a", "internal/b", "internal/c", "internal/d", "internal/e"}
	file := filepath.Join(t.TempDir(), "rotation.json")
	state := dreamBenchState{RankedAt: time.Now().UTC(), Ranking: slices.Clone(ranking)}
	for _, want := range [][]string{{"internal/a", "internal/b"}, {"internal/c", "internal/d"}, {"internal/e", "internal/a"}} {
		got := nextDreamBenchPackages(state.Ranking, state.Last)
		if !slices.Equal(got, want) {
			t.Fatalf("after %q: got %v, want %v", state.Last, got, want)
		}
		state.Last = got[len(got)-1]
		if err := saveDreamBenchState(file, state); err != nil {
			t.Fatal(err)
		}
		loaded, err := readDreamBenchState(file)
		if err != nil || !reflect.DeepEqual(loaded, state) {
			t.Fatalf("rotation reload = %+v, %v; want %+v", loaded, err, state)
		}
		state = loaded
	}
	for _, tc := range []struct {
		ranking []string
		last    string
		want    []string
	}{
		{nil, "", nil}, {[]string{"one"}, "one", []string{"one"}},
		{ranking, "removed", ranking[:2]}, {[]string{"internal/c", "internal/b", "internal/a"}, "internal/b", []string{"internal/a", "internal/c"}},
	} {
		if got := nextDreamBenchPackages(tc.ranking, tc.last); !slices.Equal(got, tc.want) {
			t.Fatalf("next(%v, %q) = %v, want %v", tc.ranking, tc.last, got, tc.want)
		}
	}
	if !slices.Equal(state.Ranking, ranking) {
		t.Fatalf("ranking mutated: %v", state.Ranking)
	}
}
