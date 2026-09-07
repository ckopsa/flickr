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
	// BackdropPath is the wide still a home screen leads with; "" = none.
	BackdropPath string
}

// Details is what a search result does NOT carry, and a home screen and a
// detail page do: one detail call per title covers all three.
type Details struct {
	RuntimeMinutes int // a show's is its typical episode run time
	// Certification is the US rating ("PG", "TV-14"); "" when TMDB has
	// none for the US, which is common and not an error.
	Certification string
	Cast          []string // top-billed names, in TMDB's own order
}

// castNames is how many names a detail page shows: enough to say who is in
// it, few enough to fit under a title.
const castNames = 5

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
	// Details fetches runtime, US certification and top-billed cast for one
	// title, kind "movie" or "tv". Nil (with nil error) means TMDB has no
	// such title.
	Details(ctx context.Context, kind string, id int64) (*Details, error)
	Backdrop(ctx context.Context, backdropPath string) ([]byte, error)
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
		BackdropPath string  `json:"backdrop_path"`
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
	r := &Result{ID: top.ID, Title: title, Overview: top.Overview, PosterPath: top.PosterPath,
		BackdropPath: top.BackdropPath, GenreIDs: top.GenreIDs}
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

// detailsResponse is one detail call with its two appended sub-resources.
// The movie and the TV endpoints spell the same facts differently — runtime
// against episode_run_time, release_dates against content_ratings — so both
// spellings are decoded here and `details` reads the one its kind uses.
type detailsResponse struct {
	Runtime        int   `json:"runtime"`          // movie: minutes
	EpisodeRunTime []int `json:"episode_run_time"` // tv: minutes per episode
	ReleaseDates   struct {
		Results []struct {
			Country      string `json:"iso_3166_1"`
			ReleaseDates []struct {
				Certification string `json:"certification"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
	ContentRatings struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
	Credits struct {
		Cast []struct {
			Name string `json:"name"`
		} `json:"cast"`
	} `json:"credits"`
}

// details reads the payload into the three facts. A missing certification is
// left empty rather than guessed at: TMDB has no US entry for plenty of
// titles, and "unrated" is a claim nobody made.
func (d *detailsResponse) details(kind string) *Details {
	out := &Details{}
	if kind == "tv" {
		if len(d.EpisodeRunTime) > 0 {
			out.RuntimeMinutes = d.EpisodeRunTime[0]
		}
		for _, r := range d.ContentRatings.Results {
			if r.Country == "US" && r.Rating != "" {
				out.Certification = r.Rating
				break
			}
		}
	} else {
		out.RuntimeMinutes = d.Runtime
		for _, r := range d.ReleaseDates.Results {
			if r.Country != "US" {
				continue
			}
			for _, rd := range r.ReleaseDates {
				if rd.Certification != "" {
					out.Certification = rd.Certification
					break
				}
			}
			break
		}
	}
	for _, c := range d.Credits.Cast {
		if len(out.Cast) == castNames {
			break
		}
		if c.Name != "" {
			out.Cast = append(out.Cast, c.Name)
		}
	}
	return out
}

// Details fetches /movie/{id} or /tv/{id} with the certification and credits
// appended — one request per title, not three. A 404 is a nil Details.
func (c *HTTPClient) Details(ctx context.Context, kind string, id int64) (*Details, error) {
	var dr detailsResponse
	appended := "release_dates,credits"
	if kind == "tv" {
		appended = "content_ratings,credits"
	}
	params := url.Values{"append_to_response": {appended}}
	ok, err := c.getJSON(ctx, fmt.Sprintf("%s/%d", kind, id), params, true, &dr)
	if err != nil || !ok {
		return nil, err
	}
	return dr.details(kind), nil
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

// Backdrop downloads the wide still at w1280 — it is drawn full-bleed behind
// a home screen, so it is the one image worth more than a thumbnail.
func (c *HTTPClient) Backdrop(ctx context.Context, backdropPath string) ([]byte, error) {
	return c.image(ctx, "w1280", backdropPath)
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
