//go:build unit

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureFormatJSON(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"SELECT 1", "SELECT 1 FORMAT JSON"},
		{"SELECT 1;", "SELECT 1 FORMAT JSON"},
		{"SELECT 1   ;\n  ", "SELECT 1 FORMAT JSON"},
		{"SELECT 1 FORMAT JSON", "SELECT 1 FORMAT JSON"},
		{"select 1 format JSONEachRow", "select 1 format JSONEachRow"},
		{"SELECT 1 FORMAT TabSeparated  ", "SELECT 1 FORMAT TabSeparated"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ensureFormatJSON(c.in), "input=%q", c.in)
	}
}

func TestGrafanaFieldTypeFromCH(t *testing.T) {
	cases := map[string]string{
		"String":                "string",
		"UInt64":                "number",
		"Int32":                 "number",
		"Float64":               "number",
		"Decimal(10,2)":         "number",
		"DateTime":              "time",
		"DateTime64(3)":         "time",
		"Date":                  "time",
		"Bool":                  "boolean",
		"Nullable(String)":      "string",
		"Nullable(Int64)":       "number",
		"LowCardinality(String)": "string",
		"Array(Int64)":          "string", // unknown → string fallback
	}
	for in, want := range cases {
		assert.Equal(t, want, grafanaFieldTypeFromCH(in), "input=%q", in)
	}
}

// fakeGrafanaProxy spins up an httptest.Server that mimics Grafana's
// /api/datasources/proxy/uid/<uid>/ endpoint as observed against the live
// vertamedia datasource. It records the inbound request for assertions.
type fakeGrafanaProxy struct {
	server      *httptest.Server
	lastReq     *http.Request
	lastRawQuery string
	body        []byte
	status      int
	headers     http.Header
}

func newFakeProxy(t *testing.T, dsUID string) *fakeGrafanaProxy {
	t.Helper()
	f := &fakeGrafanaProxy{
		status:  http.StatusOK,
		headers: http.Header{},
	}
	prefix := "/api/datasources/proxy/uid/" + dsUID + "/"
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			http.NotFound(w, r)
			return
		}
		f.lastReq = r.Clone(context.Background())
		f.lastRawQuery = r.URL.RawQuery
		for k, vs := range f.headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(f.status)
		_, _ = w.Write(f.body)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGrafanaProxy) setSuccessBody(v any) {
	b, _ := json.Marshal(v)
	f.body = b
	f.status = http.StatusOK
	f.headers.Set("Content-Type", "application/json")
}

func (f *fakeGrafanaProxy) setErrorBody(status int, code, msg string) {
	f.body = []byte(msg)
	f.status = status
	f.headers = http.Header{}
	if code != "" {
		f.headers.Set("X-ClickHouse-Exception-Code", code)
	}
	f.headers.Set("Content-Type", "text/plain; charset=UTF-8")
}

func TestVertamediaClient_Query_Success(t *testing.T) {
	const dsUID = "otel"
	fake := newFakeProxy(t, dsUID)
	fake.setSuccessBody(vertamediaQueryResponse{
		Meta: []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}{
			{Name: "model", Type: "String"},
			{Name: "n", Type: "UInt64"},
		},
		Data: []map[string]any{
			{"model": "claude-opus-4-7", "n": float64(42)},
			{"model": "claude-sonnet-4-6", "n": float64(7)},
		},
		Rows: 2,
	})

	client := newVertamediaClickHouseClient(fake.server.Client(), fake.server.URL, dsUID, map[string]any{"defaultDatabase": "claude_otel"})

	resp, err := client.query(context.Background(), dsUID, "SELECT model, n FROM t", time.Time{}, time.Time{})
	require.NoError(t, err)

	// Inbound request shape matches what the live dashboard sends.
	require.NotNil(t, fake.lastReq)
	assert.Equal(t, http.MethodGet, fake.lastReq.Method)
	assert.Equal(t, "/api/datasources/proxy/uid/"+dsUID+"/", fake.lastReq.URL.Path)
	q := fake.lastReq.URL.Query()
	assert.Equal(t, "claude_otel", q.Get("database"))
	assert.Equal(t, "SELECT model, n FROM t FORMAT JSON", q.Get("query"))

	// Translated to one frame, two columns, two rows, in column-major order.
	r := resp.Results["A"]
	require.Len(t, r.Frames, 1)
	frame := r.Frames[0]
	require.Len(t, frame.Schema.Fields, 2)
	assert.Equal(t, "model", frame.Schema.Fields[0].Name)
	assert.Equal(t, "string", frame.Schema.Fields[0].Type)
	assert.Equal(t, "n", frame.Schema.Fields[1].Name)
	assert.Equal(t, "number", frame.Schema.Fields[1].Type)
	require.Len(t, frame.Data.Values, 2)
	assert.Equal(t, []interface{}{"claude-opus-4-7", "claude-sonnet-4-6"}, frame.Data.Values[0])
	assert.Equal(t, []interface{}{float64(42), float64(7)}, frame.Data.Values[1])
}

func TestVertamediaClient_Query_ErrorEnvelope(t *testing.T) {
	const dsUID = "otel"
	fake := newFakeProxy(t, dsUID)
	fake.setErrorBody(http.StatusForbidden, "516",
		"Code: 516. DB::Exception: user@example.com: Authentication failed. (AUTHENTICATION_FAILED)\n")

	client := newVertamediaClickHouseClient(fake.server.Client(), fake.server.URL, dsUID, nil)

	_, err := client.query(context.Background(), dsUID, "SELECT 1", time.Time{}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "code=516")
	assert.Contains(t, err.Error(), "AUTHENTICATION_FAILED")
}

func TestVertamediaClient_Query_DefaultDatabaseFallback(t *testing.T) {
	const dsUID = "otel"
	fake := newFakeProxy(t, dsUID)
	fake.setSuccessBody(vertamediaQueryResponse{Meta: nil, Data: nil, Rows: 0})

	// No defaultDatabase in jsonData → fallback to "default".
	client := newVertamediaClickHouseClient(fake.server.Client(), fake.server.URL, dsUID, nil)
	_, err := client.query(context.Background(), dsUID, "SELECT 1", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "default", fake.lastReq.URL.Query().Get("database"))
}

// TestVertamediaClient_TransportPropagatesIdentityHeaders ensures that any header
// installed by the inbound HTTP client (in production, X-JWT-Assertion via
// JWTAssertionRoundTripper) is preserved on the outbound proxy call. The whole
// motivation for this adapter is per-user OAuth identity reaching ClickHouse.
func TestVertamediaClient_TransportPropagatesIdentityHeaders(t *testing.T) {
	const dsUID = "otel"
	fake := newFakeProxy(t, dsUID)
	fake.setSuccessBody(vertamediaQueryResponse{})

	httpClient := &http.Client{Transport: &headerInjectingTransport{
		base:  http.DefaultTransport,
		key:   "X-JWT-Assertion",
		value: "test-jwt-token",
	}}

	client := newVertamediaClickHouseClient(httpClient, fake.server.URL, dsUID, nil)
	_, err := client.query(context.Background(), dsUID, "SELECT 1", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "test-jwt-token", fake.lastReq.Header.Get("X-JWT-Assertion"))
}

type headerInjectingTransport struct {
	base       http.RoundTripper
	key, value string
}

func (h *headerInjectingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set(h.key, h.value)
	return h.base.RoundTrip(r)
}

