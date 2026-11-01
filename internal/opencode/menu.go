package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// MenuRow is one `opencode catalog list` line; Live, Ready and Check are set only for a live listing.
type MenuRow struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Responses bool   `json:"responses"`
	Source    string `json:"source"`
	Live      string `json:"live,omitempty"`
	Ready     string `json:"ready,omitempty"`
	Check     string `json:"check,omitempty"`
}

// MenuRows lists the shared menu; live marks each entry served or missing on provider's workspace.
func MenuRows(live bool, provider string) ([]MenuRow, error) {
	src := "default"
	if providers.OpencodeMenuFromVault() {
		src = "vault"
	}
	var served map[string]providers.ModelInfo
	var p *providers.Provider
	var cache map[string]serving.CheckResult
	if live {
		var err error
		if served, p, err = OpencodeServed(provider); err != nil {
			return nil, err
		}
		cache, _ = LoadCheckCache(p.Name)
	}
	var rows []MenuRow
	for _, m := range providers.OpencodeMenu() {
		r := MenuRow{ID: m.ID, Label: m.Label, Responses: m.Responses, Source: src}
		if live {
			r.Live = "missing"
			if info, ok := ServedInfo(served, p, m.ID); ok {
				r.Live, r.Ready = "served", info.Ready
			}
			r.Check = CheckCell(cache, m.ID)
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// MenuAddServed is the menu with arg (`id[=Label]`) added; unless force, the workspace must serve the id.
func MenuAddServed(arg string, responses, responsesSet, force bool, provider string) (id string, next []providers.OpencodeModel, err error) {
	id, label, _ := strings.Cut(arg, "=")
	id = MenuID(id)
	if id == "" {
		return "", nil, errors.New("empty model id")
	}
	if !force {
		served, p, err := OpencodeServed(provider)
		if err != nil {
			return "", nil, fmt.Errorf("cannot check %s against the workspace (--force skips): %v", id, err)
		}
		if _, ok := ServedInfo(served, p, id); !ok {
			return "", nil, fmt.Errorf("%s%s is not served by %s (--force skips the check)", p.ModelPrefix, id, p.Name)
		}
	}
	var r *bool
	if responsesSet {
		r = &responses
	}
	return id, MenuAdd(providers.OpencodeMenu(), id, strings.TrimSpace(label), r), nil
}

// MenuImportDiff parses file and returns the menu it describes plus what changes against the current one.
func MenuImportDiff(file string) (next []providers.OpencodeModel, changes []MenuChange, err error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, err
	}
	next, err = ParseMenuImport(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %v", file, err)
	}
	return next, DiffMenu(providers.OpencodeMenu(), next), nil
}

// CheckCell renders a cached probe as "ok tools", "ok no-tools", "fail 400", "missing" or "-".
func CheckCell(cache map[string]serving.CheckResult, id string) string {
	r, ok := cache[id]
	switch {
	case !ok:
		return "-"
	case r.Status == "ok" && r.Tools == "no":
		return "ok no-tools"
	case r.Status == "ok":
		return "ok tools"
	case r.Status == "fail" && r.HTTP > 0:
		return fmt.Sprintf("fail %d", r.HTTP)
	}
	return r.Status
}

// MenuCheck probes the chosen endpoints and caches the results; cacheErr is a cache write failure, not a probe one.
func MenuCheck(ids []string, all bool, provider string, timeout time.Duration) (results []serving.CheckResult, cacheErr, err error) {
	served, p, err := OpencodeServed(provider)
	if err != nil {
		return nil, nil, err
	}
	targets := OpencodeCheckTargets(ids, all, served, p)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	results, err = serving.Probe(ctx, p, targets, serving.CheckOptions{Concurrency: 4, Timeout: timeout})
	if err != nil && results == nil {
		return nil, nil, err
	}
	SortCheckResults(results)
	cacheErr = SaveCheckCache(CheckCache{CheckedAt: time.Now().UTC(), Provider: p.Name, Results: results})
	return results, cacheErr, nil
}

// SyncOptions are `opencode catalog sync`'s switches.
type SyncOptions struct {
	Apply   bool
	Refresh bool
	Verify  bool
}

// SyncPlan is a discovery run merged into the shared menu; nothing is written until Seal and the caller's menu write.
type SyncPlan struct {
	Provider *providers.Provider
	Snapshot serving.Snapshot
	IDs      []string // eligible serving ids
	Menu     []providers.OpencodeModel
	Next     []providers.OpencodeModel
	Skipped  int // additions dropped because their probe failed
	Changes  []MenuChange
	Verified bool
}

// MenuSync discovers profile's serving models and merges absent ids in; existing order, labels and Responses stay.
func MenuSync(profile string, opts SyncOptions) (SyncPlan, error) {
	p, snapshot, err := OpencodeSyncModels(profile, opts.Refresh)
	if err != nil {
		return SyncPlan{}, err
	}
	ids := OpencodeSyncIDs(snapshot.Models)
	menu := providers.OpencodeMenu()
	if opts.Verify {
		if err := OpencodeSyncVerify(p, &snapshot, ids, menu); err != nil {
			return SyncPlan{}, err
		}
	}
	next, skipped := OpencodeSyncMerge(menu, ids, snapshot.Checks)
	return SyncPlan{Provider: p, Snapshot: snapshot, IDs: ids, Menu: menu, Next: next, Skipped: skipped,
		Changes: DiffMenu(menu, next), Verified: opts.Verify}, nil
}

// Seal stores the discovery snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func (pl SyncPlan) Seal() (sealed bool, err error) {
	if pl.Snapshot.CacheHit && !pl.Verified {
		return false, nil
	}
	if err := serving.Save(pl.Provider, pl.Snapshot); err != nil {
		return false, err
	}
	return true, nil
}

// SyncSummaryLines are the discovery and verification lines that head a sync report.
func SyncSummaryLines(snapshot serving.Snapshot, verified bool) []string {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	lines := []string{fmt.Sprintf("Valid identity (not endpoint permissions); %s, discovered %s",
		source, snapshot.DiscoveredAt.Format(time.RFC3339))}
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
		return append(lines, "Verification: listing only; models are discovered/unverified")
	}
	return append(lines,
		fmt.Sprintf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted",
			label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools),
		"Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}
