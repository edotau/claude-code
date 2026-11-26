package worktree

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// A held gate gets a warm slot; with every slot busy it gives up after the wait and gates in the lane.
func TestHoldGate(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	top, sha := auditFixtureRepo(t)
	ctx := context.Background()
	oldWait, oldPoll := gateSlotWait, gateSlotPoll
	t.Cleanup(func() { gateSlotWait, gateSlotPoll = oldWait, oldPoll })
	gateSlotWait, gateSlotPoll = 0, time.Millisecond
	var held []*HeldGate
	for range slotCount {
		h := HoldGate(ctx, top, sha, "lane", &bytes.Buffer{})
		if h == nil || h.Where("lane-dir") == "lane-dir" {
			t.Fatalf("slot %d not held: %v", len(held), h)
		}
		held = append(held, h)
	}
	if h := HoldGate(ctx, top, sha, "lane", &bytes.Buffer{}); h != nil || h.Where("lane-dir") != "lane-dir" {
		t.Fatalf("all slots busy: got %v, want nil (lane)", h)
	}
	if err := held[0].Reset(ctx, sha); err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		h.Release()
	}
	if !usesGateSlot(fullGoGate, "go.mod") || usesGateSlot("make test", "Makefile") {
		t.Error("usesGateSlot picks the wrong gates")
	}
}

// A scoped gate is [build] then [vet test]; the full go gate too, any other gate one stage.
func TestGateStages(t *testing.T) {
	stages := gateStages(scopedGateCommand([]string{"a"}), scopedGateSource+", x")
	if len(stages) != 2 || len(stages[0]) != 1 || len(stages[1]) != 2 {
		t.Fatalf("stages = %v, want [build] then [vet test]", stages)
	}
	if got := gateStages(fullGoGate, "go.mod"); len(got) != 2 || gateStages("make test", "Makefile")[0][0] != "make test" {
		t.Fatalf("full gate stages = %v", got)
	}
}

// Stages run in order with per-step seconds recorded; a failed stage is named and later stages never run.
func TestRunStages(t *testing.T) {
	var out, errOut bytes.Buffer
	at := gateAt{Dir: t.TempDir()}
	secs, err := runStages(context.Background(), at, [][]string{{"echo one"}, {"printf two", "true"}}, &out, &errOut)
	if err != nil || len(secs) != 3 || strings.Index(out.String(), "one") > strings.Index(out.String(), "two") {
		t.Fatalf("err=%v secs=%v out=%q", err, secs, out.String())
	}
	out.Reset()
	_, err = runStages(context.Background(), at, [][]string{{"echo b; exit 2"}, {"echo test-ran"}}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "echo stage") || strings.Contains(out.String(), "test-ran") {
		t.Fatalf("err=%v out=%q, want the first stage named and no later stage run", err, out.String())
	}
}
