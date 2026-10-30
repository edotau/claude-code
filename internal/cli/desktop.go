package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/helptext"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

var desktopPage = helptext.Page{
	Usage: "claude-code configure desktop [--profile p] [--revert] [--dry-run]",
	About: "Point Claude Desktop at the loopback proxy using a Claude-3p profile.\n" +
		"Open Desktop once first, then quit it before applying or reverting.\n" +
		"On WSL, targets Windows Desktop and runs the credential helper in WSL.\n" +
		"--revert restores the previous profile selection. --dry-run writes nothing\n" +
		"and prints exact destination files with custom header values masked.",
}

var (
	desktopRoot          = desktop.Root
	desktopHelper        = desktop.HelperSpec
	desktopEnsure        = proxy.Ensure
	desktopGatewayModels = providers.GatewayModels
)

func cmdConfigureDesktop(args []string) int {
	fs := helptext.NewFlagSet("configure desktop", desktopPage)
	profile := fs.String("profile", "", "profile serving Desktop (default: session profile)")
	revert := fs.Bool("revert", false, "remove our Desktop profile and restore the previous selection")
	dryRun := fs.Bool("dry-run", false, "print exact files with secrets masked; write nothing")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if fs.NArg() != 0 {
		return fail("configure desktop: unexpected argument %q", fs.Arg(0))
	}
	if *revert && *profile != "" {
		return fail("configure desktop: --profile cannot be combined with --revert")
	}
	root, err := desktopRoot()
	if err != nil {
		return fail("configure desktop: %v", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fail("configure desktop: open Claude Desktop once first (missing directory %s)", root)
	}
	opts := desktop.Options{Root: root, Revert: *revert, DryRun: *dryRun}
	if !*revert {
		reg, err := providers.Load()
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		sel, err := providers.Select(reg, *profile)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		p := sel.Provider
		if p.Auth.Method() == providers.AuthCLI {
			return fail("configure desktop: profile %s uses a CLI login; select an API profile", p.Name)
		}
		opts.Profile = p.Name
		opts.HelperPath, opts.HelperArgs, opts.HelperWindows, err = desktopHelper(p.Name)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		opts.Models, err = desktopModelList(ctx, p)
		if err != nil {
			return fail("configure desktop: %v", err)
		}
		opts.Headers = proxy.HarnessHeaders()
		opts.Headers[proxy.AgentHeader] = "desktop"
		base := proxy.Base(proxy.DefaultPort)
		if *dryRun {
			if st, ok := proxy.Running(ctx); ok {
				base = proxy.Base(st.Port)
			}
		} else {
			base, err = desktopEnsure(ctx)
			if err != nil {
				return fail("configure desktop: proxy unavailable; run gateway proxy --start")
			}
		}
		opts.BaseURL = proxy.SessionBase(base, p.Name)
	}
	if err := desktop.Configure(opts, os.Stdout); err != nil {
		return fail("configure desktop: %v", err)
	}
	return 0
}

func desktopModelName(id string) string {
	if canonical, _, ok := models.Resolve(id); ok {
		if entry, ok := models.Lookup(canonical); ok {
			return entry.Name
		}
	}
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

func desktopModelList(ctx context.Context, p *providers.Provider) ([]desktop.Model, error) {
	var infos []providers.ModelInfo
	if p.IsAIGateway() {
		var err error
		infos, err = desktopGatewayModels(ctx, p)
		if err != nil {
			fmt.Fprintln(os.Stderr, "configure desktop: gateway catalog unavailable; using profile model pins")
		}
	}
	if len(infos) == 0 {
		for _, id := range p.Models {
			id = models.Strip1M(id)
			infos = append(infos, providers.ModelInfo{ID: id, Name: desktopModelName(id)})
		}
	}
	var out []desktop.Model
	seen := map[string]bool{}
	for _, info := range infos {
		id := models.Strip1M(info.ID)
		if models.FamilyKind(id) != models.FamilyClaude || seen[id] {
			continue
		}
		seen[id] = true
		name := info.Name
		if name == "" {
			name = id
		}
		out = append(out, desktop.Model{Name: id, LabelOverride: name})
	}
	slices.SortFunc(out, func(a, b desktop.Model) int { return strings.Compare(a.Name, b.Name) })
	if len(out) == 0 {
		return nil, fmt.Errorf("profile %s has no Claude models in its catalog", p.Name)
	}
	return out, nil
}
