package waves

import (
	"context"
	"slices"
	"testing"
)

func TestLaunchRoundsOrdersByBlocker(t *testing.T) {
	var rounds [][]string
	r := runner{slug: "p", h: runHooks{launch: func(_ context.Context, _ string, specs []Spec, _ LaunchOptions) ([]Envelope, error) {
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
		rounds = append(rounds, ids)
		return out, nil
	}}}
	r.o.Executable, r.reports = "cc", t.TempDir()
	wave := []Spec{{ID: "1.3"}, {ID: "1.1"}, {ID: "1.2"}, {ID: "1.4"}, {ID: "1.5"}}
	waits := map[string]wait{"1.3": {after: []string{"1.1"}}, "1.5": {after: []string{"1.4"}}}
	out, err := r.launchRounds(context.Background(), wave, wave, waits, func([]Envelope) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 2 || !slices.Equal(rounds[0], []string{"1.1", "1.2", "1.4"}) || !slices.Equal(rounds[1], []string{"1.3"}) {
		t.Fatalf("rounds = %v", rounds)
	}
	i := slices.IndexFunc(out, func(e Envelope) bool { return e.TaskID == "p-1.5" })
	if i < 0 || out[i].Status != LeafStateSkipped || out[i].Blocked != "blocker 1.4 failed" {
		t.Fatalf("1.5 not skipped on its failed blocker: %+v", out)
	}
}
