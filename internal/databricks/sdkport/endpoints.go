// Adapted from github.com/databricks/databricks-sdk-go v0.190.0 credentials/u2m/endpoint_supplier.go; modified.

package sdkport

import (
	"fmt"
	"regexp"
)

// Endpoints are the OAuth authorize and token URLs of one authorization server.
type Endpoints struct {
	Authorize, Token string
}

var accountIDRE = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// WorkspaceEndpoints are a workspace's fixed OIDC endpoints (what LoginU2M and RefreshU2M use).
func WorkspaceEndpoints(host string) (Endpoints, error) {
	h, err := normHost(host)
	if err != nil {
		return Endpoints{}, err
	}
	return Endpoints{Authorize: authorizeURL(h), Token: tokenURL(h)}, nil
}

// AccountEndpoints are the account-level OIDC endpoints, <accounts host>/oidc/accounts/<account_id>/v1/….
// Unused until account-level login lands; unified hosts (discovered via .well-known) are deferred.
func AccountEndpoints(accountsHost, accountID string) (Endpoints, error) {
	h, err := normHost(accountsHost)
	if err != nil {
		return Endpoints{}, err
	}
	if !accountIDRE.MatchString(accountID) {
		return Endpoints{}, fmt.Errorf("oauth: account id %q must match [0-9a-fA-F-]{8,64}", accountID)
	}
	base := fmt.Sprintf("%s/oidc/accounts/%s/v1", h, accountID)
	return Endpoints{Authorize: base + "/authorize", Token: base + "/token"}, nil
}
