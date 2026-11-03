package settings

import "slices"

// HookEvents derives from the hook chain the registry declares, in declaration order, deduplicated.
func HookEvents() []string {
	var out []string
	for _, s := range Specs() {
		if !slices.Contains(out, s.Event) {
			out = append(out, s.Event)
		}
	}
	return out
}
