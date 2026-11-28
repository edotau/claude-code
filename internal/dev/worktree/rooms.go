// Rooms: named threads between Claude sessions and external agents; one append-only log and a cursor per reader.

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
	roomSchema    = 1
	roomTextMax   = 4000    // bytes of one post's text
	roomFileMax   = 8 << 20 // bytes of one attachment
	roomNoticeCut = 200     // runes of a post a live session's notice carries
	roomKind      = "room"
)

// ErrRoomInput marks a refused room name, empty or oversize text or unreadable attachment: a usage error.
var ErrRoomInput = errors.New("room input")

var roomNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// RoomFile is one attachment: where it came from and the copy kept under the room.
type RoomFile struct {
	Path string `json:"path"`
	Copy string `json:"copy"`
}

// Message is one post in a room's log.
type Message struct {
	Schema int        `json:"schema"`
	Seq    int64      `json:"seq"`
	TS     int64      `json:"ts"`
	From   string     `json:"from"`
	Text   string     `json:"text"`
	Files  []RoomFile `json:"files,omitempty"`
}

var _ = schemas.Register(schemas.Format{
	Name: "room-log", Owner: "internal/dev/worktree", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(roomsDir(), "*", "log.jsonl") },
	New:     func() any { return &Message{} },
	Version: roomSchema, Keep: "room threads and their attachments stay until the room directory is removed",
})

func roomsDir() string { return filepath.Join(paths.StateDir(), "rooms") }

func roomLog(room string) string { return filepath.Join(roomsDir(), room, "log.jsonl") }

func roomCursor(room, member string) string {
	return filepath.Join(roomsDir(), room, "members", member+".seq")
}

func checkRoomName(kind, s string) error {
	if !roomNameRe.MatchString(s) {
		return fmt.Errorf("%w: %s %q must match %s", ErrRoomInput, kind, s, roomNameRe)
	}
	return nil
}

// PostRoom stores a post and its files, then notifies live Claude members; a notify error still returns the Message.
func PostRoom(room, from, text string, files []string) (Message, error) {
	if err := errors.Join(checkRoomName("room", room), checkRoomName("sender", from)); err != nil {
		return Message{}, err
	}
	if len(text) > roomTextMax {
		return Message{}, fmt.Errorf("%w: text is %d bytes (cap %d): send it as a --file", ErrRoomInput, len(text), roomTextMax)
	}
	if strings.TrimSpace(text) == "" && len(files) == 0 {
		return Message{}, fmt.Errorf("%w: nothing to post", ErrRoomInput)
	}
	atts, err := readAttachments(files)
	if err != nil {
		return Message{}, err
	}
	var msg Message
	err = paths.WithFileLock(roomLog(room), func() error {
		var perr error
		msg, perr = appendPost(room, from, text, atts)
		return perr
	})
	if err != nil {
		return Message{}, err
	}
	if err := errors.Join(joinRoom(room, from), joinRoom(room, room)); err != nil { // a room named after a member is its inbox
		return msg, err
	}
	return msg, notifyRoom(room, msg)
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
			return nil, fmt.Errorf("%w: %v", ErrRoomInput, err)
		}
		data, err := io.ReadAll(io.LimitReader(fh, roomFileMax+1))
		err = errors.Join(err, fh.Close())
		switch {
		case err != nil:
			return nil, fmt.Errorf("%w: read %s: %v", ErrRoomInput, abs, err)
		case len(data) > roomFileMax:
			return nil, fmt.Errorf("%w: %s is over %d bytes", ErrRoomInput, abs, roomFileMax)
		}
		out = append(out, attachment{orig: abs, base: paths.Slug(filepath.Base(abs), "._", 80, "file"), data: data})
	}
	return out, nil
}

// appendPost stores atts and the post under the log lock the caller holds.
func appendPost(room, from, text string, atts []attachment) (Message, error) {
	prior, err := readRoomLog(roomLog(room))
	if err != nil {
		return Message{}, err
	}
	msg := Message{Schema: roomSchema, Seq: lastSeq(prior) + 1, TS: time.Now().Unix(), From: from, Text: text}
	for _, a := range atts {
		dest := filepath.Join(roomsDir(), room, "files", strconv.FormatInt(msg.Seq, 10)+"-"+a.base)
		if err := paths.AtomicWrite(dest, a.data, 0o600); err != nil {
			return Message{}, err
		}
		msg.Files = append(msg.Files, RoomFile{Path: a.orig, Copy: dest})
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return Message{}, err
	}
	f, err := os.OpenFile(roomLog(room), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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

func readRoomLog(file string) ([]Message, error) {
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

// joinRoom makes name a member (cursor 0) unless it already is.
func joinRoom(room, name string) error {
	cur := roomCursor(room, name)
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

// ReadRoom returns the posts in room newer than reader's cursor that reader did not send, then advances the cursor.
func ReadRoom(room, reader string) ([]Message, error) {
	if err := errors.Join(checkRoomName("room", room), checkRoomName("reader", reader)); err != nil {
		return nil, err
	}
	var out []Message
	err := paths.WithFileLock(roomLog(room), func() error {
		log, err := readRoomLog(roomLog(room))
		if err != nil {
			return err
		}
		cur := roomCursor(room, reader)
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

// RoomsFor lists the rooms reader is a member of, sorted.
func RoomsFor(reader string) ([]string, error) {
	if err := checkRoomName("reader", reader); err != nil {
		return nil, err
	}
	hits, err := filepath.Glob(roomCursor("*", reader))
	if err != nil {
		return nil, err
	}
	rooms := make([]string, 0, len(hits))
	for _, h := range hits {
		rooms = append(rooms, filepath.Base(filepath.Dir(filepath.Dir(h))))
	}
	return rooms, nil
}

// notifyRoom copies msg into the inbox of every live Claude member but the sender; other members pull.
func notifyRoom(room string, msg Message) error {
	hits, err := filepath.Glob(roomCursor(room, "*"))
	if err != nil {
		return err
	}
	live := liveSessionIDs()
	var errs []error
	for _, h := range hits {
		name := strings.TrimSuffix(filepath.Base(h), ".seq")
		if id, ok := live[name]; ok && name != msg.From {
			errs = append(errs, Append(id, Notice{TS: time.Now().Unix(), From: "room " + room, Kind: roomKind,
				Text: roomNotice(room, msg)}))
		}
	}
	return errors.Join(errs...)
}

func roomNotice(room string, msg Message) string {
	runes := []rune(strings.Join(strings.Fields(msg.Text), " "))
	text := fmt.Sprintf("msg @%s · %s: %s", room, msg.From, string(runes[:min(len(runes), roomNoticeCut)]))
	if len(runes) > roomNoticeCut || len(msg.Files) > 0 {
		text += " read: claude-code agents msg @" + room
	}
	return text
}

type sessionReg struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	ProcStart string `json:"procStart"`
}

// memberName is a registry entry's room member name: its session name, else the first 8 characters of its id.
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

// CallerName is the room member name of the calling Claude session, or "" outside one.
func CallerName() string {
	id := os.Getenv(paths.SessionIDEnv)
	for _, e := range readSessionRegistry() {
		if id != "" && e.SessionID == id {
			return e.memberName()
		}
	}
	return ""
}
