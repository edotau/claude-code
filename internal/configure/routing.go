// routing.go — the "routing" page: every internal/agent vendor module's fields, grouped by vendor, edited
// and held until the page is left, then written to the providers overlay's routing object via
// providers.UpdateRouting. Mirrors the "claude code" page's editing shape (claude_settings.go).
package configure

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/ui/tui"
)

// routingRow is the main-menu "routing" row: N overrides from default, or "vault error" on a load failure.
func routingRow(d *dialog) homeRow {
	overlay, err := providers.RoutingOverlay()
	detail := "vault error"
	if err == nil {
		detail = fmt.Sprintf("%d overrides", countRoutingOverrides(overlay))
	}
	return homeRow{"routing", detail, "price weight, subagents, opencode sub-pick", func(d *dialog) { runRouting(d) }}
}

// countRoutingOverrides is how many fields the vault overlay itself sets, across every module.
func countRoutingOverrides(overlay providers.Routing) int {
	n := 0
	for _, m := range agent.Modules() {
		for _, f := range m.Fields() {
			if f.IsSet(overlay) {
				n++
			}
		}
	}
	return n
}

// routingSource reports "vault" when the overlay itself sets f, else "default" — derived from presence
// in the overlay (providers.RoutingOverlay), never from comparing f's resolved value to the default.
func routingSource(f agent.Field, overlay providers.Routing) string {
	if f.IsSet(overlay) {
		return "vault"
	}
	return "default"
}

// runRouting is the "routing" page: interactive on a terminal (edits staged until the page is left, then
// one confirm writes the overlay); otherwise a read-only report, the same gate every other section uses.
func runRouting(d *dialog) bool {
	reg, err := providers.Load()
	if err != nil {
		d.printf("routing: %v\n", err)
		return false
	}
	overlay, err := providers.RoutingOverlay()
	if err != nil {
		d.printf("routing: %v\n", err)
		return false
	}
	if d.o.Interactive && !d.o.Check && !d.o.DryRun && isStdinTTY() {
		return runRoutingMenu(d, reg.Routing, overlay)
	}
	return reportRouting(d, reg.Routing, overlay)
}

// reportRouting prints one line per field under its module's header: key, effective value, source. The
// core module (Vendor() "routing") skips its own header — Run's section header already reads "routing".
func reportRouting(d *dialog, r, overlay providers.Routing) bool {
	for _, m := range agent.Modules() {
		fields := m.Fields()
		if len(fields) == 0 {
			continue
		}
		if m.Vendor() != "routing" {
			d.println("== " + m.Vendor() + " ==")
		}
		for _, f := range fields {
			d.println(f.Key + "  " + f.Get(r) + "  " + routingSource(f, overlay))
		}
	}
	return true
}

// runRoutingMenu is the interactive page: rows grouped by module, edits staged until the page is left
// (Esc or the trailing "save and go back" row), then one confirm writes the overlay.
func runRoutingMenu(d *dialog, r, overlay providers.Routing) bool {
	staged := map[string]string{}  // field key -> new typed value, this page
	resetKeys := map[string]bool{} // field key -> explicit reset to default, this page
	defer saveRouting(d, staged, resetKeys)
	start := 0
	for {
		var sections []tui.Section
		var fields []agent.Field
		for _, m := range agent.Modules() {
			var rows [][2]string
			for _, f := range m.Fields() {
				val, source := f.Get(r), routingSource(f, overlay)
				switch {
				case resetKeys[f.Key]:
					val, source = f.Get(providers.DefaultRouting()), "default"
				case staged[f.Key] != "":
					val, source = staged[f.Key], "vault"
				}
				rows = append(rows, [2]string{f.Key, val + "  " + source})
				fields = append(fields, f)
			}
			if len(rows) > 0 {
				sections = append(sections, tui.Section{Title: "== " + m.Vendor() + " ==", Rows: rows})
			}
		}
		idx, err := pickSettingsPage("routing", sections, enterRow("save and go back"), start)
		if err != nil || idx < 0 || idx >= len(fields) {
			return true
		}
		start = idx
		f := fields[idx]
		input, ok := editRoutingField(d, f)
		if !ok {
			continue
		}
		stageRoutingField(staged, resetKeys, f, input)
	}
}

// stageRoutingField records one edit: "" (explicit reset) marks the key reset; otherwise it stages the
// typed input string, passed verbatim to f.Set on save.
func stageRoutingField(staged map[string]string, resetKeys map[string]bool, f agent.Field, input string) {
	delete(staged, f.Key)
	delete(resetKeys, f.Key)
	if input == "" {
		resetKeys[f.Key] = true
		return
	}
	staged[f.Key] = input
}

// editRoutingField prompts for a new value: a picker for Bool/Enum, a text line otherwise. Returns the raw
// string to stage ("" means reset); ok is false on cancel or an invalid value (already reported).
func editRoutingField(d *dialog, f agent.Field) (string, bool) {
	switch f.Kind {
	case agent.KindBool:
		return editRoutingChoice(d, f, []string{"true", "false"})
	case agent.KindEnum:
		return editRoutingChoice(d, f, f.Values)
	default:
		return editRoutingText(d, f)
	}
}

// editRoutingChoice is the on/off or enum picker, with a trailing "reset to default" row.
func editRoutingChoice(d *dialog, f agent.Field, values []string) (string, bool) {
	rows := make([][2]string, 0, len(values)+1)
	for _, v := range values {
		rows = append(rows, [2]string{v, ""})
	}
	resetIdx := len(rows)
	rows = append(rows, [2]string{"reset to default (" + f.Get(providers.DefaultRouting()) + ")", ""})
	idx, err := pickPlain(d, f.Key, rows)
	if err != nil || idx < 0 {
		return "", false
	}
	if idx == resetIdx {
		return "", true
	}
	if err := validateRoutingInput(f, values[idx]); err != nil {
		d.printf("%s: %v\n", f.Key, err)
		return "", false
	}
	return values[idx], true
}

// editRoutingText is the number/text/list prompt; an empty line resets to the default.
func editRoutingText(d *dialog, f agent.Field) (string, bool) {
	input := strings.TrimSpace(textLine(d, f.Key+" (empty resets to default): "))
	if input == "" {
		return "", true
	}
	if err := validateRoutingInput(f, input); err != nil {
		d.printf("%s: %v\n", f.Key, err)
		return "", false
	}
	return input, true
}

// validateRoutingInput runs f.Set against a scratch Routing, so a bad value is reported before staging.
func validateRoutingInput(f agent.Field, input string) error {
	var scratch providers.Routing
	return f.Set(&scratch, input)
}

// saveRouting asks to save n staged change(s) on leaving the page; n/Esc discards them. Every module's
// fields are applied to the overlay's own routing object inside one providers.UpdateRouting transaction, so
// a reset truly removes the field (falls back to defaults.json) rather than writing the default value.
func saveRouting(d *dialog, staged map[string]string, resetKeys map[string]bool) {
	n := len(staged) + len(resetKeys)
	if n == 0 {
		return
	}
	if !confirmLeave(n, "routing setting(s)") {
		d.printf("routing: %d change(s) discarded\n", n)
		return
	}
	err := providers.UpdateRouting(func(r *providers.Routing) error {
		for _, m := range agent.Modules() {
			for _, f := range m.Fields() {
				switch {
				case resetKeys[f.Key]:
					if err := f.Set(r, ""); err != nil {
						return err
					}
				case staged[f.Key] != "":
					if err := f.Set(r, staged[f.Key]); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		d.printf("routing: %v\n", err)
		return
	}
	d.changed = true
	d.printf("routing: %d setting(s) saved\n", n)
}
