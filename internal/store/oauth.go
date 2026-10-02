package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Lifetimes of what hakobu's OAuth server hands out. A refresh token is
// replaced at each use, so a client used at least monthly stays connected.
const (
	OAuthCodeTTL    = 2 * time.Minute
	OAuthAccessTTL  = time.Hour
	OAuthRefreshTTL = 30 * 24 * time.Hour

	// oauthRetryGrace is how soon after its first use a code or refresh
	// token presented again is taken for the client's own retry (it lost
	// the response) rather than for someone else's copy.
	oauthRetryGrace = time.Minute
)

// ErrOAuthToken is any code or token that can't be used: unknown, expired,
// of another kind or exchanged before.
var ErrOAuthToken = errors.New("invalid or expired token")

var oauthPrefix = map[string]string{"code": "hakobu_code_", "access": "hakobu_at_", "refresh": "hakobu_rt_"}

// NewOAuthToken issues a code ("code", with the PKCE challenge it must be
// redeemed with), access or refresh token for a grant. Like sessions, only
// its SHA-256 is stored.
func (s *Store) NewOAuthToken(ctx context.Context, grantID int64, kind, challenge string, ttl time.Duration) (token string, expires time.Time, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	token = oauthPrefix[kind] + hex.EncodeToString(b)
	expires = time.Now().Add(ttl)
	return token, expires, s.CreateOAuthToken(ctx, CreateOAuthTokenParams{
		ID: sessionID(token), GrantID: grantID, Kind: kind, CodeChallenge: challenge, ExpiresAt: timestamp(expires),
	})
}

// RedeemOAuthToken takes a code or refresh token for its grant, once. One
// presented again after the retry grace revokes the grant: whoever has the
// copy, the client and the thief are both cut off until the owner approves
// the client again.
func (s *Store) RedeemOAuthToken(ctx context.Context, token, kind string) (OAuthToken, OAuthGrant, error) {
	t, err := s.GetOAuthToken(ctx, sessionID(token))
	if errors.Is(err, sql.ErrNoRows) || err == nil && (t.Kind != kind || t.ExpiresAt < timestamp(time.Now())) {
		return t, OAuthGrant{}, ErrOAuthToken
	}
	if err != nil {
		return t, OAuthGrant{}, err
	}
	n, err := s.UseOAuthToken(ctx, UseOAuthTokenParams{ID: t.ID, UsedAt: timestamp(time.Now())})
	if err != nil {
		return t, OAuthGrant{}, err
	}
	if n == 0 {
		if usedAt, err := time.Parse(time.RFC3339, t.UsedAt); err == nil && time.Since(usedAt) > oauthRetryGrace {
			if err := s.DeleteOAuthGrant(ctx, t.GrantID); err != nil {
				return t, OAuthGrant{}, err
			}
		}
		return t, OAuthGrant{}, ErrOAuthToken
	}
	g, err := s.GetOAuthGrant(ctx, t.GrantID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, g, ErrOAuthToken
	}
	return t, g, err
}

// OAuthAccess returns the grant of a live access token and when the token
// expires, and notes the grant used.
func (s *Store) OAuthAccess(ctx context.Context, token string) (OAuthGrant, time.Time, error) {
	t, err := s.GetOAuthToken(ctx, sessionID(token))
	if errors.Is(err, sql.ErrNoRows) || err == nil && (t.Kind != "access" || t.ExpiresAt < timestamp(time.Now())) {
		return OAuthGrant{}, time.Time{}, ErrOAuthToken
	}
	if err != nil {
		return OAuthGrant{}, time.Time{}, err
	}
	expires, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err != nil {
		return OAuthGrant{}, time.Time{}, err
	}
	g, err := s.GetOAuthGrant(ctx, t.GrantID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, expires, ErrOAuthToken
	}
	if err != nil {
		return g, expires, err
	}
	_ = s.TouchOAuthGrant(ctx, TouchOAuthGrantParams{ID: g.ID, LastUsedAt: timestamp(time.Now())})
	return g, expires, nil
}

// LiveOAuthGrants lists the grants a client can still use.
func (s *Store) LiveOAuthGrants(ctx context.Context) ([]OAuthGrant, error) {
	return s.ListOAuthGrants(ctx, timestamp(time.Now()))
}

// pruneOAuth deletes expired codes and tokens, grants left with none, and
// registered clients nobody approved within a day.
func (s *Store) pruneOAuth(ctx context.Context) error {
	now := timestamp(time.Now())
	if err := s.PruneOAuthTokens(ctx, now); err != nil {
		return err
	}
	if err := s.PruneOAuthGrants(ctx, now); err != nil {
		return err
	}
	return s.PruneOAuthClients(ctx, timestamp(time.Now().Add(-24*time.Hour)))
}
