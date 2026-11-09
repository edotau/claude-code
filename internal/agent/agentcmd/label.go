package agentcmd

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/edotau/claude-code/internal/hooks/agents"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// agentsLabelPage is `agents label -h`.
var agentsLabelPage = helptext.Page{
	Usage: "claude-code agents label <type> [flags]",
	About: "Print the \"<model>: \" prefix the dispatch-effort hook requires on a dispatch description, " +
		"for a leaf of <type> with the given model and brief.",
}

// labelRow is `agents label --format json`.
type labelRow struct {
	Type     string `json:"type"`
	Model    string `json:"model"`
	Label    string `json:"label"`
	Source   string `json:"source"`
	Pressure bool   `json:"pressure,omitempty"`
}

// cmdAgentsLabel is `agents label <type>`: the description prefix the hook would write for this dispatch.
func cmdAgentsLabel(args []string) int {
	fs := helptext.NewFlagSet("agents label", agentsLabelPage)
	model := fs.String("model", "", "the dispatch's model (default unset: the hook classifies the brief)")
	brief := fs.String("brief", "", "the brief file the leaf gets; - reads stdin")
	sessionModel := fs.String("session-model", "", "the session's model id (default: from this session's transcript)")
	format := helptext.FormatFlag(fs)
	pos, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(pos) != 1 || !slices.Contains([]string{helptext.Text, helptext.JSON}, *format) {
		fs.Usage()
		return 2
	}
	task, err := readBrief(*brief)
	if err != nil {
		return helptext.Fail("agents label: %v", err)
	}
	cwd, _ := os.Getwd()
	sm := cmp.Or(*sessionModel, sessionModelOf(cwd, os.Getenv(memory.SessionIDEnv)))
	label, pinned, pressure := agents.DispatchLabel(agents.LabelInput{Type: pos[0], Model: *model, Brief: task, SessionModel: sm})
	row := labelRow{Type: pos[0], Model: pinned, Label: label, Source: labelSource(*model, label, sm), Pressure: pressure}
	if *format == helptext.JSON {
		return helptext.WriteJSONOrFail("agents label", row)
	}
	if label != "" { // an empty label is no prefix, as the hook writes none
		if _, err := fmt.Println(label + ": "); err != nil {
			return helptext.Fail("agents label: %v", err)
		}
	}
	return 0
}

// labelSource names where the label's model came from: the dispatch's --model, the session, or the pin and route.
func labelSource(dispatchModel, label, sessionModel string) string {
	switch {
	case dispatchModel != "" && dispatchModel != "inherit" && label == dispatchModel:
		return "dispatch"
	case sessionModel != "" && label == sessionModel:
		return "session"
	}
	return "derived"
}

// sessionModelOf is the model of the last turn in session sid's transcript in cwd's project; "" when unreadable.
func sessionModelOf(cwd, sid string) string {
	u, _ := transcript.LastUsage(memory.LiveTranscript(cwd, sid))
	return u.Model
}

// readBrief returns the brief file's text; "" is an empty brief and "-" reads stdin.
func readBrief(path string) (string, error) {
	switch path {
	case "":
		return "", nil
	case "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}
