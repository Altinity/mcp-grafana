package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// vertamediaDefaultDatabase is the fallback default database when neither the tool
// invocation nor the datasource jsonData specifies one. ClickHouse itself uses
// "default" if no database is selected on a query.
const vertamediaDefaultDatabase = "default"

// vertamediaClickHouseClient routes ClickHouse queries through Grafana's generic
// datasource proxy for the vertamedia-clickhouse-datasource plugin
// (https://github.com/Altinity/clickhouse-grafana).
//
// Unlike the official plugin path which posts to /api/ds/query with a plugin-specific
// dataframe payload, vertamedia exposes the ClickHouse HTTP interface directly through
// /api/datasources/proxy/uid/<uid>/. Grafana core handles auth (including OAuth
// pass-through via Authorization: Bearer <jwt> when oauthPassThru=true on the datasource),
// so the inbound X-JWT-Assertion identity that mcp-grafana already carries propagates
// end-to-end without any new wiring.
type vertamediaClickHouseClient struct {
	httpClient      *http.Client
	baseURL         string
	dsUID           string
	defaultDatabase string
}

func newVertamediaClickHouseClient(httpClient *http.Client, baseURL, dsUID string, jsonData any) *vertamediaClickHouseClient {
	c := &vertamediaClickHouseClient{
		httpClient:      httpClient,
		baseURL:         baseURL,
		dsUID:           dsUID,
		defaultDatabase: vertamediaDefaultDatabase,
	}
	if m, ok := jsonData.(map[string]any); ok {
		if v, ok := m["defaultDatabase"].(string); ok && v != "" {
			c.defaultDatabase = v
		}
	}
	return c
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
	sql := ensureFormatJSON(rawSQL)

	params := url.Values{}
	params.Set("query", sql)
	params.Set("database", c.defaultDatabase)
	endpoint := fmt.Sprintf("%s/api/datasources/proxy/uid/%s/?%s", c.baseURL, url.PathEscape(c.dsUID), params.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

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
		return nil, fmt.Errorf("vertamedia proxy returned status %d: %s", resp.StatusCode, msg)
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
