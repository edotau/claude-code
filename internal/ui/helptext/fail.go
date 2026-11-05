package helptext

import (
	"fmt"
	"os"
)

// Fail prints a one-line error and returns exit code 1.
func Fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "claude-code: "+format+"\n", a...)
	return 1
}

// FailUsage prints a one-line error and returns usage exit code 2.
func FailUsage(format string, a ...any) int {
	Fail(format, a...)
	return 2
}

// WriteJSONOrFail writes v as indented JSON to stdout; a write error is reported under label and returns 1.
func WriteJSONOrFail(label string, v any) int {
	if err := WriteJSON(os.Stdout, v); err != nil {
		return Fail("%s: %v", label, err)
	}
	return 0
}

// BadFormat reports an invalid --format value as a usage error (exit 2).
func BadFormat(prefix string, err error) int {
	if prefix == "" {
		return FailUsage("%v", err)
	}
	return FailUsage(prefix+": %v", err)
}
