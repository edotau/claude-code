package rtkwrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// SessionFlagSource names the session flag in CodeModeSourceFor.
const SessionFlagSource = "session flag"

// SessionFlag is the flag file of one session: "on" or "off".
func SessionFlag(session string) (string, error) {
	if session == "" || session == "." || session == ".." || strings.ContainsAny(session, `/\`+"\x00") {
		return "", errors.New("invalid session id " + session)
	}
	return filepath.Join(paths.StateDir(), "rtkwrap", "sessions", session), nil
}

// SetSessionFlag records the session's code mode as on or off; an explicit off beats CODEMODE and the vendor variable.
func SetSessionFlag(session string, on bool) error {
	file, err := SessionFlag(session)
	if err != nil {
		return err
	}
	word := "off\n"
	if on {
		word = "on\n"
	}
	return paths.AtomicWrite(file, []byte(word), 0o600)
}

// sessionState is the session's recorded switch; set is false with no flag file.
func sessionState(session string) (on, set bool) {
	file, err := SessionFlag(session)
	if err != nil {
		return false, false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return false, false
	}
	return strings.TrimSpace(string(b)) == "on", true
}

// CodeModeFor is CodeMode plus that session's flag (hook path: the payload's session id); one file read.
func CodeModeFor(session string) bool { return CodeModeSourceFor(session) != "" }

// CodeModeSourceFor is CodeModeSource, led by the session flag; "" is off.
func CodeModeSourceFor(session string) string {
	if on, set := sessionState(session); set {
		if on {
			return SessionFlagSource
		}
		return ""
	}
	return CodeModeSource()
}

// CodeModeStatus is the `agents codemode status` line for a valid session id.
func CodeModeStatus(session string) (string, error) {
	if _, err := SessionFlag(session); err != nil {
		return "", err
	}
	state, src := "on", ""
	if _, set := sessionState(session); set {
		src = SessionFlagSource
		if CodeModeSourceFor(session) == "" {
			state = "off"
		}
	} else if v := CodeModeSource(); v == "" {
		state, src = "off", "default"
	} else if src = "CODEMODE"; v != CodemodeEnv {
		src = "vendor var"
	}
	return "codemode: " + state + " (" + src + ")", nil
}
