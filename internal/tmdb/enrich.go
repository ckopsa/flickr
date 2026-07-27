package tmdb

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"flickr/internal/model"
	"flickr/internal/store"
)

// EnrichmentVersion is stamped into every enrichment JSON ("v"). Bumping it
// makes the next pass re-enrich items whose stored enrichment predates the
// current field set, even though their identity is unchanged.
// v2: genres + per-episode fields (title, overview, still).
const EnrichmentVersion = 2

// libraryStore is the slice of the store the enricher needs (narrow so tests
// can fake it without SQLite).
type libraryStore interface {
	NeedingEnrichment(minVersion int) ([]store.Item, error)
	SetEnrichment(id int64, e *model.Enrichment, ident *model.Identity) error
}

// Enricher fills in TMDB metadata after identification. Failures are silent
// by design: an unenriched item stays unenriched (still NULL in the store)
// and is simply retried on the next scan.
type Enricher struct {
	Client     Client
	Library    libraryStore
	PostersDir string
	StillsDir  string
}

// runCache memoizes per-run TMDB lookups so N episodes cost one show
// search, one season fetch, one poster download — and one genre-list call
// per kind covers the whole library. Failed fetches are remembered too
// (as skip-this-run markers), so one broken endpoint doesn't retry per item.
type runCache struct {
	shows     map[string]*Result          // lowercased show title -> result (nil = known miss)
	posters   map[string][]byte           // poster path -> bytes
	genres    map[string]map[int64]string // "movie"/"tv" -> id -> name
	genreErr  map[string]bool             // genre list fetch failed this run
	seasons   map[string]*Season          // "tvID/season" -> payload (nil = TMDB has no such season)
	seasonErr map[string]bool             // season fetch failed this run
}

func newRunCache() *runCache {
	return &runCache{
		shows:     map[string]*Result{},
		posters:   map[string][]byte{},
		genres:    map[string]map[int64]string{},
		genreErr:  map[string]bool{},
		seasons:   map[string]*Season{},
		seasonErr: map[string]bool{},
	}
}

// EnrichAll processes every item needing enrichment, returning how many were
// enriched. Episodes are enriched with their SHOW's entry — one TMDB lookup
// per distinct show per run — plus per-episode fields from one season call
// per show-season.
func (e *Enricher) EnrichAll(ctx context.Context) (int, error) {
	items, err := e.Library.NeedingEnrichment(EnrichmentVersion)
	if err != nil {
		return 0, err
	}
	cache := newRunCache()
	enriched := 0
	for _, it := range items {
		if ctx.Err() != nil {
			return enriched, ctx.Err()
		}
		res, err := e.lookup(ctx, it.Identity, cache.shows)
		if err != nil || res == nil {
			continue // silent: retried next scan
		}
		if e.enrichOne(ctx, it, res, cache) != nil {
			enriched++
		}
	}
	return enriched, nil
}

// EnrichItem enriches a single item on demand (the /enrich endpoint),
// bypassing the "already enriched" filter.
func (e *Enricher) EnrichItem(ctx context.Context, it store.Item) (*model.Enrichment, error) {
	res, err := e.lookup(ctx, it.Identity, map[string]*Result{})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	return e.enrichOne(ctx, it, res, newRunCache()), nil
}

// lookup resolves an identity to a TMDB result: movies by title+year,
// episodes by show title (memoized in shows across a run).
func (e *Enricher) lookup(ctx context.Context, ident *model.Identity, shows map[string]*Result) (*Result, error) {
	if ident == nil {
		return nil, nil
	}
	switch ident.Kind {
	case "movie":
		return e.Client.SearchMovie(ctx, ident.Title, ident.Year)
	case "episode":
		key := strings.ToLower(ident.Title)
		if res, ok := shows[key]; ok {
			return res, nil
		}
		res, err := e.Client.SearchTV(ctx, ident.Title)
		if err != nil {
			return nil, err
		}
		shows[key] = res // cache misses too — don't re-ask for every episode
		return res, nil
	default:
		return nil, nil // unknown items are not enrichable
	}
}

// enrichOne persists one enrichment (images first, so has_poster/has_still
// are honest) and returns it. A nil return means "not persisted" — either a
// dependent fetch (genre list, season) failed and the item should be retried
// next scan, or the store write failed.
func (e *Enricher) enrichOne(ctx context.Context, it store.Item, res *Result, cache *runCache) *model.Enrichment {
	hasPoster := false
	if res.PosterPath != "" {
		b, ok := cache.posters[res.PosterPath]
		if !ok {
			var err error
			b, err = e.Client.Poster(ctx, res.PosterPath)
			if err != nil {
				b = nil
			}
			cache.posters[res.PosterPath] = b
		}
		if b != nil {
			hasPoster = writeImage(e.PostersDir, it.ID, b)
		}
	}

	genres, ok := e.genreNames(ctx, it.Identity.Kind, res.GenreIDs, cache)
	if !ok {
		return nil // genre list fetch failed — skip so the item is retried
	}

	enr := mapResult(res, hasPoster)
	enr.Genres = genres

	if it.Identity.Kind == "episode" && it.Identity.Season > 0 && it.Identity.Episode > 0 {
		ep, ok := e.seasonEpisode(ctx, res.ID, it.Identity.Season, it.Identity.Episode, cache)
		if !ok {
			return nil // season fetch failed — skip so the item is retried
		}
		// ep == nil (season or episode absent from TMDB) is a legitimate
		// outcome: persist the show-level enrichment without episode fields.
		if ep != nil {
			enr.EpisodeTitle = ep.Title
			enr.EpisodeOverview = ep.Overview
			if ep.StillPath != "" {
				if b, err := e.Client.Still(ctx, ep.StillPath); err == nil {
					enr.HasStill = writeImage(e.StillsDir, it.ID, b)
				}
			}
		}
	}

	if err := e.Library.SetEnrichment(it.ID, enr, it.Identity); err != nil {
		log.Printf("enrich: persist item %d: %v", it.ID, err)
		return nil
	}
	return enr
}

// genreNames resolves genre ids to names via the per-run cached genre list
// for the identity's kind. ok=false means the list fetch failed this run.
func (e *Enricher) genreNames(ctx context.Context, identKind string, ids []int64, cache *runCache) ([]string, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	kind := "movie"
	if identKind == "episode" {
		kind = "tv"
	}
	m, cached := cache.genres[kind]
	if !cached {
		if cache.genreErr[kind] {
			return nil, false
		}
		var err error
		m, err = e.Client.GenreList(ctx, kind)
		if err != nil {
			cache.genreErr[kind] = true
			return nil, false
		}
		cache.genres[kind] = m
	}
	var names []string
	for _, id := range ids {
		if name, ok := m[id]; ok {
			names = append(names, name)
		}
	}
	return names, true
}

// seasonEpisode returns the episode entry for (show, season, episode) from
// the per-run cached season payload. ok=false means the season fetch failed
// this run (retry next scan); a nil episode with ok=true means TMDB's
// payload simply lacks it.
func (e *Enricher) seasonEpisode(ctx context.Context, tvID int64, season, episode int, cache *runCache) (*Episode, bool) {
	key := fmt.Sprintf("%d/%d", tvID, season)
	s, cached := cache.seasons[key]
	if !cached {
		if cache.seasonErr[key] {
			return nil, false
		}
		var err error
		s, err = e.Client.Season(ctx, tvID, season)
		if err != nil {
			cache.seasonErr[key] = true
			return nil, false
		}
		cache.seasons[key] = s // nil = known "no such season", cached too
	}
	if s == nil {
		return nil, true
	}
	for i := range s.Episodes {
		if s.Episodes[i].Number == episode {
			return &s.Episodes[i], true
		}
	}
	return nil, true
}

// writeImage stores one jpeg under dir/<itemID>.jpg, reporting success —
// the has_poster/has_still flags must reflect what's actually on disk.
func writeImage(dir string, itemID int64, b []byte) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	path := filepath.Join(dir, strconv.FormatInt(itemID, 10)+".jpg")
	return os.WriteFile(path, b, 0o644) == nil
}

// mapResult is the pure TMDB-result -> enrichment mapping.
func mapResult(r *Result, hasPoster bool) *model.Enrichment {
	return &model.Enrichment{
		Version:   EnrichmentVersion,
		TMDBID:    r.ID,
		Title:     r.Title,
		Year:      r.Year,
		Overview:  r.Overview,
		HasPoster: hasPoster,
	}
}
