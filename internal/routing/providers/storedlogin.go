package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// loginSecretSuffix ends every oauth profile's login secret name (SecretName upper-cases the method).
var loginSecretSuffix = "_" + strings.ToUpper(AuthOAuth)

// LoginSecretProvider is the oauth row whose login secret is name, else nil; the suffix test only skips a load.
func LoginSecretProvider(name string) *Provider {
	if !strings.HasSuffix(name, loginSecretSuffix) {
		return nil
	}
	reg, err := Load()
	if err != nil {
		return nil
	}
	for _, p := range reg.Providers {
		if p.Auth.Type == AuthOAuth && AuthSecretName(p) == name {
			return p
		}
	}
	return nil
}

// CheckLoginRecord refuses a login value the reader cannot use: it must be a JSON record holding a refresh token.
func CheckLoginRecord(name, value string) error {
	var ov oauthValue
	if json.Unmarshal([]byte(strings.TrimSpace(value)), &ov) != nil || strings.TrimSpace(ov.Refresh) == "" {
		return fmt.Errorf(`%s holds a login record, not a bare token: store {"refresh":"<refresh token>"}`, name)
	}
	return nil
}

// VerifyStoredLogin tries p's just-stored login once under its refresh lock and returns the line to print.
func VerifyStoredLogin(ctx context.Context, p *Provider) string {
	tok, _, err := lockedUserMint(ctx, p, true, expiryMargin)
	switch {
	case err == nil:
		return fmt.Sprintf("%s: stored login verified and sealed%s", p.Name, expiresIn(tok))
	case Verdict(err).Condemns():
		return fmt.Sprintf("%s: stored the login, but the workspace rejected it (%s); sign in with `%s`", p.Name,
			Verdict(err), LoginCommand(p.Name))
	}
	clearLoginMark(p.Name)
	why := "the token endpoint could not be reached"
	if errors.Is(err, errRefreshLockTimeout) {
		why = "another refresh holds the lock"
	}
	return fmt.Sprintf("%s: stored the login; verification skipped (%s), the next request will try it", p.Name, why)
}

// expiresIn is " · token expires in Nm" for a JWT access token, else "".
func expiresIn(tok string) string {
	exp, ok := JWTExp(tok)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" · token expires in %dm", max(0, int(time.Until(time.Unix(exp, 0)).Minutes())))
}
