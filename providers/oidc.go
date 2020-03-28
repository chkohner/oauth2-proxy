package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc"
	"golang.org/x/oauth2"

	"github.com/oauth2-proxy/oauth2-proxy/pkg/apis/sessions"
	"github.com/oauth2-proxy/oauth2-proxy/pkg/requests"
)

// OIDCProvider represents an OIDC based Identity Provider
type OIDCProvider struct {
	*ProviderData

	Verifier             *oidc.IDTokenVerifier
	AllowUnverifiedEmail bool
}

// NewOIDCProvider initiates a new OIDCProvider
func NewOIDCProvider(p *ProviderData) *OIDCProvider {
	p.ProviderName = "OpenID Connect"
	return &OIDCProvider{ProviderData: p}
}

// Redeem exchanges the OAuth2 authentication token for an ID token
func (p *OIDCProvider) Redeem(redirectURL, code string) (s *sessions.SessionState, err error) {
	clientSecret, err := p.GetClientSecret()
	if err != nil {
		return
	}

	ctx := context.Background()
	c := oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL: p.RedeemURL.String(),
		},
		RedirectURL: redirectURL,
	}
	token, err := c.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %v", err)
	}

	// in the initial exchange the id token is mandatory
	idToken, err := p.findVerifiedIDToken(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("could not verify id_token: %v", err)
	} else if idToken == nil {
		return nil, fmt.Errorf("token response did not contain an id_token")
	}

	s, err = p.createSessionState(token, idToken)
	if err != nil {
		return nil, fmt.Errorf("unable to update session: %v", err)
	}

	return
}

// RefreshSessionIfNeeded checks if the session has expired and uses the
// RefreshToken to fetch a new Access Token (and optional ID token) if required
func (p *OIDCProvider) RefreshSessionIfNeeded(s *sessions.SessionState) (bool, error) {
	if s == nil || s.ExpiresOn.After(time.Now()) || s.RefreshToken == "" {
		return false, nil
	}

	err := p.redeemRefreshToken(s)
	if err != nil {
		return false, fmt.Errorf("unable to redeem refresh token: %v", err)
	}

	fmt.Printf("refreshed access token %s (expired on %s)\n", s, s.ExpiresOn)
	return true, nil
}

func (p *OIDCProvider) redeemRefreshToken(s *sessions.SessionState) (err error) {
	clientSecret, err := p.GetClientSecret()
	if err != nil {
		return
	}

	c := oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: clientSecret,
		Endpoint: oauth2.Endpoint{
			TokenURL: p.RedeemURL.String(),
		},
	}
	ctx := context.Background()
	t := &oauth2.Token{
		RefreshToken: s.RefreshToken,
		Expiry:       time.Now().Add(-time.Hour),
	}
	token, err := c.TokenSource(ctx, t).Token()
	if err != nil {
		return fmt.Errorf("failed to get token: %v", err)
	}

	// in the token refresh response the id_token is optional
	idToken, err := p.findVerifiedIDToken(ctx, token)
	if err != nil {
		return fmt.Errorf("unable to extract id_token from response: %v", err)
	}

	newSession, err := p.createSessionState(token, idToken)
	if err != nil {
		return fmt.Errorf("unable create new session state from response: %v", err)
	}

	// It's possible that if the refresh token isn't in the token response the session will not contain an id token
	// if it doesn't it's probably better to retain the old one
	if newSession.IDToken != "" {
		s.IDToken = newSession.IDToken
		s.Email = newSession.Email
		s.User = newSession.User
		s.PreferredUsername = newSession.PreferredUsername
	}

	s.AccessToken = newSession.AccessToken
	s.RefreshToken = newSession.RefreshToken
	s.CreatedAt = newSession.CreatedAt
	s.ExpiresOn = newSession.ExpiresOn

	return
}

func (p *OIDCProvider) findVerifiedIDToken(ctx context.Context, token *oauth2.Token) (*oidc.IDToken, error) {

	getIDToken := func() (string, bool) {
		rawIDToken, _ := token.Extra("id_token").(string)
		return rawIDToken, len(strings.TrimSpace(rawIDToken)) > 0
	}

	if rawIDToken, present := getIDToken(); present {
		verifiedIDToken, err := p.Verifier.Verify(ctx, rawIDToken)
		return verifiedIDToken, err
	}
	return nil, nil
}

func (p *OIDCProvider) createSessionState(token *oauth2.Token, idToken *oidc.IDToken) (*sessions.SessionState, error) {

	newSession := &sessions.SessionState{}

	if idToken != nil {
		claims, err := findClaimsFromIDToken(idToken, token.AccessToken, p.ProfileURL.String())
		if err != nil {
			return nil, fmt.Errorf("couldn't extract claims from id_token (%e)", err)
		}

		if claims != nil {

			if !p.AllowUnverifiedEmail && claims.EmailVerified != nil && !*claims.EmailVerified {
				return nil, fmt.Errorf("email in id_token (%s) isn't verified", claims.Email)
			}

			newSession.IDToken = token.Extra("id_token").(string)
			newSession.Email = claims.Email
			newSession.User = claims.Subject
			newSession.PreferredUsername = claims.PreferredUsername
			newSession.SetRawClaims(claims.rawClaims)
		}
	}

	newSession.AccessToken = token.AccessToken
	newSession.RefreshToken = token.RefreshToken
	newSession.CreatedAt = time.Now()
	newSession.ExpiresOn = token.Expiry
	return newSession, nil
}

// ValidateSessionState checks that the session's IDToken is still valid
func (p *OIDCProvider) ValidateSessionState(s *sessions.SessionState) bool {
	ctx := context.Background()
	if idToken, err := p.Verifier.Verify(ctx, s.IDToken); err != nil {
		return false
	} else if p.ExtractRawClaims {
		// Stash these since the proxy is going to need them...
		s.SetRawClaimsFromIDToken(idToken)
	}

	return true
}

func getOIDCHeader(accessToken string) http.Header {
	header := make(http.Header)
	header.Set("Accept", "application/json")
	header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	return header
}

func findClaimsFromIDToken(idToken *oidc.IDToken, accessToken string, profileURL string) (*OIDCClaims, error) {

	claims := &OIDCClaims{}

	if err := idToken.Claims(&claims.rawClaims); err != nil {
		return nil, fmt.Errorf("failed to parse id_token claims: %v", err)
	}

	// TODO: Implement Aggregated and/or Distributed claims fetching...
	// <https://openid.net/specs/openid-connect-core-1_0.html#AggregatedDistributedClaims>
	// This is necessary if, for example, the identity is a part of so many groups that it can't fit
	// them into a normal claims assertion. Unfortunately, it's entirely possible that the OAuth2
	// provider has a lot of these that we don't really care about, so when implementing this, we
	// probably want a config option like `OAUTH2_PROXY_FETCH_DISTRIBUTED_CLAIMS=groups` (or
	// whatever) so that if it comes back distributed, we have the extra hint that it's actually
	// worth the time spent fetching it before running validation. If the validator doesn't care,
	// then we don't really care either.

	// Extract custom claims manually (to avoid parsing twice)

	if value, ok := claims.rawClaims["sub"].(string); ok {
		claims.Subject = value
	}
	if value, ok := claims.rawClaims["email"].(string); ok {
		claims.Email = value
	}
	if value, ok := claims.rawClaims["email_verified"].(*bool); ok {
		claims.EmailVerified = value
	}
	if value, ok := claims.rawClaims["preferred_username"].(string); ok {
		claims.PreferredUsername = value
	}

	if claims.Email == "" {
		if profileURL == "" {
			return nil, fmt.Errorf("id_token did not contain an email")
		}

		// If the userinfo endpoint profileURL is defined, then there is a chance the userinfo
		// contents at the profileURL contains the email.
		// Make a query to the userinfo endpoint, and attempt to locate the email from there.

		req, err := http.NewRequest("GET", profileURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header = getOIDCHeader(accessToken)

		respJSON, err := requests.Request(req)
		if err != nil {
			return nil, err
		}

		email, err := respJSON.Get("email").String()
		if err != nil {
			return nil, fmt.Errorf("neither id_token nor userinfo endpoint contained an email")
		}

		claims.Email = email
	}

	return claims, nil
}

type OIDCClaims struct {
	rawClaims         map[string]interface{}
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     *bool  `json:"email_verified"`
	PreferredUsername string `json:"preferred_username"`
}
