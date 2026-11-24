package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/paths"
)

// SpentBy is c's spend over the dispatch records of its subtree, a re-assigned holder's included.
func SpentBy(c contract.Contract) contract.Spent {
	return contract.SpentOf(c, DispatchUses(c.ID))
}

// budgetRecord is the part of a dispatch record the budget sums; the rest of the record is never decoded.
type budgetRecord struct {
	ID       string `json:"id"`
	Parent   string `json:"parent"`
	Usage    *Usage `json:"usage"`
	Activity *struct {
		Usage Usage `json:"usage"`
	} `json:"activity"`
}

// DispatchUses reads the dispatch records stamped no earlier than root, read-only: none is reaped or rewritten.
func DispatchUses(root string) []contract.Use {
	stamp, _, _ := strings.Cut(root, "-") // Start makes the contract id a dispatch id: its name starts with its stamp
	dir := paths.DispatchDir()
	ents, _ := os.ReadDir(dir) // no dir: nothing spent
	var out []contract.Use
	for _, e := range slices.Backward(ents) {
		if e.Name() < stamp {
			break // started before the root: no subtree member
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var r budgetRecord
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && json.Unmarshal(b, &r) == nil {
			out = append(out, contract.Use{ID: r.ID, Parent: r.Parent, Tokens: r.tokens()})
		}
	}
	return out
}

// tokens is the record's final usage, else (still running) its live usage so far.
func (r budgetRecord) tokens() int {
	u := r.Usage
	if u == nil && r.Activity != nil {
		u = &r.Activity.Usage
	}
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
}
