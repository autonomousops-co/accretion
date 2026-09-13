package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/autonomousops-co/accretion/internal/config"
)

// Client is an HTTP client for the Tempo trace backend.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a new Tempo client from the given configuration.
func NewClient(cfg *config.TempoConfig) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// SearchParams for trace search.
type SearchParams struct {
	Tags        map[string]string
	ServiceName string
	SpanName    string
	MinDuration string // e.g. "100ms", "1s"
	MaxDuration string
	Start       int64 // unix epoch seconds
	End         int64
	Limit       int
}

// TraceSearchResult from Tempo search API.
type TraceSearchResult struct {
	Traces []TraceSummary `json:"traces"`
}

// TraceSummary represents a single trace returned by the Tempo search API.
type TraceSummary struct {
	TraceID           string          `json:"traceID"`
	RootServiceName   string          `json:"rootServiceName"`
	RootTraceName     string          `json:"rootTraceName"`
	StartTimeUnixNano string          `json:"startTimeUnixNano"`
	SpanSets          json.RawMessage `json:"spanSets,omitempty"`
	DurationMs        float64         `json:"durationMs"`
}

// SearchTraces searches for traces matching the given parameters.
func (c *Client) SearchTraces(ctx context.Context, params *SearchParams) (*TraceSearchResult, error) {
	u, err := url.Parse(c.baseURL + "/api/search")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	if params.ServiceName != "" {
		q.Set("service.name", params.ServiceName)
	}
	if params.SpanName != "" {
		q.Set("name", params.SpanName)
	}
	if params.MinDuration != "" {
		q.Set("minDuration", params.MinDuration)
	}
	if params.MaxDuration != "" {
		q.Set("maxDuration", params.MaxDuration)
	}
	if params.Limit > 0 {
		q.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Start > 0 {
		q.Set("start", strconv.FormatInt(params.Start, 10))
	}
	if params.End > 0 {
		q.Set("end", strconv.FormatInt(params.End, 10))
	}
	for k, v := range params.Tags {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create search request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempo search request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("tempo search returned %d (failed to read body: %w)", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("tempo search returned %d: %s", resp.StatusCode, string(body))
	}

	var result TraceSearchResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode tempo search response: %w", err)
	}
	return &result, nil
}

// GetTrace retrieves a single trace by its trace ID.
func (c *Client) GetTrace(ctx context.Context, traceID string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/traces/"+traceID, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create get trace request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempo get trace request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return nil, fmt.Errorf("tempo get trace returned %d (failed to read body: %w)", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("tempo get trace returned %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read trace response: %w", err)
	}
	return json.RawMessage(body), nil
}
