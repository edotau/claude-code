package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

type opencodeSyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// opencodeSyncModels resolves the profile; discovery validates identity before reading the sealed cache.
func opencodeSyncModels(profile string, refresh bool) (*providers.Provider, providers.ServingDiscovery, error) {
	_, p, err := opencodeProvider(profile)
	if err != nil {
		return nil, providers.ServingDiscovery{}, errors.New("cannot resolve profile; check the provider configuration")
	}
	snapshot, err := providers.DiscoverServingModels(context.Background(), p, refresh)
	return p, snapshot, err
}

// opencodeSyncIDs selects unique, losslessly spellable READY chat endpoint ids in deterministic order.
func opencodeSyncIDs(infos []providers.ModelInfo) []string {
	var ids []string
	for _, info := range infos {
		if info.Gateway || info.Ready != "READY" || info.Task != "llm/v1/chat" ||
			!strings.HasPrefix(info.ID, models.DatabricksPrefix) {
			continue
		}
		id := providers.MenuID(info.ID)
		if id != "" && models.Wire("databricks-serving", id) == info.ID {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// opencodeMenuSync only appends absent ids; existing order, labels and explicit Responses flags are untouched.
func opencodeMenuSync(profile string, opts opencodeSyncOptions, fail func(string, ...any) int) int {
	p, snapshot, err := opencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return fail("%v", err)
	}
	ids := opencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := opencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return fail("%v", err)
		}
	}
	opencodeSyncSummary(snapshot, opts.Verify)
	next, skipped := opencodeSyncMerge(menu, ids, snapshot.Checks)
	changes := providers.DiffMenu(menu, next)
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
		return 0
	}
	if !snapshot.CacheHit || opts.Verify {
		if err := providers.SaveServingDiscovery(p, snapshot); err != nil {
			return fail("%v", err)
		}
		fmt.Println("opencode catalog: discovery snapshot sealed in vault")
	}
	if len(changes) == 0 {
		fmt.Println("opencode catalog: no menu change")
		return 0
	}
	return opencodeMenuWrite(next, fmt.Sprintf("synced %d addition(s); relaunch Codex too if used", len(changes)),
		func(string, ...any) int { return fail("cannot persist the shared model menu; check vault access") })
}

func opencodeSyncMerge(menu []providers.OpencodeModel, ids []string, checks []providers.CheckResult) ([]providers.OpencodeModel, int) {
	failed := make(map[string]bool)
	for _, c := range checks {
		failed[c.ID] = c.Status == "fail" || c.Status == "missing"
	}
	next := slices.Clone(menu)
	skipped := 0
	for _, id := range ids {
		if !slices.ContainsFunc(menu, func(m providers.OpencodeModel) bool { return m.ID == id }) {
			if failed[id] {
				skipped++
				continue
			}
			// MenuAdd canonicalizes its input, so pass the lossless wire name rather than canonicalizing the id twice.
			next = providers.MenuAdd(next, models.Wire("databricks-serving", id), "", nil)
		}
	}
	return next, skipped
}

func opencodeSyncSummary(snapshot providers.ServingDiscovery, verified bool) {
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
