package wire

import (
	"errors"
	"testing"
)

type failSink struct{}

func (failSink) Emit(Event) error { return errors.New("consumer gone") }

// Given a sink that fails every emit, Then the chunk is still in the answer: it was recorded before the emit.
func TestAnswerKeepsChunkWhenEmitFails(t *testing.T) {
	a := Answer{Join: "\n\n"}
	if err := a.Emit(failSink{}, " one ", true); err == nil {
		t.Fatal("want the sink's error")
	}
	if got := a.String(); got != "one" || a.Empty() {
		t.Fatalf("answer = %q, empty = %v; want the failed chunk kept", got, a.Empty())
	}
}

// Given three chunks, Then Sep marks every chunk after the first only when turns is set, String joins them and Last is the final one.
func TestAnswerTurnsJoinAndLast(t *testing.T) {
	rec := &recSink{}
	turns, deltas := Answer{Join: "\n"}, Answer{}
	for _, c := range []string{"a", "b", "c "} {
		_ = turns.Emit(rec, c, true)
		_ = deltas.Emit(Discard, c, false)
	}
	if rec.got[0].Sep || !rec.got[1].Sep || !rec.got[2].Sep {
		t.Errorf("Sep flags = %v %v %v; want false true true", rec.got[0].Sep, rec.got[1].Sep, rec.got[2].Sep)
	}
	if turns.String() != "a\nb\nc" || deltas.String() != "abc" || turns.Last() != "c" {
		t.Errorf("turns %q deltas %q last %q", turns.String(), deltas.String(), turns.Last())
	}
	if (&Answer{}).Last() != "" {
		t.Error("empty answer's Last must be empty")
	}
}

type recSink struct{ got []Event }

func (s *recSink) Emit(ev Event) error { s.got = append(s.got, ev); return nil }
