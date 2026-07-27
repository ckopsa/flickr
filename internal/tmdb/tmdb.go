// Package tmdb is the enrichment boundary: the only package that knows The
// Movie Database exists. A small Client interface separates the HTTP calls
// from the enrichment logic so the latter is testable with a fake.
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Result is one search hit, normalized across the movie and TV endpoints.
type Result struct {
	ID         int64
	Title      string
	Year       int
	Overview   string
	PosterPath string // e.g. "/abc123.jpg"; "" = no poster
}

// Client is what the enricher needs from TMDB. Nil results (with nil error)
// mean "no match" — a legitimate, silently-skipped outcome.
type Client interface {
	SearchMovie(ctx context.Context, title string, year int) (*Result, error)
	SearchTV(ctx context.Context, title string) (*Result, error)
	Poster(ctx context.Context, posterPath string) ([]byte, error)
}

// HTTPClient talks to api.themoviedb.org v3, rate-limited to ~3 req/s so a
// first full-library enrichment doesn't trip TMDB's abuse detection.
type HTTPClient struct {
	APIKey string
	HTTP   *http.Client

	mu   sync.Mutex
	last time.Time
}

func NewHTTPClient(apiKey string) *HTTPClient {
	return &HTTPClient{APIKey: apiKey, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// wait spaces requests ~334ms apart (3/s) across all callers.
func (c *HTTPClient) wait() {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.last.Add(334 * time.Millisecond)
	if now := time.Now(); now.Before(next) {
		time.Sleep(next.Sub(now))
	}
	c.last = time.Now()
}

type searchResponse struct {
	Results []struct {
		ID           int64  `json:"id"`
		Title        string `json:"title"` // movie endpoint
		Name         string `json:"name"`  // tv endpoint
		ReleaseDate  string `json:"release_date"`
		FirstAirDate string `json:"first_air_date"`
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
	} `json:"results"`
}

func (c *HTTPClient) search(ctx context.Context, endpoint string, params url.Values) (*Result, error) {
	c.wait()
	params.Set("api_key", c.APIKey)
	u := "https://api.themoviedb.org/3/" + endpoint + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb %s: HTTP %d", endpoint, resp.StatusCode)
	}
	var sr searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	if len(sr.Results) == 0 {
		return nil, nil // no match — not an error
	}
	top := sr.Results[0]
	title, date := top.Title, top.ReleaseDate
	if title == "" {
		title, date = top.Name, top.FirstAirDate
	}
	r := &Result{ID: top.ID, Title: title, Overview: top.Overview, PosterPath: top.PosterPath}
	if len(date) >= 4 {
		r.Year, _ = strconv.Atoi(date[:4])
	}
	return r, nil
}

func (c *HTTPClient) SearchMovie(ctx context.Context, title string, year int) (*Result, error) {
	params := url.Values{"query": {title}}
	if year > 0 {
		params.Set("year", strconv.Itoa(year))
	}
	return c.search(ctx, "search/movie", params)
}

func (c *HTTPClient) SearchTV(ctx context.Context, title string) (*Result, error) {
	return c.search(ctx, "search/tv", url.Values{"query": {title}})
}

func (c *HTTPClient) Poster(ctx context.Context, posterPath string) ([]byte, error) {
	c.wait()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://image.tmdb.org/t/p/w342"+posterPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb poster: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
