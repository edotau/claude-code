package waves

import (
	"context"
	"io"
	"slices"
	"testing"
)

// roundLauncher records each round's ids and fails leaf 1.4's dispatch.
func roundLauncher(rounds *[][]string) func(context.Context, string, []Spec, LaunchOptions) ([]Envelope, error) {
	return func(_ context.Context, _ string, specs []Spec, _ LaunchOptions) ([]Envelope, error) {
		var ids []string
		var out []Envelope
		for _, s := range specs {
			ids = append(ids, s.ID)
			status := LeafStateOK
			if s.ID == "1.4" {
				status = "error"
			}
			out = append(out, Envelope{TaskID: "p-" + s.ID, Status: status})
		}
		*rounds = append(*rounds, ids)
		return out, nil
	}
}

func TestLaunchRoundsOrdersByBlocker(t *testing.T) {
	var rounds [][]string
	r := runner{slug: "p", h: runHooks{launch: roundLauncher(&rounds)}}
	r.o.Executable, r.reports, r.dir = "cc", t.TempDir(), t.TempDir()
	runs := 0
	r.h.check.Exec = func(_ context.Context, _, cmd string, _ []string, _ io.Writer) (int, error) {
		runs++
		return map[string]int{"bad": 1}[cmd], nil
	}
	wave := []Spec{{ID: "1.3"}, {ID: "1.1", Verify: "good"}, {ID: "1.2"}, {ID: "1.4"}, {ID: "1.5"},
		{ID: "1.6", Verify: "bad"}, {ID: "1.7"}}
	waits := map[string]wait{"1.3": {after: []string{"1.1"}}, "1.5": {after: []string{"1.4"}},
		"1.7": {after: []string{"1.6"}}}
	out, err := r.launchRounds(context.Background(), wave, wave, waits, func([]Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || !slices.Equal(rounds[0], []string{"1.1", "1.2", "1.4", "1.6"}) || !slices.Equal(rounds[1], []string{"1.3"}) {
		t.Fatalf("rounds = %v", rounds)
	}
	for id, want := range map[string]string{"p-1.5": "blocker 1.4 failed", "p-1.7": "blocker 1.6 failed its verify"} {
		i := slices.IndexFunc(out, func(e Envelope) bool { return e.TaskID == id })
		if i < 0 || out[i].Status != LeafStateSkipped || out[i].Blocked != want {
			t.Fatalf("%s not skipped as %q: %+v", id, want, out)
		}
	}
	before := runs
	_, _, _ = r.checkOnce(context.Background(), wave[1])
	if runs != before {
		t.Errorf("verifyLeaf reran 1.1's release verify: runs %d->%d", before, runs)
	}
}
