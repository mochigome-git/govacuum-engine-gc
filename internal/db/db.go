// Package db handles data access for govacuum-engine-gc via Supabase's
// PostgREST REST API — no direct Postgres connection, no connection pool,
// no pooler-mode gotchas. Needs the service_role key (not anon) since it
// must bypass RLS to read historical data, and the "analytics" schema must
// be added to Supabase's exposed-schemas list (Project Settings → API) or
// every request 404s.
//
// Note: this package no longer writes to the DB. vacuum-engine only reads
// history here (FetchHistoricalXY) — the computed row itself is published
// to EMQX (see internal/mqtt.PublishMetric) and written by the existing
// general insert engine, the same path every other readings row already
// goes through.
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
