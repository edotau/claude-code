package configure

import (
	"github.com/edotau/claude-code/internal/ui/tui"
	"regexp"
	"strings"
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

// keyRows renders one picker row per credential (name, then mode and check aligned) and reports whether every
// credential is present and accepted.
func keyRows(targets []keyTarget) ([][2]string, bool) {
	names := make([]string, len(targets))
	cols := make([][2]string, len(targets))
	allOK := true
	for i, kt := range targets {
		names[i] = kt.label
		cols[i] = keyCols(kt)
		if cols[i][1] != "verified" {
			allOK = false
		}
	}
	return withChecks(names, cols), allOK
}

// withChecks lays out name rows whose detail is two aligned columns: the mode, then the check ("verified" is green).
func withChecks(names []string, cols [][2]string) [][2]string {
	width := 0
	for _, c := range cols {
		width = max(width, len([]rune(c[0])))
	}
	rows := make([][2]string, len(names))
	for i, c := range cols {
		pad := strings.Repeat(" ", width-len([]rune(c[0])))
		rows[i] = [2]string{names[i], strings.TrimRight(c[0]+pad+"  "+c[1], " ")}
	}
	return rows
}

// keyCols is one credential's mode (how it signs in · how it bills) and check: verified, unverified, rejected or
// missing. A login counts as verified when its own CLI's status check confirmed it.
func keyCols(kt keyTarget) [2]string {
	if !kt.present {
		return [2]string{"not set up", "not verified"}
	}
	if kt.provider != nil && kt.envVar != "" {
		status, _ := probe(kt)
		if status == "verified" {
			return [2]string{modeAPIKey, "verified"}
		}
		return [2]string{modeAPIKey, "not verified"}
	}
	return [2]string{loginLine(kt), "verified"}
}

var tierRE = regexp.MustCompile(`\((\w+)\)`)

// loginLine is a login credential's mode: "subscription", with the tier when the CLI reports one ("subscription (max)").
func loginLine(kt keyTarget) string {
	if kt.label == "claude" {
		if m := tierRE.FindStringSubmatch(kt.source); m != nil {
			return "subscription (" + m[1] + ")"
		}
	}
	return "subscription"
}

// changeKey runs the picked credential's actions: rotate when present, set up when absent.
func changeKey(d *dialog, kt keyTarget) bool {
	if !kt.present {
		if !actionConfirmed("Set up " + kt.label) {
			return false
		}
		return resolveKey(d, kt)
	}
	if len(rotateOptions(kt)) == 0 {
		d.printf("%-10s nothing to rotate here: %s\n", kt.label, kt.source)
		return false
	}
	return runRotate(d, kt)
}
