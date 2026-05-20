package tools

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	mcpgrafana "github.com/grafana/mcp-grafana"
)

// vertamediaDefaultDatabase is the fallback default database when neither the
// datasource jsonData nor the explicit URL specifies one.
const vertamediaDefaultDatabase = "default"

// vertamediaQueryTimeout caps each ClickHouse HTTP call. Tuned against the
// row-limit cap (max 1000 rows), not for OLAP-scale exports.
const vertamediaQueryTimeout = 30 * time.Second

// vertamediaClickHouseClient issues ClickHouse queries directly against the
// ClickHouse HTTP interface, using the inbound user's bearer token as the
// per-user identity. Discovery of the CH endpoint and database default still
// goes through Grafana (getDatasourceByUID), so the datasource's existence
// and the user's RBAC visibility are still enforced — but the query itself
// bypasses Grafana's datasource proxy.
//
// Why we bypass the proxy: Grafana's oauthPassThru only forwards an OAuth
// token when the user has a Grafana-managed OAuth session (i.e., the
// cookie-based browser login path). The MCP server's path establishes
// identity via X-JWT-Assertion → Grafana [auth.jwt], which resolves the
// user but does NOT mint an OAuth session, so oauthPassThru has nothing
// to forward. The dashboard works, the MCP proxy path doesn't, both are
// consistent with how Grafana's auth subsystems compose.
//
// Sending the inbound bearer directly to ClickHouse sidesteps the gap:
// ClickHouse's token_processor / JWKS validates it the same way it would
// have validated the Grafana-forwarded copy. Net auth posture: identical.
//
// Tradeoff: this is a fork-only divergence from upstream mcp-grafana,
// which exclusively talks through the Grafana proxy. Not a candidate for
// upstream PR.
type vertamediaClickHouseClient struct {
	httpClient      *http.Client
	chURL           string
	bearer          string
	defaultDatabase string
}

func newVertamediaClickHouseClient(chURL, bearer, defaultDatabase string, tlsSkipVerify bool) *vertamediaClickHouseClient {
	transport := http.DefaultTransport
	if tlsSkipVerify {
		transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	if defaultDatabase == "" {
		defaultDatabase = vertamediaDefaultDatabase
	}
	return &vertamediaClickHouseClient{
		httpClient:      &http.Client{Transport: transport, Timeout: vertamediaQueryTimeout},
		chURL:           strings.TrimRight(chURL, "/"),
		bearer:          bearer,
		defaultDatabase: defaultDatabase,
	}
}

// vertamediaQueryResponse mirrors the ClickHouse HTTP JSON envelope emitted by
// FORMAT JSON. The structure has been stable across ClickHouse versions for years.
type vertamediaQueryResponse struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data []map[string]any `json:"data"`
	Rows int              `json:"rows"`
}

// formatJSONRe matches a trailing FORMAT clause (case-insensitive) so we don't
// double-append. We don't bother with string-literal awareness because macro
// substitution already ran upstream and SQL injection is not the threat model.
var formatJSONRe = regexp.MustCompile(`(?i)\bFORMAT\s+\w+\s*$`)

// ensureFormatJSON appends "FORMAT JSON" if the query doesn't already end with a
// FORMAT clause. Trailing semicolons/whitespace are stripped first.
func ensureFormatJSON(sql string) string {
	sql = strings.TrimSpace(sql)
	sql = strings.TrimRight(sql, ";")
	sql = strings.TrimSpace(sql)
	if formatJSONRe.MatchString(sql) {
		return sql
	}
	return sql + " FORMAT JSON"
}

func (c *vertamediaClickHouseClient) query(ctx context.Context, _ string, rawSQL string, _, _ time.Time) (*clickHouseQueryResponse, error) {
	if c.bearer == "" {
		return nil, fmt.Errorf("vertamedia client requires an inbound user bearer (GrafanaConfig.JWTAssertion); none was set on the request context — is the OAuth broker enabled?")
	}

	sql := ensureFormatJSON(rawSQL)
	params := url.Values{}
	params.Set("query", sql)
	params.Set("database", c.defaultDatabase)
	endpoint := c.chURL + "/?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.bearer)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := resp.Header.Get("X-ClickHouse-Exception-Code")
		msg := strings.TrimSpace(string(body))
		if code != "" {
			return nil, fmt.Errorf("clickhouse error (code=%s): %s", code, msg)
		}
		return nil, fmt.Errorf("clickhouse returned status %d: %s", resp.StatusCode, msg)
	}

	bodyBytes, err := readResponseBody(resp.Body, defaultResponseLimitBytes)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var chResp vertamediaQueryResponse
	if err := json.Unmarshal(bodyBytes, &chResp); err != nil {
		return nil, fmt.Errorf("unmarshaling clickhouse response: %w", err)
	}

	return translateVertamediaResponse(&chResp), nil
}

// translateVertamediaResponse transposes ClickHouse's row-oriented JSON envelope
// into the columnar Grafana dataframe shape that queryClickHouse expects, so the
// downstream row-extraction loop (clickhouse.go: "Process frames") is shared with
// the official plugin path.
func translateVertamediaResponse(chResp *vertamediaQueryResponse) *clickHouseQueryResponse {
	frame := clickHouseFrame{
		Schema: clickHouseFrameSchema{
			RefID:  "A",
			Fields: make([]clickHouseField, len(chResp.Meta)),
		},
		Data: clickHouseFrameData{
			Values: make([][]interface{}, len(chResp.Meta)),
		},
	}
	for i, m := range chResp.Meta {
		frame.Schema.Fields[i] = clickHouseField{Name: m.Name, Type: grafanaFieldTypeFromCH(m.Type)}
		frame.Data.Values[i] = make([]interface{}, 0, len(chResp.Data))
	}
	for _, row := range chResp.Data {
		for i, m := range chResp.Meta {
			frame.Data.Values[i] = append(frame.Data.Values[i], row[m.Name])
		}
	}
	return &clickHouseQueryResponse{
		Results: map[string]clickHouseQueryResultEntry{
			"A": {Status: http.StatusOK, Frames: []clickHouseFrame{frame}},
		},
	}
}

// grafanaFieldTypeFromCH maps a ClickHouse type name (including parameterised types
// like Nullable(Int64), DateTime64(3), LowCardinality(String)) to one of the Grafana
// data-frame field type strings. The downstream queryClickHouse loop only reads field
// Name, not Type, so this is primarily for response symmetry/debug — but it keeps the
// shape honest if anything else inspects it later.
func grafanaFieldTypeFromCH(t string) string {
	upper := strings.ToUpper(t)
	if strings.HasPrefix(upper, "NULLABLE(") {
		return grafanaFieldTypeFromCH(t[len("Nullable(") : len(t)-1])
	}
	if strings.HasPrefix(upper, "LOWCARDINALITY(") {
		return grafanaFieldTypeFromCH(t[len("LowCardinality(") : len(t)-1])
	}
	switch {
	case strings.HasPrefix(upper, "DATETIME"), strings.HasPrefix(upper, "DATE"):
		return "time"
	case strings.HasPrefix(upper, "UINT"), strings.HasPrefix(upper, "INT"),
		strings.HasPrefix(upper, "FLOAT"), strings.HasPrefix(upper, "DECIMAL"):
		return "number"
	case upper == "BOOL", upper == "BOOLEAN":
		return "boolean"
	default:
		return "string"
	}
}

// vertamediaDatasourceConfig pulls the bits the adapter needs from a Grafana
// datasource model + request context. Factored out so the dispatch in
// newClickHouseClient stays readable.
func vertamediaDatasourceConfig(ctx context.Context, chURL string, jsonData any) (defaultDatabase string, tlsSkipVerify bool, bearer string) {
	defaultDatabase = vertamediaDefaultDatabase
	if m, ok := jsonData.(map[string]any); ok {
		if v, ok := m["defaultDatabase"].(string); ok && v != "" {
			defaultDatabase = v
		}
		if v, ok := m["tlsSkipVerify"].(bool); ok {
			tlsSkipVerify = v
		}
	}
	cfg := mcpgrafana.GrafanaConfigFromContext(ctx)
	bearer = cfg.JWTAssertion
	_ = chURL // accepted for symmetry with the call site; URL parsing happens at use
	return
}
