package worktree

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func msgPost(t *testing.T, from, text string, files ...string) {
	t.Helper()
	if _, err := PostMsg("#duo", from, text, files); err != nil {
		t.Fatal(err)
	}
}

// msgRead returns the comma-joined texts reader sees and the attachment copies among them.
func msgRead(t *testing.T, reader string) (texts string, copies []string) {
	t.Helper()
	got, err := ReadMsgs("#duo", reader)
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

func TestMsgReadersSeeOnlyNewForeignPosts(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	attach := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(attach, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgPost(t, "alpha", "one", attach)
	msgPost(t, "beta", "two")
	msgPost(t, "alpha", "three")
	got, copies := msgRead(t, "beta")
	_, statErr := os.Stat(strings.Join(copies, ""))
	again, _ := msgRead(t, "beta")
	mine, _ := msgRead(t, "alpha")
	chats, roomsErr := ChatsFor("alpha")
	_, badErr := PostMsg("#../x", "alpha", "hi", nil)
	for _, c := range []struct{ name, got, want string }{
		{"beta first read", got, "one,three"},
		{"beta second read", again, ""},
		{"alpha read", mine, "two"},
		{"alpha chats", strings.Join(chats, ","), "#duo"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if statErr != nil || roomsErr != nil || !errors.Is(badErr, ErrMsgInput) {
		t.Fatalf("copy stat %v, chats %v, bad name %v", statErr, roomsErr, badErr)
	}
}
