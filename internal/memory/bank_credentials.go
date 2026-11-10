package memory

import (
	"regexp"

	"github.com/edotau/claude-code/internal/shapes"
)

// credentialPattern is a coarse credential-shaped tripwire, not a full secrets scanner.
var credentialPattern = regexp.MustCompile(
	`sk-[A-Za-z0-9]{10,}|` + shapes.GitHubPattern(10) + `|dapi[0-9a-f]{10,}|AKIA[A-Z0-9]{10,}|-----BEGIN`)
