package helptext

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/edotau/claude-code/internal/textutil"
)

// Exit codes: 0 ok, 1 runtime, 2 usage, 3 auth.
const (
	ExitRuntime = 1
	ExitUsage   = 2
	ExitAuth    = 3
)

// Error is a CLI failure carrying its exit code, kind and hints.
type Error struct {
	Code  int
	Kind  string
	Msg   string
	Hints []string
}

func (e *Error) Error() string { return e.Msg }

// authError is implemented by errors that mean a rejected credential (retry.Error).
type authError interface{ IsAuth() bool }

var jsonErrors atomic.Bool

// SetJSONErrors switches Fail output to a stderr JSON object; it returns the old setting.
func SetJSONErrors(on bool) bool { return jsonErrors.Swap(on) }

// WantsJSON reports --format json (or --format=json) before any "--".
func WantsJSON(args []string) bool {
	for i, a := range args {
		switch a {
		case "--":
			return false
		case "--format", "-format":
			return i+1 < len(args) && args[i+1] == JSON
		case "--format=json", "-format=json":
			return true
		}
	}
	return false
}

// NewError builds the Error for exit code code; any auth error among args turns a runtime failure into code 3.
func NewError(code int, format string, a ...any) *Error {
	e := &Error{Code: code, Msg: fmt.Sprintf(format, a...), Hints: []string{}}
	for _, v := range a {
		var ae authError
		if err, ok := v.(error); ok && code == ExitRuntime && errors.As(err, &ae) && ae.IsAuth() {
			e.Code, e.Hints = ExitAuth, []string{"run `claude-code doctor` to check credentials"}
		}
	}
	switch e.Code {
	case ExitUsage:
		e.Kind = "usage"
	case ExitAuth:
		e.Kind = "auth"
	default:
		e.Kind = "runtime"
	}
	return e
}

// Report prints e to stderr (a JSON object under SetJSONErrors, secrets redacted) and returns its exit code.
func Report(e *Error) int {
	msg := textutil.RedactSecrets(e.Msg)
	if !jsonErrors.Load() {
		fmt.Fprintf(os.Stderr, "claude-code: %s\n", msg)
		return e.Code
	}
	obj := struct {
		Error    string   `json:"error"`
		Kind     string   `json:"kind"`
		ExitCode int      `json:"exit_code"`
		Hints    []string `json:"hints"`
	}{msg, e.Kind, e.Code, e.Hints}
	if err := WriteJSON(os.Stderr, obj); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code: %s\n", strings.TrimSpace(msg))
	}
	return e.Code
}
