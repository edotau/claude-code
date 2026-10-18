package configure

import (
	"github.com/edotau/claude-code/internal/tui"
)

// pickKey is the terminal picker seam; tests replace it.
var pickKey = tui.PickBack

// keysMenu lists every credential with its status; picking one opens its Login / OAuth token / API key actions
// (or the set-up flow when absent), then returns to the list. Back leaves. Nothing changes unless a row is picked.
func keysMenu(d *dialog) (allOK, changed bool) {
	start := 0
	for {
		targets, err := keyTargets(d.reg, d.o.Vendor)
		if err != nil {
			d.println(err)
			return false, changed
		}
		rows, ok := keyRows(targets)
		idx, err := pickKey("keys — pick a credential to change, or Back to leave", rows, start)
		if err != nil || idx < 0 {
			return ok, changed
		}
		start = idx
		if changeKey(d, targets[idx]) {
			changed = true
		}
	}
}

// keyRows renders one picker row per credential and reports whether every one is present and accepted.
func keyRows(targets []keyTarget) ([][2]string, bool) {
	rows := make([][2]string, len(targets))
	allOK := true
	for i, kt := range targets {
		rows[i] = [2]string{kt.label, keyStatus(kt)}
		if !kt.present || rows[i][1] == "rejected · pick to fix" {
			allOK = false
		}
	}
	return rows, allOK
}

// keyStatus is the row's right-hand column: where the credential lives and whether a live probe accepts it.
func keyStatus(kt keyTarget) string {
	if !kt.present {
		return "missing · pick to set up"
	}
	status, _ := probe(kt)
	switch status {
	case "rejected":
		return "rejected · pick to fix"
	case "", "login":
		return kt.source
	}
	return kt.source + " · " + status
}

// changeKey runs the picked credential's actions: rotate when present, set up when absent.
func changeKey(d *dialog, kt keyTarget) bool {
	if !kt.present {
		return resolveKey(d, kt)
	}
	if len(rotateOptions(kt)) == 0 {
		d.printf("%-10s nothing to rotate here: %s\n", kt.label, kt.source)
		return false
	}
	return runRotate(d, kt)
}
