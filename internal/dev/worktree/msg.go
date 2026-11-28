// Messages: named threads between Claude sessions and external agents; one append-only log and a cursor per reader.

package worktree

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/schemas"
)

const (
	msgSchema    = 1
	msgTextMax   = 4000    // bytes of one post's text
	msgFileMax   = 8 << 20 // bytes of one attachment
	msgNoticeCut = 200     // runes of a post a live session's notice carries
	msgKind      = "msg"
)

// ErrMsgInput marks a refused chat name, empty or oversize text or unreadable attachment: a usage error.
var ErrMsgInput = errors.New("chat input")

var chatNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// MsgFile is one attachment: where it came from and the copy kept under the chat.
type MsgFile struct {
	Path string `json:"path"`
	Copy string `json:"copy"`
}

// Message is one post in a chat's log.
type Message struct {
	Schema int       `json:"schema"`
	Seq    int64     `json:"seq"`
	TS     int64     `json:"ts"`
	From   string    `json:"from"`
	Text   string    `json:"text"`
	Files  []MsgFile `json:"files,omitempty"`
}

var _ = schemas.Register(schemas.Format{
	Name: "msg-log", Owner: "internal/dev/worktree", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(msgsDir(), "*", "log.jsonl") },
	New:     func() any { return &Message{} },
	Version: msgSchema, Keep: "chat threads and their attachments stay until the chat directory is removed",
})

func msgsDir() string { return filepath.Join(paths.StateDir(), "msgs") }

func chatLog(chat string) string { return filepath.Join(msgsDir(), chat, "log.jsonl") }

func chatCursor(chat, member string) string {
	return filepath.Join(msgsDir(), chat, "members", member+".seq")
}

func checkName(kind, s string) error {
	if !chatNameRe.MatchString(s) {
		return fmt.Errorf("%w: %s %q must match %s", ErrMsgInput, kind, s, chatNameRe)
	}
	return nil
}

// checkChat accepts a chat id: @name (one member's inbox) or #name (a named chat); the two never collide.
func checkChat(chat string) error {
	if chat == "" || !strings.ContainsRune("@#", rune(chat[0])) {
		return fmt.Errorf("%w: chat %q starts with @ (a member) or # (a chat)", ErrMsgInput, chat)
	}
	return checkName("chat", chat[1:])
}

// PostMsg stores a post and its files, then notifies live Claude members; a notify error still returns the Message.
func PostMsg(chat, from, text string, files []string) (Message, error) {
	if err := errors.Join(checkChat(chat), checkName("sender", from)); err != nil {
		return Message{}, err
	}
	if len(text) > msgTextMax {
		return Message{}, fmt.Errorf("%w: text is %d bytes (cap %d): send it as a --file", ErrMsgInput, len(text), msgTextMax)
	}
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return Message{}, fmt.Errorf("%w: nothing to post", ErrMsgInput)
	}
	atts, err := readAttachments(files)
	if err != nil {
		return Message{}, err
	}
	var msg Message
	err = paths.WithFileLock(chatLog(chat), func() error {
		var perr error
		msg, perr = appendPost(chat, from, text, atts)
		return perr
	})
	if err != nil {
		return Message{}, err
	}
	owner := from
	if chat[0] == '@' { // @name is name's inbox: its owner is a member from the first message
		owner = chat[1:]
	}
	if err := errors.Join(joinChat(chat, from), joinChat(chat, owner)); err != nil {
		return msg, err
	}
	return msg, notifyChat(chat, msg)
}

type attachment struct {
	orig, base string
	data       []byte
}

func readAttachments(files []string) ([]attachment, error) {
	out := make([]attachment, 0, len(files))
	for _, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		fh, err := os.Open(abs)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrMsgInput, err)
		}
		data, err := io.ReadAll(io.LimitReader(fh, msgFileMax+1))
		err = errors.Join(err, fh.Close())
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: read %s: %v", ErrMsgInput, abs, err)
		case len(data) > msgFileMax:
			return nil, fmt.Errorf("%w: %s is over %d bytes", ErrMsgInput, abs, msgFileMax)
		}
		out = append(out, attachment{orig: abs, base: paths.Slug(filepath.Base(abs), "._", 80, "file"), data: data})
	}
	return out, nil
}

// appendPost stores atts and the post under the log lock the caller holds.
func appendPost(chat, from, text string, atts []attachment) (Message, error) {
	prior, err := readChatLog(chatLog(chat))
	if err != nil {
		return Message{}, err
	}
	msg := Message{Schema: msgSchema, Seq: lastSeq(prior) + 1, TS: time.Now().Unix(), From: from, Text: text}
	for _, a := range atts {
		dest := filepath.Join(msgsDir(), chat, "files", strconv.FormatInt(msg.Seq, 10)+"-"+a.base)
		if err := paths.AtomicWrite(dest, a.data, 0o600); err != nil {
			return Message{}, err
		}
		msg.Files = append(msg.Files, MsgFile{Path: a.orig, Copy: dest})
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return Message{}, err
	}
	f, err := os.OpenFile(chatLog(chat), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return Message{}, err
	}
	_, err = f.Write(append(line, '\n'))
	return msg, errors.Join(err, f.Close())
}

func lastSeq(log []Message) int64 {
	if len(log) == 0 {
		return 0
	}
	return log[len(log)-1].Seq
}

func readChatLog(file string) ([]Message, error) {
	raw, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Message
	for i, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", file, i+1, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// joinChat makes name a member (cursor 0) unless it already is.
func joinChat(chat, name string) error {
	cur := chatCursor(chat, name)
	if err := os.MkdirAll(filepath.Dir(cur), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(cur, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.WriteString("0")
	return errors.Join(err, f.Close())
}

// ReadMsgs returns the posts in chat newer than reader's cursor that reader did not send, then advances the cursor.
func ReadMsgs(chat, reader string) ([]Message, error) {
	if err := errors.Join(checkChat(chat), checkName("reader", reader)); err != nil {
		return nil, err
	}
	if _, err := os.Stat(chatLog(chat)); errors.Is(err, fs.ErrNotExist) {
		return nil, nil // reading a chat nobody wrote to creates nothing, not even its lock
	}
	var out []Message
	err := paths.WithFileLock(chatLog(chat), func() error {
		log, err := readChatLog(chatLog(chat))
		if err != nil {
			return err
		}
		cur := chatCursor(chat, reader)
		seen := readInt(cur)
		for _, m := range log {
			if m.Seq > seen && m.From != reader {
				out = append(out, m)
			}
		}
		if _, serr := os.Stat(cur); serr == nil && lastSeq(log) <= seen {
			return nil
		}
		return paths.AtomicWrite(cur, []byte(strconv.FormatInt(max(seen, lastSeq(log)), 10)), 0o600)
	})
	return out, err
}

// ChatsFor lists the chats reader is a member of, sorted.
func ChatsFor(reader string) ([]string, error) {
	if err := checkName("reader", reader); err != nil {
		return nil, err
	}
	hits, err := filepath.Glob(chatCursor("*", reader))
	if err != nil {
		return nil, err
	}
	chats := make([]string, 0, len(hits))
	for _, h := range hits {
		chats = append(chats, filepath.Base(filepath.Dir(filepath.Dir(h))))
	}
	return chats, nil
}

// notifyChat copies msg into the inbox of every live Claude member but the sender; other members pull.
func notifyChat(chat string, msg Message) error {
	hits, err := filepath.Glob(chatCursor(chat, "*"))
	if err != nil {
		return err
	}
	live := liveSessionIDs()
	var errs []error
	for _, h := range hits {
		name := strings.TrimSuffix(filepath.Base(h), ".seq")
		if id, ok := live[name]; ok && name != msg.From {
			errs = append(errs, Append(id, Notice{TS: time.Now().Unix(), From: msg.From + " in " + chat, Kind: msgKind,
				Text: msgNotice(chat, msg)}))
		}
	}
	return errors.Join(errs...)
}

func msgNotice(chat string, msg Message) string {
	runes := []rune(strings.Join(strings.Fields(msg.Text), " "))
	text := string(runes[:min(len(runes), msgNoticeCut)])
	if len(runes) > msgNoticeCut || len(msg.Files) > 0 {
		text += " read: claude-code agents msg '" + chat + "'"
	}
	return text
}

type sessionReg struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	ProcStart string `json:"procStart"`
}

// memberName is a registry entry's chat member name: its session name, else the first 8 characters of its id.
func (s sessionReg) memberName() string {
	name := s.Name
	if name == "" && len(s.SessionID) >= 8 {
		name = s.SessionID[:8]
	}
	return paths.Slug(name, "", 64, "")
}

// readSessionRegistry reads <config>/sessions/<pid>.json, the same files SessionLabel and sessionLive read.
func readSessionRegistry() []sessionReg {
	files, _ := filepath.Glob(filepath.Join(paths.ConfigDir(), "sessions", "*.json"))
	var out []sessionReg
	for _, f := range files {
		var e sessionReg
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &e) == nil && e.SessionID != "" && e.PID > 0 {
			out = append(out, e)
		}
	}
	return out
}

// liveSessionIDs maps member name → session id for every live registered Claude session.
func liveSessionIDs() map[string]string {
	out := map[string]string{}
	for _, e := range readSessionRegistry() {
		if name := e.memberName(); name != "" && proc.Alive(e.PID) && !startChanged(e.PID, e.ProcStart) {
			out[name] = e.SessionID
		}
	}
	return out
}

// CallerName is the chat member name of the calling Claude session, or "" outside one.
func CallerName() string {
	id := os.Getenv(paths.SessionIDEnv)
	for _, e := range readSessionRegistry() {
		if id != "" && e.SessionID == id {
			return e.memberName()
		}
	}
	return ""
}
