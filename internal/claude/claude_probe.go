package claude

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

const claudeProbeTTL = 10 * time.Second

type claudeProbe struct {
	loggedIn bool
	plan     string
}

// claudeProbeMemo is single-flight; only a logged-in answer is kept, so the recheck after a login sees it.
var claudeProbeMemo = paths.NewMemo[struct{}, claudeProbe]("claude.probe")

// claudeLoginProbe asks `claude auth status` (JSON); err != nil means unknown. Single-flight; logged-in memo 10s.
func claudeLoginProbe() (loggedIn bool, plan string, err error) {
	p, err := claudeProbeMemo.Do(struct{}{}, func(p claudeProbe, err error) time.Duration {
		if err == nil && p.loggedIn {
			return claudeProbeTTL
		}
		return -1
	}, runClaudeProbe)
	return p.loggedIn, p.plan, err
}

func runClaudeProbe() (claudeProbe, error) {
	bin, err := ClaudeBinary()
	if err != nil {
		return claudeProbe{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.ProbeTimeout)
	defer cancel()
	probe := agent.ProbeContext(ctx, "claude") // claude's own login only
	out, runErr := agent.ProbeCLI(probe, bin, agent.EnvSpec{Mode: agent.EnvAgent}, false, "auth", "status")
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if jerr := json.Unmarshal(out, &st); jerr != nil { // logged-out may exit nonzero but still print JSON
		if runErr != nil {
			return claudeProbe{}, runErr
		}
		return claudeProbe{}, jerr
	}
	p := claudeProbe{loggedIn: st.LoggedIn}
	if st.SubscriptionType != "" {
		p.plan = " (" + st.SubscriptionType + ")"
	}
	return p, nil
}
