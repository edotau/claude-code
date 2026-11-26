package doctor

import (
	"cmp"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/router"
)

// localRow is the last-resort row: the cached probe, machine-wide holders, the last fallback and the ranks.json stage.
func localRow() check {
	st := router.LocalStatus()
	at, kind, surface, _ := router.LastLocalFallback("")
	return localCheck(st, at, kind, surface, catalog.Local().Stage)
}

// localCheck renders localRow; down (no row included) is a soft ✗ that drops the fallback hint.
func localCheck(st router.LocalState, at time.Time, kind, surface, stage string) check {
	row := check{Name: "local", OK: st.Up, Detail: "down (" + st.Reason + ")"}
	if st.Up {
		row.Detail = fmt.Sprintf("up · %s · %d running", st.Model, st.Running)
		if !at.IsZero() {
			row.Detail += fmt.Sprintf(" · last fallback %s %s (%s)", at.Local().Format("2006-01-02 15:04"), kind, surface)
		}
	}
	row.Detail += " · stage " + cmp.Or(stage, "unset")
	return row
}
