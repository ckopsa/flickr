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
// v3: backdrop, runtime, US certification and the top-billed cast.
const EnrichmentVersion = 3

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
	Client       Client
	Library      libraryStore
	PostersDir   string
	StillsDir    string
	BackdropsDir string
}

// runCache memoizes per-run TMDB lookups so N episodes cost one show
// search, one season fetch, one poster download — and one genre-list call
// per kind covers the whole library. Failed fetches are remembered too
// (as skip-this-run markers), so one broken endpoint doesn't retry per item.
type runCache struct {
	shows     map[string]*Result          // lowercased show title -> result (nil = known miss)
	posters   map[string][]byte           // poster path -> bytes
	backdrops map[string][]byte           // backdrop path -> bytes
	genres    map[string]map[int64]string // "movie"/"tv" -> id -> name
	genreErr  map[string]bool             // genre list fetch failed this run
	seasons   map[string]*Season          // "tvID/season" -> payload (nil = TMDB has no such season)
	seasonErr map[string]bool             // season fetch failed this run
	details   map[string]*Details         // "kind/id" -> payload (nil = TMDB has no such title)
	detailErr map[string]bool             // detail fetch failed this run
}

func newRunCache() *runCache {
	return &runCache{
		shows:     map[string]*Result{},
		posters:   map[string][]byte{},
		backdrops: map[string][]byte{},
		genres:    map[string]map[int64]string{},
		genreErr:  map[string]bool{},
		seasons:   map[string]*Season{},
		seasonErr: map[string]bool{},
		details:   map[string]*Details{},
		detailErr: map[string]bool{},
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

// enrichOne persists one enrichment (images first, so has_poster,
// has_backdrop and has_still are honest) and returns it. A nil return means
// "not persisted" — either a dependent fetch (genre list, details, season)
// failed and the item should be retried next scan, or the store write failed.
func (e *Enricher) enrichOne(ctx context.Context, it store.Item, res *Result, cache *runCache) *model.Enrichment {
	hasPoster := e.image(ctx, e.PostersDir, it.ID, res.PosterPath, cache.posters, e.Client.Poster)
	hasBackdrop := e.image(ctx, e.BackdropsDir, it.ID, res.BackdropPath, cache.backdrops, e.Client.Backdrop)

	genres, ok := e.genreNames(ctx, it.Identity.Kind, res.GenreIDs, cache)
	if !ok {
		return nil // genre list fetch failed — skip so the item is retried
	}
	det, ok := e.details(ctx, it.Identity.Kind, res.ID, cache)
	if !ok {
		return nil // detail fetch failed — skip so the item is retried
	}

	enr := mapResult(res, det, hasPoster, hasBackdrop)
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

// image downloads one artwork path and writes it under the item id,
// reporting whether the file is there afterwards. The per-run cache is keyed
// by TMDB path, so a show's poster and backdrop are fetched once however
// many of its episodes carry them; a failed download is cached as nil bytes.
func (e *Enricher) image(ctx context.Context, dir string, itemID int64, path string,
	cached map[string][]byte, fetch func(context.Context, string) ([]byte, error)) bool {
	if path == "" || dir == "" {
		return false
	}
	b, ok := cached[path]
	if !ok {
		var err error
		if b, err = fetch(ctx, path); err != nil {
			b = nil
		}
		cached[path] = b
	}
	return b != nil && writeImage(dir, itemID, b)
}

// tmdbKind is which half of TMDB an identity is looked up in: an episode is
// its show's TV entry, and everything else enrichable is a movie.
func tmdbKind(identKind string) string {
	if identKind == "episode" {
		return "tv"
	}
	return "movie"
}

// details returns the per-title detail payload — runtime, certification,
// cast — from the per-run cache. ok=false means the fetch failed this run
// (retry next scan); a nil Details with ok=true means TMDB has no such
// title, which is legitimate and leaves those three fields empty.
func (e *Enricher) details(ctx context.Context, identKind string, tmdbID int64, cache *runCache) (*Details, bool) {
	kind := tmdbKind(identKind)
	key := fmt.Sprintf("%s/%d", kind, tmdbID)
	d, cached := cache.details[key]
	if !cached {
		if cache.detailErr[key] {
			return nil, false
		}
		var err error
		d, err = e.Client.Details(ctx, kind, tmdbID)
		if err != nil {
			cache.detailErr[key] = true
			return nil, false
		}
		cache.details[key] = d
	}
	return d, true
}

// genreNames resolves genre ids to names via the per-run cached genre list
// for the identity's kind. ok=false means the list fetch failed this run.
func (e *Enricher) genreNames(ctx context.Context, identKind string, ids []int64, cache *runCache) ([]string, bool) {
	if len(ids) == 0 {
		return nil, true
	}
	kind := tmdbKind(identKind)
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

// mapResult is the pure TMDB-result -> enrichment mapping. det is what the
// detail call added and may be nil — TMDB matched the search but answered
// nothing for the title behind it. Each of its three facts stays empty when
// TMDB did not know it, so "no certification" reads as an absence rather
// than as a claim somebody made.
func mapResult(r *Result, det *Details, hasPoster, hasBackdrop bool) *model.Enrichment {
	e := &model.Enrichment{
		Version:     EnrichmentVersion,
		TMDBID:      r.ID,
		Title:       r.Title,
		Year:        r.Year,
		Overview:    r.Overview,
		HasPoster:   hasPoster,
		HasBackdrop: hasBackdrop,
	}
	if det != nil {
		e.RuntimeMinutes = det.RuntimeMinutes
		e.Certification = det.Certification
		e.Cast = det.Cast
	}
	return e
}
