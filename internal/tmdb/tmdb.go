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
	PosterPath string  // e.g. "/abc123.jpg"; "" = no poster
	GenreIDs   []int64 // resolved to names via GenreList — no per-title detail call
}

// Season is one TV season's episode listing (GET /tv/{id}/season/{n}) —
// a single call covers every episode of that show-season.
type Season struct {
	Episodes []Episode
}

// Episode is one entry of a season payload.
type Episode struct {
	Number    int
	Title     string
	Overview  string
	StillPath string // "" = no still image
}

// Client is what the enricher needs from TMDB. Nil results (with nil error)
// mean "no match" — a legitimate, silently-skipped outcome.
type Client interface {
	SearchMovie(ctx context.Context, title string, year int) (*Result, error)
	SearchTV(ctx context.Context, title string) (*Result, error)
	Poster(ctx context.Context, posterPath string) ([]byte, error)
	// GenreList maps genre id -> name for kind "movie" or "tv". Search
	// results carry genre_ids, so two cached list calls per run replace a
	// per-title detail request.
	GenreList(ctx context.Context, kind string) (map[int64]string, error)
	// Season returns a show-season's episode listing; nil (with nil error)
	// means TMDB has no such season.
	Season(ctx context.Context, tvID int64, season int) (*Season, error)
	Still(ctx context.Context, stillPath string) ([]byte, error)
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
		ID           int64   `json:"id"`
		Title        string  `json:"title"` // movie endpoint
		Name         string  `json:"name"`  // tv endpoint
		ReleaseDate  string  `json:"release_date"`
		FirstAirDate string  `json:"first_air_date"`
		Overview     string  `json:"overview"`
		PosterPath   string  `json:"poster_path"`
		GenreIDs     []int64 `json:"genre_ids"`
	} `json:"results"`
}

// getJSON fetches one rate-limited API endpoint into out. notFoundOK turns
// a 404 into (false, nil) — for lookups where "doesn't exist" is an answer,
// not an error.
func (c *HTTPClient) getJSON(ctx context.Context, endpoint string, params url.Values, notFoundOK bool, out any) (bool, error) {
	c.wait()
	if params == nil {
		params = url.Values{}
	}
	params.Set("api_key", c.APIKey)
	u := "https://api.themoviedb.org/3/" + endpoint + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if notFoundOK && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("tmdb %s: HTTP %d", endpoint, resp.StatusCode)
	}
	return true, json.NewDecoder(resp.Body).Decode(out)
}

func (c *HTTPClient) search(ctx context.Context, endpoint string, params url.Values) (*Result, error) {
	var sr searchResponse
	if _, err := c.getJSON(ctx, endpoint, params, false, &sr); err != nil {
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
	r := &Result{ID: top.ID, Title: title, Overview: top.Overview, PosterPath: top.PosterPath, GenreIDs: top.GenreIDs}
	if len(date) >= 4 {
		r.Year, _ = strconv.Atoi(date[:4])
	}
	return r, nil
}

// GenreList fetches /genre/{movie|tv}/list — the id->name table search
// results' genre_ids point into.
func (c *HTTPClient) GenreList(ctx context.Context, kind string) (map[int64]string, error) {
	var gr struct {
		Genres []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"genres"`
	}
	if _, err := c.getJSON(ctx, "genre/"+kind+"/list", nil, false, &gr); err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(gr.Genres))
	for _, g := range gr.Genres {
		out[g.ID] = g.Name
	}
	return out, nil
}

// Season fetches /tv/{id}/season/{n}; a 404 (show has no such season) is a
// nil Season, not an error.
func (c *HTTPClient) Season(ctx context.Context, tvID int64, season int) (*Season, error) {
	var sr struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
			Overview      string `json:"overview"`
			StillPath     string `json:"still_path"`
		} `json:"episodes"`
	}
	endpoint := fmt.Sprintf("tv/%d/season/%d", tvID, season)
	ok, err := c.getJSON(ctx, endpoint, nil, true, &sr)
	if err != nil || !ok {
		return nil, err
	}
	s := &Season{}
	for _, ep := range sr.Episodes {
		s.Episodes = append(s.Episodes, Episode{
			Number: ep.EpisodeNumber, Title: ep.Name, Overview: ep.Overview, StillPath: ep.StillPath,
		})
	}
	return s, nil
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
	return c.image(ctx, "w342", posterPath)
}

// Still downloads an episode still at w300 (stills are landscape frames;
// w300 keeps them thumbnail-sized).
func (c *HTTPClient) Still(ctx context.Context, stillPath string) ([]byte, error) {
	return c.image(ctx, "w300", stillPath)
}

func (c *HTTPClient) image(ctx context.Context, size, imagePath string) ([]byte, error) {
	c.wait()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://image.tmdb.org/t/p/"+size+imagePath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb image %s: HTTP %d", imagePath, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
