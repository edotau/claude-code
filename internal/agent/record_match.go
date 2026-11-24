package agent

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoRecord means no dispatch record matches the id.
var ErrNoRecord = errors.New("0 matches")

// noMatch is the error for n prefix matches of id; zero wraps ErrNoRecord.
func noMatch(id string, names []string, n int) error {
	why := ErrNoRecord
	if n > 0 {
		why = fmt.Errorf("%d matches", n)
	}
	return fmt.Errorf("dispatch %q: %w (see claude-code agents status); nearest: %s",
		id, why, strings.Join(nearestIDs(names, id), ", "))
}
