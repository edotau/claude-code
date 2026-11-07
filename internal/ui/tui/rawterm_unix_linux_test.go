//go:build linux

package tui

import "testing"

// TestFeedPasswordSwallowsSS3Arrow: ESC O A (application-mode Up) must not leak "OA" into the secret.
func TestFeedPasswordSwallowsSS3Arrow(t *testing.T) {
	var esc escState
	line, done, err := feedPassword(nil, []byte("ab\x1bOAc\n"), &esc)
	if err != nil || !done || string(line) != "abc" {
		t.Fatalf("feedPassword = %q, %v, %v; want abc, true, nil", line, done, err)
	}
}
