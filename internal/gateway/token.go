package gateway

import (
	"context"
	"errors"
)

var (
	tokenEnsure = Ensure
	tokenState  = Running
	tokenMint   = MintSessionToken
)

// Token ensures the daemon is running and mints a profile-scoped token bound to its PID.
func Token(ctx context.Context, profile string) (string, error) {
	if _, err := tokenEnsure(ctx); err != nil {
		return "", errors.New("proxy unavailable; run gateway proxy --start")
	}
	st, ok := tokenState(ctx)
	if !ok {
		return "", errors.New("proxy not running")
	}
	return tokenMint(ctx, profile, st.PID)
}
