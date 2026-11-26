package route

import "slices"

// Fence is the profile set a dispatch may ever touch; Open means unconfined.
type Fence struct {
	Profiles []string `json:"profiles,omitempty"`
	Open     bool     `json:"open,omitempty"`
}

// Allows reports profile inside f.
func (f Fence) Allows(profile string) bool { return f.Open || slices.Contains(f.Profiles, profile) }

// Narrow is f held to child too: a nested fence never widens its parent's.
func (f Fence) Narrow(child Fence) Fence {
	switch {
	case child.Open:
		return f
	case f.Open:
		return Fence{Profiles: slices.Sorted(slices.Values(child.Profiles))}
	}
	out := slices.DeleteFunc(slices.Clone(child.Profiles), func(p string) bool { return !slices.Contains(f.Profiles, p) })
	return Fence{Profiles: slices.Sorted(slices.Values(out))}
}
