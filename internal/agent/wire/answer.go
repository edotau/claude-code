package wire

import "strings"

// Answer collects the text a stream parser decoded; each chunk is recorded before it is emitted, so a failed emit keeps it.
type Answer struct {
	Join   string // between chunks in String: "" for deltas, a newline run for whole messages
	chunks []string
}

// Emit records text, then emits it as a text event; turns marks every chunk after the first as a new turn (Sep).
func (a *Answer) Emit(sink Sink, text string, turns bool) error {
	a.chunks = append(a.chunks, text)
	return sink.Emit(Event{Type: EventText, Text: text, Sep: turns && len(a.chunks) > 1})
}

// Empty is true until a chunk has been emitted.
func (a *Answer) Empty() bool { return len(a.chunks) == 0 }

// String is the chunks joined by Join and trimmed.
func (a *Answer) String() string { return strings.TrimSpace(strings.Join(a.chunks, a.Join)) }

// Last is the final chunk, trimmed: the answer of an agent whose last message is the reply.
func (a *Answer) Last() string {
	if a.Empty() {
		return ""
	}
	return strings.TrimSpace(a.chunks[len(a.chunks)-1])
}
