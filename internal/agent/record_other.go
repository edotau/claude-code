//go:build !unix

package agent

// processGroup is always 0 where process groups are unavailable.
func processGroup(int) int { return 0 }
