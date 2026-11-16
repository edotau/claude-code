package usage

import (
	"context"
	"fmt"
	"io"

	"github.com/edotau/claude-code/internal/gemini"
)

// googleLimits is the agy /usage seam; tests stub it so no run reaches the binary.
var googleLimits = gemini.Quotas

type googleResult struct {
	rows []gemini.QuotaRow
	err  error
}

// startGoogleFetch reads agy's limits beside the local scan; empty at once when quotas are off.
func startGoogleFetch(enabled bool) <-chan googleResult {
	ch := make(chan googleResult, 1)
	if !enabled {
		ch <- googleResult{}
		return ch
	}
	go func() {
		rows, err := googleLimits(context.Background())
		ch <- googleResult{rows, err}
	}()
	return ch
}

// printGoogleLimits prints one line per Antigravity pool and per-model limit, or one line saying why none.
func printGoogleLimits(out io.Writer, g googleResult) {
	if g.err == nil && len(g.rows) == 0 {
		return
	}
	fmt.Fprintf(out, "\nGOOGLE LIMITS (agy /usage, local time)\n")
	if g.err != nil {
		fmt.Fprintln(out, g.err)
		return
	}
	for _, r := range g.rows {
		fmt.Fprintln(out, r.Line())
	}
}
