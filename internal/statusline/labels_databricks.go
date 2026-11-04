package statusline

import "github.com/edotau/claude-code/internal/providers"

// databricksLabeler drops the provider cell: the model id and the u2m/m2m countdown carry everything.
var databricksLabeler = labeler{provider: func(*providers.Provider) string { return "" }, model: modelSegment, auth: tokenAuth}
