package memory

import "testing"

// credentialPattern is the coarse memory tripwire: ghs_ with 10+ body characters is caught, a short stub is not.
func TestCredentialPatternGitHubShapes(t *testing.T) {
	if !credentialPattern.MatchString("token ghs_abcdefghij12 leaked") {
		t.Error("ghs_ + 12 chars must match")
	}
	if credentialPattern.MatchString("see ghs_abc for the format") {
		t.Error("ghs_abc is too short to be a token")
	}
}
