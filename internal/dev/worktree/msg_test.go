package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/platform/paths"
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

func TestPostMsgKeepsSenderOutOfInbox(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := PostMsg("@owner", "sender", "one", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(chatCursor("@owner", "sender")); !os.IsNotExist(err) {
		t.Fatalf("sender joined inbox: %v", err)
	}
	if err := joinChat("@owner", "sender"); err != nil {
		t.Fatal(err)
	} // pre-migration store
	if _, err := PostMsg("#room", "sender", "room", nil); err != nil {
		t.Fatal(err)
	}
	chats, err := ChatsFor("sender")
	if err != nil || !slices.Equal(chats, []string{"#room"}) {
		t.Fatalf("sender chats %v, %v", chats, err)
	}
	if raw, err := os.ReadFile(chatCursor("@owner", "sender")); err != nil || string(raw) != "0" {
		t.Fatalf("legacy cursor changed: %q, %v", raw, err)
	}
	chats, err = ChatsFor("owner")
	if err != nil || !slices.Contains(chats, "@owner") {
		t.Fatalf("owner chats %v, %v", chats, err)
	}
	msgs, err := ReadMsgs("@owner", "owner")
	if err != nil || len(msgs) != 1 || msgs[0].Text != "one" {
		t.Fatalf("owner lost message: %v, %v", msgs, err)
	}
	if _, err := PostMsg("@sender", "owner", "reply", nil); err != nil {
		t.Fatal(err)
	}
	msgs, err = ReadMsgs("@sender", "sender")
	if err != nil || len(msgs) != 1 || msgs[0].Text != "reply" {
		t.Fatalf("sender lost reply: %v, %v", msgs, err)
	}
}

func TestSentItemsReadReceiptTransition(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if _, err := PostMsg("@idle", "sender", "ping", nil); err != nil {
		t.Fatal(err)
	}
	before, _ := CollectInbox([]string{"@idle"}, "sender")
	_, _ = CollectInbox([]string{"@idle"}, "idle")
	after, _ := CollectInbox([]string{"@idle"}, "sender")
	if got := InboxText(before) + InboxText(after); got != "@idle 1 from sender (unread): ping\n@idle 1 from sender (read): ping\n" {
		t.Fatalf("read receipt transition = %q", got)
	}
}

func TestCallerNameWithinSkipsOuterSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(paths.SessionIDEnv, "child-session")
	raw, _ := json.Marshal(sessionReg{PID: os.Getpid(), SessionID: "child-session", Name: "child-name"})
	path := filepath.Join(paths.ConfigDir(), "sessions", "fixture.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if got := CallerNameWithin(os.Getppid()); got != "child-name" {
		t.Fatalf("child name = %q", got)
	}
	if got := CallerNameWithin(os.Getpid()); got != "" {
		t.Fatalf("inherited outer session = %q", got)
	}
}
