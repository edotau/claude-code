package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func roomPost(t *testing.T, from, text string, files ...string) {
	t.Helper()
	if _, err := PostRoom("#duo", from, text, files); err != nil {
		t.Fatal(err)
	}
}

// roomRead returns the comma-joined texts reader sees and the attachment copies among them.
func roomRead(t *testing.T, reader string) (texts string, copies []string) {
	t.Helper()
	got, err := ReadRoom("#duo", reader)
	if err != nil {
		t.Fatal(err)
	}
	var ts []string
	for _, m := range got {
		ts = append(ts, m.Text)
		for _, f := range m.Files {
			copies = append(copies, f.Copy)
		}
	}
	return strings.Join(ts, ","), copies
}

func TestRoomReadersSeeOnlyNewForeignPosts(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	attach := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(attach, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	roomPost(t, "alpha", "one", attach)
	roomPost(t, "beta", "two")
	roomPost(t, "alpha", "three")
	got, copies := roomRead(t, "beta")
	_, statErr := os.Stat(strings.Join(copies, ""))
	again, _ := roomRead(t, "beta")
	mine, _ := roomRead(t, "alpha")
	rooms, roomsErr := RoomsFor("alpha")
	_, badErr := PostRoom("#../x", "alpha", "hi", nil)
	for _, c := range []struct{ name, got, want string }{
		{"beta first read", got, "one,three"},
		{"beta second read", again, ""},
		{"alpha read", mine, "two"},
		{"alpha rooms", strings.Join(rooms, ","), "#duo"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if statErr != nil || roomsErr != nil || !errors.Is(badErr, ErrRoomInput) {
		t.Fatalf("copy stat %v, rooms %v, bad name %v", statErr, roomsErr, badErr)
	}
}
