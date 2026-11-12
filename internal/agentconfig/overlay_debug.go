// overlay_debug.go — COWORKER_OVERLAY_DEBUG: a 0600 JSONL log of each child's env names with salted fingerprints.
package agentconfig

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// overlayDebugVar names the 0600 JSONL file RecordOverlayDebug appends to; unset (or no _SALT) records nothing.
const overlayDebugVar = "COWORKER_OVERLAY_DEBUG"

const overlayDebugSaltVar = overlayDebugVar + "_SALT"

// OverlayDebugOn reports whether RecordOverlayDebug writes: COWORKER_OVERLAY_DEBUG and its _SALT are both set.
func OverlayDebugOn() bool {
	path, salt := overlayDebugEnv()
	return path != "" && salt != ""
}

func overlayDebugEnv() (path, salt string) {
	return os.Getenv(overlayDebugVar), os.Getenv(overlayDebugSaltVar)
}

type overlayDebugRecord struct {
	Label string            `json:"label"`
	Agent string            `json:"agent"`
	PID   int               `json:"pid"`
	Names []string          `json:"names"`
	FP    map[string]string `json:"fp"`
}

// EnableOverlayDebug turns recording on for this process and its children; the salt sits beside the log (.salt).
func EnableOverlayDebug(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if os.Getenv(overlayDebugSaltVar) == "" {
		salt, err := overlayDebugSalt(abs + ".salt")
		if err != nil {
			return err
		}
		if err := os.Setenv(overlayDebugSaltVar, salt); err != nil {
			return err
		}
	}
	return os.Setenv(overlayDebugVar, abs)
}

// overlayDebugSalt reads the salt beside the log, or links a full one into place (a racing loser reads the winner's).
func overlayDebugSalt(path string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	salt := hex.EncodeToString(b)
	switch err := linkSalt(path, salt); {
	case err == nil:
		return salt, nil
	case !errors.Is(err, os.ErrExist):
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) == 0 {
		return "", errors.Join(err, errors.New("empty salt file "+path))
	}
	return strings.TrimSpace(string(got)), nil
}

// linkSalt writes salt to a temp file and links it to path: atomic, and ErrExist if path exists (never half-written).
func linkSalt(path, salt string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // the temp name only; path keeps the hard link
	_, werr := tmp.WriteString(salt)
	if err := errors.Join(werr, tmp.Chmod(0o600), tmp.Close()); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

// RecordOverlayDebug appends env's names and HMAC-SHA256 fingerprints (last duplicate wins, as exec does).
func RecordOverlayDebug(agentName string, env []string) {
	path, salt := overlayDebugEnv()
	if path == "" || salt == "" {
		return
	}
	rec := overlayDebugRecord{Label: os.Getenv(overlayDebugVar + "_LABEL"), Agent: agentName, PID: os.Getpid(), FP: map[string]string{}}
	m := hmac.New(sha256.New, []byte(salt)) // one MAC reused across names
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		m.Reset()
		m.Write([]byte(val))
		if _, seen := rec.FP[name]; !seen {
			rec.Names = append(rec.Names, name)
		}
		rec.FP[name] = hex.EncodeToString(m.Sum(nil))[:16]
	}
	slices.Sort(rec.Names)
	line, err := json.Marshal(rec)
	if err == nil {
		err = appendOverlayDebug(path, append(line, '\n'))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-code: overlay debug:", err)
	}
}

func appendOverlayDebug(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // a pre-existing file keeps its mode on open
		return errors.Join(err, f.Close())
	}
	_, werr := f.Write(line)
	return errors.Join(werr, f.Close())
}
