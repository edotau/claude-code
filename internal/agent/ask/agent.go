package ask

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent/client"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	usagepkg "github.com/edotau/claude-code/internal/usage"
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
func ReportBackgroundStart(format string, rec agent.Record) {
	if format == helptext.JSON {
		body, _ := json.Marshal(struct {
			ID  string `json:"id"`
			Log string `json:"log"`
		}{rec.ID, rec.Log})
		fmt.Println(string(body))
		return
	}
	fmt.Printf("job %s started — claude-code agents status|tail|result|cancel %s\n", rec.ID, rec.ID)
}

// failAsk reports a pre-run failure; with --format json it prints the same envelope a run would.
func failAsk(format, agentName string, err error) int { return failAskCode(format, agentName, err, 1) }

// failAskCode is failAsk exiting with code (2 for a usage error).
func failAskCode(format, agentName string, err error, code int) int {
	printAskFailure(format, agentName, err)
	helptext.Fail("ask: %v", err)
	return code
}

// printAskFailure prints the typed failure envelope on stdout when format is json.
func printAskFailure(format, agentName string, err error) {
	switch format {
	case helptext.JSON:
		body, _ := json.Marshal(wire.FailureEnvelope(agentName, err))
		fmt.Println(string(body))
	case formatStreamJSON:
		_ = StdoutSink(format, "").Emit(wire.ResultEvent(wire.FailureEnvelope(agentName, err)))
	}
}

func reportAskResult(format string, res wire.Result, err error, dispatchID string) {
	for _, d := range res.Dropped {
		if d == "--read-only (tools off)" {
			fmt.Fprintf(os.Stderr, "ask: the %s agent runs with tools off (read-only sandbox where it has one)\n", res.Agent)
			continue
		}
		fmt.Fprintf(os.Stderr, "ask: %s has no channel on the %s agent — dropped\n", d, res.Agent)
	}
	if format == helptext.JSON {
		body, _ := json.Marshal(wire.NewEnvelope(res, err, dispatchID))
		fmt.Println(string(body))
		return
	}
	if format == formatStreamJSON {
		return // the result event already carried the envelope
	}
	if res.Answer != "" && !strings.HasSuffix(res.Answer, "\n") {
		fmt.Println()
	}
}

// ApplyRoute exports --route as HARNESS_ROUTE, so this process's picks (and the jobs it spawns) follow that ladder.
func ApplyRoute(route string) error {
	if route == "" {
		return nil
	}
	if !slices.Contains(router.RouteModes, route) {
		return fmt.Errorf("--route %q unknown (%s)", route, strings.Join(router.RouteModes, "|"))
	}
	return os.Setenv(router.EnvRoute, route)
}
