// Package db handles all data access for govacuum-engine-gc via
// Supabase's PostgREST REST API — no direct Postgres connection, no
// connection pool, no pooler-mode gotchas. Needs the service_role key
// (not anon) since it must bypass RLS for both the historical read and
// the insert, and the "analytics" schema must be added to Supabase's
// exposed-schemas list (Project Settings → API) or every request 404s.
package db

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Config holds what's needed to talk to Supabase's REST API.
type Config struct {
	URL            string // e.g. "https://YOUR-PROJECT-REF.supabase.co"
	ServiceRoleKey string // service_role key — bypasses RLS; anon key will NOT work here
	Schema         string // e.g. "analytics" — must be in Supabase's exposed-schemas list
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

// doRequest is the shared HTTP plumbing: sets auth headers, sets the
// schema via profileHeader ("Accept-Profile" for reads, "Content-Profile"
// for writes — PostgREST uses different headers per direction), and
// surfaces non-2xx responses with the actual PostgREST error body.
func (c Config) doRequest(ctx context.Context, method, path string, query url.Values, profileHeader string, body []byte) ([]byte, error) {
	reqURL := fmt.Sprintf("%s/rest/v1/%s", c.URL, path)
	if query != nil {
		reqURL += "?" + query.Encode()
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("apikey", c.ServiceRoleKey)
	req.Header.Set("Authorization", "Bearer "+c.ServiceRoleKey)
	req.Header.Set("Content-Type", "application/json")
	if c.Schema != "" {
		req.Header.Set(profileHeader, c.Schema)
	}
	if method == http.MethodPost {
		req.Header.Set("Prefer", "return=representation")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("supabase REST error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// FetchHistoricalXY pulls the last `limit` x/y pairs for this device from
// analytics.metrics via PostgREST, most recent first, then returns them
// sorted ascending (required by the percentile calculation in package
// logic). Filters to rows that have both readings.x and readings.y set,
// so mixed metric kinds sharing this table don't pollute the IQR calc.
func FetchHistoricalXY(ctx context.Context, cfg Config, deviceID string, limit int) (xs, ys []float64, err error) {
	q := url.Values{}
	q.Set("select", "readings")
	q.Set("device_id", "eq."+deviceID)
	q.Set("readings->>x", "not.is.null")
	q.Set("readings->>y", "not.is.null")
	q.Set("order", "created_at.desc")
	q.Set("limit", strconv.Itoa(limit))

	body, err := cfg.doRequest(ctx, http.MethodGet, "metrics", q, "Accept-Profile", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch historical x/y: %w", err)
	}

	var rows []struct {
		Readings struct {
			X *float64 `json:"x"`
			Y *float64 `json:"y"`
		} `json:"readings"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, nil, fmt.Errorf("parse historical x/y: %w", err)
	}

	for _, r := range rows {
		if r.Readings.X != nil && r.Readings.Y != nil {
			xs = append(xs, *r.Readings.X)
			ys = append(ys, *r.Readings.Y)
		}
	}

	sort.Float64s(xs)
	sort.Float64s(ys)

	return xs, ys, nil
}

// InsertVacuumMetric writes the computed row into analytics.metrics via
// PostgREST. readings holds the raw + derived values; status holds the
// classification. Returns the generated id (as a string — the underlying
// column is bigint, converted here so it stays compatible with gopub-edge's
// patch.VacuumData.ID, which is typed string) and created_at.
func InsertVacuumMetric(ctx context.Context, cfg Config, tenantID, deviceID string,
	vacuumStart int, v1, v2, v3 float64, x, y *float64, xStatus, yStatus string, vacuumStatus bool) (id string, createdAt time.Time, err error) {

	payload := map[string]any{
		"tenant_id":  tenantID,
		"device_id":  deviceID,
		"resolution": "event",
		"kind":       "event",
		"readings": map[string]any{
			"vacuum_start":      vacuumStart,
			"vacuum_leave_1min": v1,
			"vacuum_leave_2min": v2,
			"vacuum_leave_3min": v3,
			"x":                 x,
			"y":                 y,
		},
		"status": map[string]any{
			"x_status":      xStatus,
			"y_status":      yStatus,
			"vacuum_status": vacuumStatus,
		},
	}

	reqBody, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal insert payload: %w", err)
	}

	respBody, err := cfg.doRequest(ctx, http.MethodPost, "metrics", nil, "Content-Profile", reqBody)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("insert metric: %w", err)
	}

	var inserted []struct {
		ID        json.Number `json:"id"`
		CreatedAt time.Time   `json:"created_at"`
	}
	if err := json.Unmarshal(respBody, &inserted); err != nil {
		return "", time.Time{}, fmt.Errorf("parse insert response: %w", err)
	}
	if len(inserted) == 0 {
		return "", time.Time{}, fmt.Errorf("insert returned no rows")
	}

	return inserted[0].ID.String(), inserted[0].CreatedAt, nil
}
