package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
)

type opencodeSyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// opencodeMenuSync only appends absent ids; existing order, labels and explicit Responses flags are untouched.
func opencodeMenuSync(profile string, opts opencodeSyncOptions) error {
	p, snapshot, err := agent.OpencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return err
	}
	ids := agent.OpencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := agent.OpencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return err
		}
	}
	opencodeSyncSummary(snapshot, opts.Verify)
	next, skipped := agent.OpencodeSyncMerge(menu, ids, snapshot.Checks)
	changes := agent.DiffMenu(menu, next)
	fmt.Printf("opencode catalog sync: %d listed, %d eligible, %d additions, %d failed additions skipped, %d retained, %d total\n",
		len(snapshot.Models), len(ids), len(changes), skipped, len(menu), len(next))
	for _, c := range changes {
		fmt.Printf("%s %s  %s  responses=%v\n", c.Op, c.Model.ID, c.Model.Label, c.Model.Responses)
	}
	fmt.Println("Shared menu: OpenCode uses all entries; Codex needs Responses + bundled metadata.")
	fmt.Println("Discovery is not inference proof; use --verify or the existing check command:")
	fmt.Printf("  claude-code opencode catalog check --all --profile %s\n", profile)
	if !opts.Apply {
		fmt.Println("opencode catalog: dry run; no discovery/menu writes; pass --apply to write")
		return nil
	}
	if !snapshot.CacheHit || opts.Verify {
		if err := agent.SaveServingDiscovery(p, snapshot); err != nil {
			return err
		}
		fmt.Println("opencode catalog: discovery snapshot sealed in vault")
	}
	if len(changes) == 0 {
		fmt.Println("opencode catalog: no menu change")
		return nil
	}
	if opencodeMenuWrite(next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(changes))) != nil {
		return errors.New("cannot persist the shared model menu; check vault access")
	}
	return nil
}

func opencodeSyncSummary(snapshot agent.ServingDiscovery, verified bool) {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	fmt.Printf("Valid identity (not endpoint permissions); %s, discovered %s\n", source, snapshot.DiscoveredAt.Format(time.RFC3339))
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		fmt.Println("Verification: listing only; models are discovered/unverified")
		return
	}
	fmt.Printf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted\n",
		label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools)
	fmt.Println("Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}
