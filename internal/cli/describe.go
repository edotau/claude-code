package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// Describe is the one machine-readable map of the harness: an agent constructing commands reads this
// instead of --help-ing its way through each verb. Credentials never appear — only whether one resolves.
type Describe struct {
	Version   string            `json:"version"`
	Commands  []DescribeCommand `json:"commands"`
	Providers DescribeProviders `json:"providers"`
	Runners   []DescribeRunner  `json:"runners"`
	Hooks     []DescribeHook    `json:"hooks"`
	Slots     []string          `json:"slots"`
}

type DescribeCommand struct {
	Name     string   `json:"name"`
	Group    string   `json:"group,omitempty"`
	Summary  string   `json:"summary"`
	Aliases  []string `json:"aliases,omitempty"`
	Hidden   bool     `json:"hidden,omitempty"`
	Subverbs []string `json:"subverbs,omitempty"`
	Flags    []string `json:"flags,omitempty"` // "--name" or "--name <value>"
}

type DescribeProviders struct {
	Session  string             `json:"session"` // the provider a launch resolves to today
	Default  string             `json:"default"`
	Fallback []string           `json:"fallback,omitempty"`
	List     []DescribeProvider `json:"list"`
}

type DescribeProvider struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Dialects   []string          `json:"dialects"`
	Auth       string            `json:"auth"`
	Login      string            `json:"login,omitempty"`
	Credential bool              `json:"credential"`
	Models     map[string]string `json:"models,omitempty"`
}

type DescribeRunner struct {
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

const describeUsage = "usage: claude-code describe [--format json|text]"

func cmdDescribe(args []string) int {
	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
	fs.Usage = func() {
		helptext.Fprint(fs.Output(), "Usage of "+fs.Name()+":\n")
		helptext.Defaults(fs)
	}
	fs.SetOutput(os.Stderr)
	format := fs.String("format", "json", "json (the full map) or text (a short listing)")
	if fs.Parse(args) != nil {
		return 2
	}
	d, err := describe()
	if err != nil {
		return fail("describe: %v", err)
	}
	switch *format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(d)
	case "text":
		printDescribe(d)
	default:
		helptext.Fprint(os.Stderr, describeUsage+"\n")
		return 2
	}
	return 0
}

func describe() (Describe, error) {
	reg, err := providers.Load()
	if err != nil {
		return Describe{}, err
	}
	d := Describe{Version: buildVersion(), Slots: models.Slots}
	for _, c := range commands() {
		if strings.HasPrefix(c.name, "__") {
			continue
		}
		node := specTree()[c.name]
		d.Commands = append(d.Commands, DescribeCommand{
			Name: c.name, Group: c.group, Summary: c.summary, Aliases: c.aliases, Hidden: c.hidden,
			Subverbs: subwordNames(node), Flags: flagNames(node),
		})
	}
	d.Providers = DescribeProviders{Default: reg.Default, Fallback: reg.Fallback}
	if sel, err := providers.Select(reg, ""); err == nil && sel.Provider != nil {
		d.Providers.Session = sel.Provider.Name
	}
	for _, n := range reg.Names() {
		p := reg.Providers[n]
		d.Providers.List = append(d.Providers.List, DescribeProvider{
			Name: n, Kind: p.Kind, Dialects: p.Dialects(), Auth: p.Auth.Type, Login: p.Auth.Login,
			Credential: providers.CredentialPresent(p), Models: p.Models,
		})
	}
	for _, r := range agent.Runners() {
		dr := DescribeRunner{Name: r.Name(), Available: true, Caps: r.Caps()}
		if err := r.Available(); err != nil {
			dr.Available, dr.Reason = false, err.Error()
		}
		d.Runners = append(d.Runners, dr)
	}
	for _, s := range hookspec.Registry {
		d.Hooks = append(d.Hooks, DescribeHook{Name: s.Name, Event: s.Event, Matcher: s.Matcher, Timeout: s.Timeout})
	}
	return d, nil
}

// subwordNames lists node's own subcommand words, sorted; nil for a leaf/launcher node.
func subwordNames(node *compNode) []string {
	if node == nil || node.sub == nil {
		return nil
	}
	out := make([]string, 0, len(node.sub))
	for w := range node.sub {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// flagNames lists a verb's completable flags across its own node and every subverb, "--flag" or
// "--flag <value>", sorted.
func flagNames(node *compNode) []string {
	if node == nil {
		return nil
	}
	seen := map[string]bool{}
	add := func(infos []flagInfo) {
		for _, f := range infos {
			name := "--" + f.name
			if !f.isBool {
				name += " <value>"
			}
			seen[name] = true
		}
	}
	add(node.flags)
	for _, sub := range node.sub {
		add(sub.flags)
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "devel"
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
	fmt.Println("runners:")
	for _, r := range d.Runners {
		fmt.Printf("  %-10s available=%v %s\n", r.Name, r.Available, r.Reason)
	}
	fmt.Printf("hooks: %d registered\n", len(d.Hooks))
}
