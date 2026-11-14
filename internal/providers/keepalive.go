package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// keepaliveMargin is how little life a token may have left before Keepalive refreshes it.
const keepaliveMargin = 15 * time.Minute

// Keepalive refreshes the tokens p's next request would use when under keepaliveMargin remains on one: an oauth row's
// user login (a dead one only after its retry window, as one probe) and paired SP, an m2m row's token. It makes no
// network call and takes no lock when nothing is due; refreshed reports a token endpoint exchange by this call.
func Keepalive(ctx context.Context, p *Provider) (refreshed bool, err error) {
	switch p.Auth.Type {
	case AuthM2M:
		return keepaliveM2M(ctx, p)
	case AuthOAuth:
		user, userErr := keepaliveUser(ctx, p)
		sp, spErr := keepaliveSP(ctx, p)
		return user || sp, errors.Join(userErr, spErr)
	}
	return false, nil
}

// tokenDue reports whether tok has under keepaliveMargin left: by its JWT exp, else by the expiry record under record,
// and a token with neither is due.
func tokenDue(tok, record string) bool {
	if exp, ok := JWTExp(tok); ok {
		return time.Until(time.Unix(exp, 0)) < keepaliveMargin
	}
	exp, ok := TokenExpiry(record)
	return !ok || time.Until(exp) < keepaliveMargin
}

// keepaliveUser refreshes a stored user login that is due; a dead mark inside its window is left alone.
func keepaliveUser(ctx context.Context, p *Provider) (bool, error) {
	if !hasUserLogin(p) {
		return false, nil
	}
	if rec, dead := readLoginMark(p.Name); dead {
		if time.Now().Before(rec.Until) {
			return false, nil
		}
		_, stillDead, err := ProbeUserLogin(ctx, p) // a login renewed in another shell clears the mark
		return err == nil && !stillDead, err
	}
	ov, err := readOAuth(p)
	if err != nil {
		return false, fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	if !tokenDue(strings.TrimSpace(ov.Access), p.Name) {
		return false, nil
	}
	_, minted, err := lockedUserMint(ctx, p, false, keepaliveMargin)
	if Verdict(err).Condemns() { // the dead login is recorded; LoginStatus reports it
		return false, nil
	}
	return minted, err
}

// keepaliveSP refreshes the paired SP's token when it is due.
func keepaliveSP(ctx context.Context, p *Provider) (bool, error) {
	if !SPPresent(p) {
		return false, nil
	}
	_, secret, access, err := spSecrets(p)
	if err != nil {
		return false, err
	}
	if !tokenDue(clientAccessToken(access, clientBind(AuthHost(p), p.Auth.SPClientID, secret)), p.Name+spExpirySuffix) {
		return false, nil
	}
	_, minted, err := mintSP(ctx, p, false, keepaliveMargin)
	return minted, err
}

// keepaliveM2M refreshes an m2m row's token when it is due, re-checking the sealed slot under the refresh lock.
func keepaliveM2M(ctx context.Context, p *Provider) (bool, error) {
	name := AuthSecretName(p)
	secret, access, err := clientSecrets(name)
	if err != nil {
		return false, fmt.Errorf("%s: vault: %w", p.Name, err)
	}
	bind := clientBind(AuthHost(p), p.Auth.ClientID, secret)
	if secret == "" || !tokenDue(clientAccessToken(access, bind), p.Name) {
		return false, nil
	}
	minted := false
	err = withRefreshLock(p, func() error {
		if v, ok := sealedClientAccess(name+"_ACCESS", bind, keepaliveMargin); ok { // a peer refreshed meanwhile
			return finishCredential(p, v, time.Time{})
		}
		_, _, err := mintAndCache(ctx, p, true)
		minted = err == nil
		return err
	})
	return minted, err
}
