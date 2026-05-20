//go:build unit

package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		"String":                 "string",
		"UInt64":                 "number",
		"Int32":                  "number",
		"Float64":                "number",
		"Decimal(10,2)":          "number",
		"DateTime":               "time",
		"DateTime64(3)":          "time",
		"Date":                   "time",
		"Bool":                   "boolean",
		"Nullable(String)":       "string",
		"Nullable(Int64)":        "number",
		"LowCardinality(String)": "string",
		"Array(Int64)":           "string", // unknown → string fallback
	}
	for in, want := range cases {
		assert.Equal(t, want, grafanaFieldTypeFromCH(in), "input=%q", in)
	}
}

// fakeClickHouse mimics the ClickHouse HTTP endpoint that the vertamedia
// adapter now talks to directly. Records the inbound request for assertions.
type fakeClickHouse struct {
	server  *httptest.Server
	lastReq *http.Request
	body    []byte
	status  int
	headers http.Header
}

func newFakeCH(t *testing.T) *fakeClickHouse {
	t.Helper()
	f := &fakeClickHouse{
		status:  http.StatusOK,
		headers: http.Header{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r.Clone(context.Background())
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

func (f *fakeClickHouse) setSuccessBody(v any) {
	b, _ := json.Marshal(v)
	f.body = b
	f.status = http.StatusOK
	f.headers.Set("Content-Type", "application/json")
}

func (f *fakeClickHouse) setErrorBody(status int, code, msg string) {
	f.body = []byte(msg)
	f.status = status
	f.headers = http.Header{}
	if code != "" {
		f.headers.Set("X-ClickHouse-Exception-Code", code)
	}
	f.headers.Set("Content-Type", "text/plain; charset=UTF-8")
}

func TestVertamediaClient_Query_Success(t *testing.T) {
	fake := newFakeCH(t)
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

	client := newVertamediaClickHouseClient(fake.server.URL, "test-jwt-token", "claude_otel", false)

	resp, err := client.query(context.Background(), "otel", "SELECT model, n FROM t", time.Time{}, time.Time{})
	require.NoError(t, err)

	// Inbound request shape: direct CH call, not /api/datasources/proxy/...
	require.NotNil(t, fake.lastReq)
	assert.Equal(t, http.MethodGet, fake.lastReq.Method)
	assert.Equal(t, "/", fake.lastReq.URL.Path)
	q := fake.lastReq.URL.Query()
	assert.Equal(t, "claude_otel", q.Get("database"))
	assert.Equal(t, "SELECT model, n FROM t FORMAT JSON", q.Get("query"))
	// The user's bearer reaches CH directly — this is the load-bearing
	// identity mechanism for the whole feature.
	assert.Equal(t, "Bearer test-jwt-token", fake.lastReq.Header.Get("Authorization"))

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
	fake := newFakeCH(t)
	fake.setErrorBody(http.StatusForbidden, "516",
		"Code: 516. DB::Exception: user@example.com: Authentication failed. (AUTHENTICATION_FAILED)\n")

	client := newVertamediaClickHouseClient(fake.server.URL, "test-jwt-token", "default", false)

	_, err := client.query(context.Background(), "otel", "SELECT 1", time.Time{}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "code=516")
	assert.Contains(t, err.Error(), "AUTHENTICATION_FAILED")
}

func TestVertamediaClient_Query_DefaultDatabaseFallback(t *testing.T) {
	fake := newFakeCH(t)
	fake.setSuccessBody(vertamediaQueryResponse{Meta: nil, Data: nil, Rows: 0})

	// Empty defaultDatabase → constructor falls back to "default".
	client := newVertamediaClickHouseClient(fake.server.URL, "test-jwt-token", "", false)
	_, err := client.query(context.Background(), "otel", "SELECT 1", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "default", fake.lastReq.URL.Query().Get("database"))
}

func TestVertamediaClient_Query_RequiresBearer(t *testing.T) {
	// No fake server needed — the request should fail before any HTTP call.
	client := newVertamediaClickHouseClient("http://example.invalid", "", "default", false)
	_, err := client.query(context.Background(), "otel", "SELECT 1", time.Time{}, time.Time{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires an inbound user bearer")
}

func TestVertamediaClient_TrimsTrailingSlash(t *testing.T) {
	fake := newFakeCH(t)
	fake.setSuccessBody(vertamediaQueryResponse{})

	// Construct with a trailing slash on chURL; the client should normalise.
	client := newVertamediaClickHouseClient(fake.server.URL+"/", "test-jwt-token", "default", false)
	_, err := client.query(context.Background(), "otel", "SELECT 1", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.Equal(t, "/", fake.lastReq.URL.Path)
}
