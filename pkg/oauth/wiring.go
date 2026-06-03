// Package oauth is the grafana-mcp adapter around
// github.com/BorisTyshkevich/go-mcp-oauth. It translates grafana-mcp's
// CLI/env configuration into mcp-oauth's Config, hands back the broker
// the caller mounts on its HTTP mux, and exposes the context-func that
// propagates a validated bearer through to the outbound Grafana request.
//
// We support forward mode only (matching altinity-mcp's antalya
// deployment shape). CIMD is the sole client-registration mechanism;
// /oauth/register returns HTTP 410 Gone via the broker. See the plan at
// .claude/plans/cimd-no-dcr-option-quirky-swing.md for context.
package oauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	mcpoauth "github.com/BorisTyshkevich/go-mcp-oauth"
	mcpgrafana "github.com/grafana/mcp-grafana"
)

// Settings is the operator-facing OAuth config surface. It mirrors the
// subset of mcpoauth.Config needed for forward-mode broker operation;
// fields not exposed here are either gating-mode specific or rely on the
// library's defaults.
type Settings struct {
	// Enabled toggles the entire OAuth stack. When false, NewBroker
	// returns (nil, nil) and the rest of grafana-mcp behaves as before.
	Enabled bool

	// Issuer is the upstream IdP issuer URL. JWKSURL, AuthURL and TokenURL
	// are discovered from it via /.well-known/openid-configuration unless
	// overridden explicitly.
	Issuer string

	// JWKSURL, AuthURL, TokenURL override the corresponding endpoints
	// when discovery cannot reach them (e.g., split-DNS deployments).
	JWKSURL  string
	AuthURL  string
	TokenURL string

	// Audience is the expected `aud` claim in inbound bearers. Set to the
	// canonical external URL the broker advertises in
	// /.well-known/oauth-protected-resource (RFC 8707 byte-equality).
	Audience string

	// ClientID and ClientSecret authenticate the broker to the upstream
	// IdP during /token redemption. Required in forward mode.
	ClientID     string
	ClientSecret string

	// SigningSecret is the HKDF master used to derive keys for the
	// stateless JWE artifacts the broker mints (pending-auth, auth-code).
	// Required in forward mode; must be >=32 bytes. Read from a file or
	// env var — never the command line.
	SigningSecret []byte

	// Scopes lists upstream scopes requested at /authorize.
	Scopes []string

	// RequiredScopes lists scopes the inbound bearer must carry to pass
	// validation. Empty means no scope gating.
	RequiredScopes []string

	// AllowedEmailDomains / AllowedHostedDomains restrict accepted
	// principals to specific domains. Empty means no domain restriction.
	AllowedEmailDomains  []string
	AllowedHostedDomains []string

	// PublicResourceURL and PublicAuthServerURL override the URLs the
	// broker advertises in its discovery metadata. Set these explicitly
	// when grafana-mcp runs behind a path-prefixing ingress.
	PublicResourceURL   string
	PublicAuthServerURL string

	// UpstreamOfflineAccess asks the upstream IdP for a refresh_token so
	// the broker can extend near-expired id_tokens at /token. Required
	// for sessions longer than the upstream id_token TTL.
	UpstreamOfflineAccess bool
}

// NewBroker constructs and validates an mcp-oauth broker from Settings.
// When s.Enabled is false it returns (nil, nil); callers must check the
// nil broker before mounting routes or wrapping middleware.
func NewBroker(s Settings) (*mcpoauth.Broker, error) {
	if !s.Enabled {
		return nil, nil
	}
	cfg := mcpoauth.Config{
		Mode:                  mcpoauth.ModeForward,
		Issuer:                strings.TrimSpace(s.Issuer),
		JWKSURL:               strings.TrimSpace(s.JWKSURL),
		AuthURL:               strings.TrimSpace(s.AuthURL),
		TokenURL:              strings.TrimSpace(s.TokenURL),
		Audience:              strings.TrimSpace(s.Audience),
		ClientID:              strings.TrimSpace(s.ClientID),
		ClientSecret:          s.ClientSecret,
		SigningSecret:         s.SigningSecret,
		Scopes:                s.Scopes,
		RequiredScopes:        s.RequiredScopes,
		AllowedEmailDomains:   s.AllowedEmailDomains,
		AllowedHostedDomains: s.AllowedHostedDomains,
		PublicResourceURL:     strings.TrimSpace(s.PublicResourceURL),
		PublicAuthServerURL:   strings.TrimSpace(s.PublicAuthServerURL),
		UpstreamOfflineAccess: s.UpstreamOfflineAccess,
	}
	broker, err := mcpoauth.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	return broker, nil
}

// PropagateIdentity copies the validated bearer from the request context
// (where mcp-oauth's Broker.Middleware put it) into the GrafanaConfig on
// the returned context, so that JWTAssertionRoundTripper picks it up
// when assembling the outbound Grafana request.
//
// This is an mcp-go HTTPContextFunc / SSEContextFunc — append it to the
// composed chain in cmd/mcp-grafana/main.go when OAuth is enabled.
func PropagateIdentity(ctx context.Context, r *http.Request) context.Context {
	raw, ok := mcpoauth.RawTokenFromContext(r.Context())
	if !ok || raw == "" {
		return ctx
	}
	cfg := mcpgrafana.GrafanaConfigFromContext(ctx)
	cfg.JWTAssertion = raw
	return mcpgrafana.WithGrafanaConfig(ctx, cfg)
}
