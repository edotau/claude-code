package configure

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// ghAccountRE pulls the account name out of `gh auth status`'s "Logged in to github.com account <name>" line.
var ghAccountRE = regexp.MustCompile(`account\s+(\S+)`)

// ghStatus is what `gh auth status` reports: whether the binary exists, whether it is logged in, and the
// account name — never a token (gh itself only ever prints a masked one).
type ghStatus struct {
	Found    bool
	LoggedIn bool
	Account  string
}

// GHAuthCheck runs `gh auth status` (8s cap); its exit code is the verdict, its stdout+stderr the account line.
func GHAuthCheck() ghStatus {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return ghStatus{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "auth", "status").CombinedOutput()
	st := ghStatus{Found: true, LoggedIn: err == nil}
	if m := ghAccountRE.FindStringSubmatch(string(out)); len(m) > 1 {
		st.Account = m[1]
	}
	return st
}

// WriteGithubCopilotModels validates and merges picks and fallback into vault overlay/providers.
func WriteGithubCopilotModels(picks map[string]string, fallback string) error {
	for _, alias := range []string{models.Opus, models.Sonnet, models.Haiku} {
		if strings.TrimSpace(picks[alias]) == "" {
			return fmt.Errorf("github-copilot models: %s model is empty", alias)
		}
	}
	if fallback == "auto" {
		return errors.New(`github-copilot opencode_fallback: "auto" is not a valid fallback (opencode's own catalog has no alias by that name)`)
	}
	fields := map[string]any{"models": picks}
	if fallback != "" {
		fields["opencode_fallback"] = fallback
	}
	reg, err := providers.Load()
	if err != nil {
		return err
	}
	p := reg.ProfileBy("github", providers.AuthCLI)
	if p == nil {
		return errors.New("github models: no github profile in the registry")
	}
	return providers.SetProviderFields(p.Name, fields)
}
