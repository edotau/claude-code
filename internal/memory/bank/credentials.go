package bank

import (
	"github.com/edotau/claude-code/internal/shapes"
	"regexp"
)

// CredentialPattern is a coarse credential-shaped tripwire, not a full secrets scanner.
var CredentialPattern = regexp.MustCompile(
	`sk-[A-Za-z0-9]{10,}|` + shapes.GitHubPattern(10) + `|dapi[0-9a-f]{10,}|AKIA[A-Z0-9]{10,}|-----BEGIN`)
