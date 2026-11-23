// actions.go is the dashboard's c/y/r actions: pure transitions to an effect, and the one runner that performs it.

package tree

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/textutil"
)

type action int

const (
	actCancel action = iota
	actCopy
	actResult
)

type effectKind int

const (
	effectNone effectKind = iota
	effectCancel
	effectCopy
	effectShow
)

// effect is what an action asks for; only perform carries it out.
type effect struct {
	Kind effectKind
	ID   string
	Line string
}

// cancelJob is the seam for `agents cancel <id>`: tests swap it so no real job is stopped.
var cancelJob = func(id string) (string, error) {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		return "", err
	}
	result, err := agent.CancelJob(context.Background(), rec.Agent, "", rec.ID)
	if err != nil {
		return "", err
	}
	if result.AlreadyFinished {
		return "already finished " + rec.ID, nil
	}
	return "cancelled " + rec.ID, nil
}

// actionTarget is the outer carrier of a folded row: cancelling it stops its child too, and openJob uses the same id.
func actionTarget(n Node) string { return cmp.Or(n.Via, n.ID) }

// onAction applies a to n; a cancel only arms the confirm (the returned pending id), it never cancels.
func onAction(pending string, a action, n Node) (string, effect) {
	id := actionTarget(n)
	switch a {
	case actCopy:
		if n.Kind != KindDispatch || id == "" {
			return "", showAction("y: no job id on this row")
		}
		return "", effect{Kind: effectCopy, ID: id, Line: actionLine("copied " + id)}
	case actResult:
		if n.Kind != KindDispatch || id == "" {
			return "", showAction("r: no job id on this row")
		}
		return "", showAction("claude-code agents result -f " + id)
	case actCancel:
		if !runningAction(n) || id == "" {
			return "", showAction("c: only a running job cancels")
		}
		const suffix = "? Y confirms, any other key keeps it"
		label := textutil.TruncateCells(textutil.OneLine(cmp.Or(n.Agent, n.Dispatch.Agent)+" "+id),
			maxLineWidth-len("cancel ")-len(suffix), "…")
		return id, showAction("cancel " + label + suffix)
	}
	return pending, effect{}
}

// onConfirm answers an armed cancel: only a confirm on the same, still running row cancels; any other key disarms.
func onConfirm(pending string, confirmed bool, n Node) (string, effect) {
	if pending == "" {
		return "", effect{}
	}
	if confirmed && pending == actionTarget(n) && runningAction(n) {
		return "", effect{Kind: effectCancel, ID: pending}
	}
	return "", showAction("cancel kept")
}

func runningAction(n Node) bool {
	return n.Kind == KindDispatch && n.Dispatch != nil && n.Dispatch.Status == statusRunning
}

// perform runs e and returns the status line; a copy writes OSC 52 to w, which reaches the clipboard over SSH too.
func perform(e effect, w io.Writer) string {
	switch e.Kind {
	case effectCancel:
		line, err := cancelJob(e.ID)
		if err != nil {
			return actionLine(fmt.Sprintf("cancel %s: %v", e.ID, err))
		}
		return actionLine(line)
	case effectCopy:
		if _, err := io.WriteString(w, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(e.ID))+"\a"); err != nil {
			return actionLine(fmt.Sprintf("copy %s: %v", e.ID, err))
		}
		return actionLine(e.Line)
	case effectShow:
		return actionLine(e.Line)
	}
	return ""
}

func showAction(line string) effect {
	return effect{Kind: effectShow, Line: actionLine(line)}
}

func actionLine(line string) string {
	return textutil.TruncateCells(textutil.OneLine(line), maxLineWidth, "…")
}
