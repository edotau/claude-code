//go:build unix && !linux && !darwin

package proc

// tagged has no env source here: none, so cancel falls back to the descendant walk.
func tagged(string) ([]int, error) { return nil, nil }

func hasTag(int, string) bool { return false }
