package ask

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/job"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/launchcli"
	"github.com/edotau/claude-code/internal/databricks"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/ui/tui"
	usagepkg "github.com/edotau/claude-code/internal/usage"
	"github.com/edotau/claude-code/internal/vault"

	// Each vendor CLI registers its roster row and driver from its package init; the program links them all through the agents package.
	_ "github.com/edotau/claude-code/internal/claude"
	_ "github.com/edotau/claude-code/internal/codex"
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini"
	_ "github.com/edotau/claude-code/internal/opencode"
)

// RouteRefresh re-fetches a stale account for a pick; a var so tests stay offline.
var RouteRefresh = usagepkg.RefreshAccount

// attachFlag collects repeatable --attach paths; Set resolves each to an absolute path and fails fast
// unless it names an existing regular file.
type attachFlag []string

func (a *attachFlag) String() string { return strings.Join(*a, ",") }

// Placeholder names the FLAGS placeholder for attachFlag (helptext.Placeholder).
func (a *attachFlag) Placeholder() string { return "PATH" }

func (a *attachFlag) Set(s string) error {
	abs, err := filepath.Abs(s)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("--attach %q: not a regular file", s)
	}
	// reject non-image binary up front: it fails the same way on every agent, so it must never reach the fallback chain
	if data, err := os.ReadFile(abs); err != nil {
		return fmt.Errorf("--attach %q: %w", s, err)
	} else if !agent.IsText(data) && !agent.IsImage(data) {
		return fmt.Errorf("--attach %q: binary file; attach text only", s)
	}
	*a = append(*a, abs)
	return nil
}

// keepEnvFlag collects repeatable --keep-env NAME[,NAME] values, comma-split per occurrence.
type keepEnvFlag []string

func (k *keepEnvFlag) String() string { return strings.Join(*k, ",") }

// Placeholder names the FLAGS placeholder for keepEnvFlag (helptext.Placeholder).
func (k *keepEnvFlag) Placeholder() string { return "NAME,NAME" }

func (k *keepEnvFlag) Set(s string) error {
	*k = append(*k, strings.Split(s, ",")...)
	return nil
}

// envFlag collects repeatable --env NAME=VALUE entries.
type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }

// Placeholder names the FLAGS placeholder for envFlag (helptext.Placeholder).
func (e *envFlag) Placeholder() string { return "NAME=VALUE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so RunAsk validates after Parse.
func (e *envFlag) Set(s string) error {
	*e = append(*e, s)
	return nil
}

// ruleFlag collects repeatable --allow/--ask/--deny permission rules; RunAsk validates them after Parse.
type ruleFlag []string

func (r *ruleFlag) String() string { return strings.Join(*r, ",") }

// Placeholder names the FLAGS placeholder for ruleFlag (helptext.Placeholder).
func (r *ruleFlag) Placeholder() string { return "RULE" }

func (r *ruleFlag) Set(s string) error {
	*r = append(*r, s)
	return nil
}

// agentsFlag is the --agents value: inline JSON or a file of agent definitions.
type agentsFlag string

func (a *agentsFlag) String() string { return string(*a) }

// Placeholder names the FLAGS placeholder for agentsFlag (helptext.Placeholder).
func (a *agentsFlag) Placeholder() string { return "JSON|FILE" }

// Set accepts anything: the flag parser echoes a rejected value in its error, so RunAsk validates after Parse.
func (a *agentsFlag) Set(s string) error {
	*a = agentsFlag(s)
	return nil
}

// NewClient builds the dispatch client; RouteRefresh is the offline-able account refresh.
func NewClient() *client.Client {
	return client.NewClient(router.RuleClassifier{}.Alias, RouteRefresh,
		func(s string) { fmt.Fprintln(os.Stderr, "ask:", s) })
}

// backgroundExtraFlags carries explicit routing and identity flags over to the spawned --job child.
func backgroundExtraFlags(fs *flag.FlagSet, fallback string, native bool) []string {
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "as":
			extra = append(extra, "--as", f.Value.String())
		case "fallback":
			extra = append(extra, "--fallback", fallback)
		case "native":
			if native {
				extra = append(extra, "--native")
			}
		}
	})
	return extra
}

// ReportBackgroundStart tells the caller how to track a detached job.
func ReportBackgroundStart(format string, rec job.Record) error {
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
		_, err := fmt.Println(string(body))
		return err
	}
	_, err := fmt.Printf("job %s started — claude-code agents status|tail|result|cancel %s\n", rec.ID, rec.ID)
	return err
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	helptext.Fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json; a lost write is noted on stderr.
func printAskFailure(format, agentName string, err error) {
	var werr error
	switch format {
	case helptext.JSON:
		body, _ := json.Marshal(wire.FailureEnvelope(agentName, err))
		_, werr = fmt.Println(string(body))
	case client.FormatStreamJSON:
		werr = StdoutSink(format, "").Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
	}
	if werr != nil {
		fmt.Fprintf(os.Stderr, "ask: failure envelope not written to stdout: %v\n", werr)
	}
}

// reportAskResult prints the outcome; it returns the stdout write error of the json envelope, else nil.
func reportAskResult(format string, res wire.Result, err error, dispatchID string) error {
	for _, d := range res.Dropped {
		if d == "--read-only (tools off)" {
			fmt.Fprintf(os.Stderr, "ask: the %s agent runs with tools off (read-only sandbox where it has one)\n", res.Agent)
			continue
		}
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == helptext.JSON {
		body, _ := json.Marshal(wire.NewEnvelope(res, err, dispatchID))
		_, werr := fmt.Println(string(body))
		return werr
	}
	if format == client.FormatStreamJSON {
		return nil // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
	return nil
}

// ApplyRoute exports --route as HARNESS_ROUTE (databricks:<p> also as CLAUDE_PROFILE) for this process and its jobs.
func ApplyRoute(route, profile string) error {
	if route == "" {
		return nil
	}
	mode, p, err := router.ParseRoute(route)
	if err != nil {
		return err
	}
	if mode == router.RouteDatabricks {
		return applyArm(p, profile)
	}
	if arm := router.ArmProfile(); arm != "" {
		return errLeaveArm(arm)
	}
	return os.Setenv(router.EnvRoute, mode)
}

// applyArm resolves route databricks:<p> (bare: from profile, else CLAUDE_PROFILE), checks the row and exports it.
func applyArm(p, profile string) error {
	if p == "" {
		if p = bareArm(profile); p == "" {
			return errors.New("--route databricks needs a workspace: databricks:<profile>")
		}
	}
	if arm := router.ArmProfile(); arm != "" && p != arm {
		return errLeaveArm(arm)
	}
	if err := providers.ServingRow(p); err != nil {
		return err
	}
	if err := os.Setenv(router.EnvRoute, router.RouteDatabricks+":"+p); err != nil {
		return err
	}
	return os.Setenv(providers.EnvSessionProfile, p)
}

// errLeaveArm refuses a typed --route that differs from the inherited databricks:<arm>.
func errLeaveArm(arm string) error {
	return fmt.Errorf("--route: a nested dispatch cannot leave databricks:%s", arm)
}

// bareArm is the first of profile and CLAUDE_PROFILE that names a Databricks row, else "".
func bareArm(profile string) string {
	for _, name := range []string{profile, os.Getenv(providers.EnvSessionProfile)} {
		if name != "" && providers.ServingRow(name) == nil {
			return name
		}
	}
	return ""
}

// CheckArm refuses a flag that would take the dispatch off route databricks:<p>; nil outside the route.
func CheckArm(agentName, profile, provider, model string, direct bool) error {
	arm := router.ArmProfile()
	if arm == "" {
		return nil
	}
	if err := armFlags(arm, profile, provider, direct); err != nil {
		return err
	}
	reg, err := providers.Load()
	if err != nil {
		return fmt.Errorf("--route databricks:%s: %w", arm, err)
	}
	if prof, _, typed := strings.Cut(model, ":"); typed && reg.Providers[prof] != nil && prof != arm {
		return fmt.Errorf("--model %s: outside --route databricks:%s", model, arm)
	}
	return armModel(reg.Providers[arm], agentName, strings.TrimPrefix(model, arm+":"))
}

// armFlags refuses --direct, a --profile other than the workspace and a --provider other than databricks.
func armFlags(arm, profile, provider string, direct bool) error {
	switch {
	case direct:
		return fmt.Errorf("--direct: refused inside --route databricks:%s", arm)
	case profile != "" && profile != arm && profile != agent.LeadSession:
		return fmt.Errorf("--profile %s: outside --route databricks:%s", profile, arm)
	case provider != "" && provider != router.RouteDatabricks:
		return fmt.Errorf("--provider %s: outside --route databricks:%s", provider, arm)
	}
	return nil
}

// armModel refuses an explicit model id the workspace does not serve agentName; aliases and discovery pass.
func armModel(p *providers.Provider, agentName, model string) error {
	if model == "" || agentName == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, model) {
		return nil
	}
	served := providers.SpellPin(p, model) != ""
	if agentName != "claude" { // Serves covers external agents only
		_, why := agent.ProfileServes(agentName, p, model)
		served = why == ""
	}
	if !served {
		return fmt.Errorf("--model %s: not served to %s on databricks:%s", model, agentName, p.Name)
	}
	return nil
}

// The terminal and sign-in UI the agent package calls but does not import.
func init() {
	launchcli.IsTerminal = proc.IsTerminal
	agent.StdinIsTerminal = tui.StdinIsTerminal
	agent.LoginRow = databricks.LoginRow
	agent.PromptSecret = func(name string) (string, error) {
		return tui.Secret("Paste the value for "+name+" (input hidden, Enter when done): ", vault.Preview)
	}
}
