// Package notices is the file channel between sessions: per-session inboxes drained into hook context.
package notices

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Notice is one inbox line: when, who sent it, its kind (conflict, diverged) and the text the reader sees.
type Notice struct {
	Schema int    `json:"schema"`
	TS     int64  `json:"ts"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Text   string `json:"text"`
}

// noticeSchema is the inbox format's current version.
const noticeSchema = 1

// drainMax caps how many notices one drain delivers; older overflow is dropped.
const drainMax = 5

var _ = schemas.Register(schemas.Format{
	Name: "notices", Owner: "internal/notices", Kind: schemas.Log,
	Glob:    func() string { return filepath.Join(Dir(), "*.jsonl") },
	New:     func() any { return &Notice{} },
	Version: noticeSchema, Keep: "per-session notice inboxes; Drain removes what it delivers",
})

// Dir holds one inbox per session.
func Dir() string { return filepath.Join(paths.StateDir(), "notices") }

// inbox is session's inbox file.
func inbox(session string) string {
	return filepath.Join(Dir(), paths.Slug(session, "", 80, "session")+".jsonl")
}

// Append adds n to session's inbox under its file lock.
func Append(session string, n Notice) error {
	if session == "" {
		return errors.New("notices: no session")
	}
	n.Schema = noticeSchema
	line, err := json.Marshal(n)
	if err != nil {
		return err
	}
	file := inbox(session)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	return paths.WithFileLock(file, func() error {
		f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(line, '\n'))
		return errors.Join(err, f.Close())
	})
}

// Drain returns and removes session's notices (at most drainMax, newest last); errors yield none.
func Drain(session string) []Notice {
	if session == "" {
		return nil
	}
	file := inbox(session)
	if _, err := os.Stat(file); err != nil {
		return nil // the common case: no inbox, no lock taken
	}
	var raw []byte
	err := paths.WithFileLock(file, func() error {
		var err error
		if raw, err = os.ReadFile(file); err != nil {
			return err
		}
		return os.Remove(file)
	})
	if err != nil {
		return nil
	}
	var out []Notice
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var n Notice
		if json.Unmarshal(sc.Bytes(), &n) == nil && n.Text != "" {
			out = append(out, n)
		}
	}
	return out[max(0, len(out)-drainMax):]
}
