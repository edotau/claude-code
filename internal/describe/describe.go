// Package describe builds the machine-readable map of the harness behind `describe`.
package describe

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/client"
	"github.com/edotau/claude-code/internal/completion"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Describe is the one machine-readable map of the harness: an agent constructing commands reads this
// instead of --help-ing its way through each verb. Credentials never appear — only whether one resolves.
type Describe struct {
	Version   string            `json:"version"`
	Commands  []DescribeCommand `json:"commands"`
	Providers DescribeProviders `json:"providers"`
	Agents    []DescribeAgent   `json:"agents"`
	Hooks     []DescribeHook    `json:"hooks"`
	Models    []string          `json:"models"`
}

type DescribeCommand struct {
	Name     string   `json:"name"`
	Group    string   `json:"group,omitempty"`
	Summary  string   `json:"summary"`
	Hidden   bool     `json:"hidden,omitempty"`
	Subverbs []string `json:"subverbs,omitempty"`
	Flags    []string `json:"flags,omitempty"` // "--name" or "--name <value>"
}

type DescribeProviders struct {
	Session  string             `json:"session"` // the profile a launch resolves to today
	Default  string             `json:"default"`
	Fallback []string           `json:"fallback,omitempty"`
	List     []DescribeProvider `json:"list"`
}

type DescribeProvider struct {
	Name       string            `json:"name"`
	Protocol   string            `json:"protocol"`
	Dialects   []string          `json:"dialects"`
	Auth       string            `json:"auth"`
	Login      string            `json:"login,omitempty"`
	Credential bool              `json:"credential"`
	Models     map[string]string `json:"models,omitempty"`
}

type DescribeAgent struct {
	Name      string     `json:"name"`
	Available bool       `json:"available"`
	Reason    string     `json:"reason,omitempty"`
	Caps      agent.Caps `json:"caps"`
}

type DescribeHook struct {
	Name    string `json:"name"`
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Timeout int    `json:"timeout"`
}

// Verb is one top-level verb as the cli table lists it; Node is its completion spec (nil for a bare launcher).
type Verb struct {
	Name, Group, Summary string
	Hidden               bool
	Node                 *completion.Node
}

// Run is the `describe` verb over the cli table's verbs.
func Run(args []string, verbs []Verb) int {
	fs := helptext.NewFlagSet("describe", helptext.Page{
		Usage: "claude-code describe [flags]",
		About: "One map of the whole harness for an agent: verbs, providers, agents, model aliases and hooks. " +
			"Credentials never appear, only whether one resolves.",
	})
	format := fs.String("format", "json", "json (the full map) or text (a short listing)")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	reg, err := providers.Load()
	if err != nil {
		return helptext.Fail("describe: %v", err)
	}
	d, err := Build(reg, verbs)
	if err != nil {
		return helptext.Fail("describe: %v", err)
	}
	switch *format {
	case "json":
		return helptext.WriteJSONOrFail("describe", d)
	case "text":
		printDescribe(d)
	default:
		fs.Usage()
		return 2
	}
	return 0
}

// Build builds the describe view from an already-loaded registry, so a caller that loaded one
// itself (e.g. configureProviderList) never pays for a second load.
func Build(reg *providers.Registry, verbs []Verb) (Describe, error) {
	d := Describe{Version: textutil.BuildVersion(), Models: models.AliasNames}
	for _, c := range verbs {
		if strings.HasPrefix(c.Name, "__") {
			continue
		}
		node := c.Node
		d.Commands = append(d.Commands, DescribeCommand{
			Name: c.Name, Group: c.Group, Summary: c.Summary, Hidden: c.Hidden,
			Subverbs: subwordNames(node), Flags: FlagNames(node),
		})
	}
	d.Providers = DescribeProviders{Default: reg.Default, Fallback: reg.Fallback}
	if sel, err := providers.Select(reg, ""); err == nil && sel.Provider != nil {
		d.Providers.Session = sel.Provider.Name
	}
	for _, n := range reg.Names() {
		p := reg.Providers[n]
		d.Providers.List = append(d.Providers.List, DescribeProvider{
			Name: n, Protocol: p.Protocol, Dialects: p.Dialects(), Auth: p.Auth.Method(), Login: p.Auth.Login,
			Credential: providers.CredentialPresent(p), Models: p.Models,
		})
	}
	for _, r := range client.RosterRows() {
		d.Agents = append(d.Agents, DescribeAgent{Name: r.Name, Available: r.Available, Reason: r.Reason, Caps: r.Levels})
	}
	for _, s := range settings.Specs() {
		d.Hooks = append(d.Hooks, DescribeHook{Name: s.Name, Event: s.Event, Matcher: s.Matcher, Timeout: s.Seconds()})
	}
	return d, nil
}

// subwordNames lists node's own subcommand words, sorted; nil for a leaf/launcher node.
func subwordNames(node *completion.Node) []string {
	if node == nil || node.Sub == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(node.Sub))
}

// FlagNames lists a verb's completable flags across its own node and every subverb, "--flag" or
// "--flag <value>", sorted.
func FlagNames(node *completion.Node) []string {
	if node == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, name := range ownFlagNames(node) {
		seen[name] = true
	}
	for _, sub := range node.Sub {
		for _, name := range ownFlagNames(sub) {
			seen[name] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// ownFlagNames lists only node's own flags ("--flag" or "--flag <value>"), sorted.
func ownFlagNames(node *completion.Node) []string {
	if node == nil {
		return nil
	}
	var out []string
	for _, f := range node.Flags {
		name := "--" + f.Name
		if !f.IsBool {
			name += " <value>"
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func printDescribe(d Describe) {
	fmt.Printf("claude-code %s\ncommands:\n", d.Version)
	for _, c := range d.Commands {
		if !c.Hidden {
			fmt.Printf("  %-12s %s\n", c.Name, c.Summary)
		}
	}
	fmt.Printf("providers (session=%s default=%s):\n", d.Providers.Session, d.Providers.Default)
	for _, p := range d.Providers.List {
		fmt.Printf("  %-15s %-10s cred=%-5v %s\n", p.Name, p.Auth, p.Credential, strings.Join(p.Dialects, ","))
	}
	fmt.Println("agents:")
	for _, r := range d.Agents {
		fmt.Printf("  %-10s available=%v %s\n", r.Name, r.Available, r.Reason)
	}
	fmt.Printf("hooks: %d registered\n", len(d.Hooks))
}

// Spec is the completion node for the describe verb.
func Spec() *completion.Node {
	return &completion.Node{Flags: []completion.Flag{completion.FormatFlag()}}
}
