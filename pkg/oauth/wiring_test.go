package oauth

import (
	"context"
	"net/http/httptest"
	"testing"

	mcpgrafana "github.com/grafana/mcp-grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBroker_Disabled(t *testing.T) {
	broker, err := NewBroker(Settings{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, broker, "disabled settings must return nil broker so main.go skips wiring")
}

func TestNewBroker_RejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name     string
		settings Settings
	}{
		{
			name:     "missing client_id",
			settings: Settings{Enabled: true, Issuer: "https://idp.example/", SigningSecret: bytes32()},
		},
		{
			name:     "missing issuer and auth/token urls",
			settings: Settings{Enabled: true, ClientID: "abc", SigningSecret: bytes32()},
		},
		{
			name:     "missing signing_secret",
			settings: Settings{Enabled: true, Issuer: "https://idp.example/", ClientID: "abc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := NewBroker(tc.settings)
			assert.Nil(t, broker)
			assert.Error(t, err)
		})
	}
}

func TestNewBroker_ValidForwardMode(t *testing.T) {
	broker, err := NewBroker(Settings{
		Enabled:       true,
		Issuer:        "https://accounts.google.com",
		ClientID:      "client-id.apps.googleusercontent.com",
		ClientSecret:  "secret",
		Audience:      "https://mcp.example.com",
		SigningSecret: bytes32(),
		Scopes:        []string{"openid", "email"},
	})
	require.NoError(t, err)
	require.NotNil(t, broker)
}

// TestPropagateIdentity_NoToken verifies the no-op path: when the request
// context has no validated bearer (e.g., OAuth disabled or middleware
// didn't run), PropagateIdentity must return the context unchanged and
// leave GrafanaConfig.JWTAssertion empty so the SA-token-only request
// path stays the default.
func TestPropagateIdentity_NoToken(t *testing.T) {
	req := httptest.NewRequest("GET", "https://mcp.example.com/mcp", nil)
	in := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{
		URL:    "http://grafana.local",
		APIKey: "sa-token",
	})

	out := PropagateIdentity(in, req)

	cfg := mcpgrafana.GrafanaConfigFromContext(out)
	assert.Empty(t, cfg.JWTAssertion)
	assert.Equal(t, "sa-token", cfg.APIKey, "SA token must remain untouched on the no-token path")
}

func bytes32() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}
