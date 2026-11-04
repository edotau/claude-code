package statusline

import (
	"net/url"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

// databricksLabeler folds the credential into the model cell; no provider or separate auth cell.
var databricksLabeler = labeler{
	provider: func(*providers.Provider) string { return "" },
	model:    databricksModel,
	auth:     func(Input, string, *providers.Provider) string { return "" },
}

// databricksModel is "<model id> · <u2m|m2m>@<host id>[:<time left>]", e.g. "…opus-5-5 · u2m@dbx-host:50min".
func databricksModel(in Input, session string) string {
	p := rowOf(session)
	if p == nil {
		return modelSegment(in, session)
	}
	cred := credMethod(p) + "@" + hostID(p)
	if exp, ok := providers.TokenExpiry(session); ok {
		cred = tokenLeft(cred, exp, time.Now())
	}
	return modelSegment(in, session) + " · " + cred
}

// hostID is the workspace id from the host ("dbc-dbx-host.cloud…" → "dbx-host"); the profile name without one.
func hostID(p *providers.Provider) string {
	u, err := url.Parse(p.Host)
	if err != nil || u.Hostname() == "" {
		u, err = url.Parse(p.BaseURL)
	}
	if err != nil || u.Hostname() == "" {
		return p.Name
	}
	label, _, _ := strings.Cut(u.Hostname(), ".")
	return strings.TrimPrefix(label, "dbc-")
}
