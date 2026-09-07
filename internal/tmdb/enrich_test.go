package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"flickr/internal/model"
	"flickr/internal/store"
)

type fakeClient struct {
	movieCalls   []string
	tvCalls      []string
	genreCalls   []string
	seasonCalls  []string
	detailCalls  []string
	results      map[string]*Result // keyed by title
	genreLists   map[string]map[int64]string
	seasons      map[string]*Season  // keyed by "tvID/season"
	seasonErr    error               // returned by every Season call when set
	details      map[string]*Details // keyed by "kind/id"
	detailErr    error               // returned by every Details call when set
	posterHits   int
	stillHits    int
	backdropHits int
}

func (f *fakeClient) SearchMovie(_ context.Context, title string, year int) (*Result, error) {
	f.movieCalls = append(f.movieCalls, title)
	return f.results[title], nil
}

func (f *fakeClient) SearchTV(_ context.Context, title string) (*Result, error) {
	f.tvCalls = append(f.tvCalls, title)
	return f.results[title], nil
}

func (f *fakeClient) Poster(_ context.Context, posterPath string) ([]byte, error) {
	f.posterHits++
	return []byte("jpeg-bytes"), nil
}

func (f *fakeClient) Still(_ context.Context, stillPath string) ([]byte, error) {
	f.stillHits++
	return []byte("still-bytes"), nil
}

func (f *fakeClient) Backdrop(_ context.Context, backdropPath string) ([]byte, error) {
	f.backdropHits++
	return []byte("backdrop-bytes"), nil
}

func (f *fakeClient) Details(_ context.Context, kind string, id int64) (*Details, error) {
	key := fmt.Sprintf("%s/%d", kind, id)
	f.detailCalls = append(f.detailCalls, key)
	if f.detailErr != nil {
		return nil, f.detailErr
	}
	return f.details[key], nil
}

func (f *fakeClient) GenreList(_ context.Context, kind string) (map[int64]string, error) {
	f.genreCalls = append(f.genreCalls, kind)
	if f.genreLists == nil {
		return map[int64]string{}, nil
	}
	return f.genreLists[kind], nil
}

func (f *fakeClient) Season(_ context.Context, tvID int64, season int) (*Season, error) {
	key := fmt.Sprintf("%d/%d", tvID, season)
	f.seasonCalls = append(f.seasonCalls, key)
	if f.seasonErr != nil {
		return nil, f.seasonErr
	}
	return f.seasons[key], nil
}

type fakeLibrary struct {
	items []store.Item
	saved map[int64]*model.Enrichment
}

func (f *fakeLibrary) NeedingEnrichment(int) ([]store.Item, error) { return f.items, nil }
func (f *fakeLibrary) SetEnrichment(id int64, e *model.Enrichment, _ *model.Identity) error {
	f.saved[id] = e
	return nil
}

func item(id int64, ident model.Identity) store.Item {
	return store.Item{ID: id, Identity: &ident}
}

func TestEnrichAll(t *testing.T) {
	client := &fakeClient{
		results: map[string]*Result{
			"Frozen":         {ID: 109445, Title: "Frozen", Year: 2013, Overview: "A princess...", PosterPath: "/frozen.jpg", BackdropPath: "/frozen-wide.jpg", GenreIDs: []int64{16, 12}},
			"Rick and Morty": {ID: 60625, Title: "Rick and Morty", Year: 2013, Overview: "A scientist...", PosterPath: "/rm.jpg", BackdropPath: "/rm-wide.jpg", GenreIDs: []int64{16, 35}},
		},
		details: map[string]*Details{
			"movie/109445": {RuntimeMinutes: 102, Certification: "PG", Cast: []string{"Kristen Bell", "Idina Menzel"}},
			"tv/60625":     {RuntimeMinutes: 22, Certification: "TV-14", Cast: []string{"Justin Roiland"}},
		},
		genreLists: map[string]map[int64]string{
			"movie": {16: "Animation", 12: "Adventure"},
			"tv":    {16: "Animation", 35: "Comedy"},
		},
		seasons: map[string]*Season{
			"60625/1": {Episodes: []Episode{
				{Number: 1, Title: "Pilot", Overview: "Rick moves in...", StillPath: "/e1.jpg"},
				{Number: 2, Title: "Lawnmower Dog", Overview: "Dog stuff", StillPath: ""},
			}},
		},
	}
	lib := &fakeLibrary{
		items: []store.Item{
			item(1, model.Identity{Kind: "movie", Title: "Frozen", Year: 2013}),
			item(2, model.Identity{Kind: "episode", Title: "Rick and Morty", Season: 1, Episode: 1}),
			item(3, model.Identity{Kind: "episode", Title: "Rick and Morty", Season: 1, Episode: 2}),
			item(4, model.Identity{Kind: "unknown", Title: "beach trip"}),
			item(5, model.Identity{Kind: "movie", Title: "No Such Film", Year: 1999}),
		},
		saved: map[int64]*model.Enrichment{},
	}
	e := &Enricher{Client: client, Library: lib,
		PostersDir: t.TempDir(), StillsDir: t.TempDir(), BackdropsDir: t.TempDir()}

	n, err := e.EnrichAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("enriched %d items, want 3", n)
	}

	// Movie: mapped per contract (version marker, genres attached), poster
	// written under the item id.
	got := lib.saved[1]
	if got == nil || got.TMDBID != 109445 || got.Title != "Frozen" || got.Year != 2013 || !got.HasPoster {
		t.Errorf("movie enrichment: %+v", got)
	}
	if got.Version != EnrichmentVersion {
		t.Errorf("movie enrichment version = %d, want %d", got.Version, EnrichmentVersion)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Animation" || got.Genres[1] != "Adventure" {
		t.Errorf("movie genres: %v", got.Genres)
	}
	if got.EpisodeTitle != "" || got.HasStill {
		t.Errorf("movie must not carry episode fields: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(e.PostersDir, "1.jpg")); err != nil {
		t.Errorf("poster file missing: %v", err)
	}
	// The detail call's three facts, and the backdrop beside the poster.
	if got.RuntimeMinutes != 102 || got.Certification != "PG" || len(got.Cast) != 2 || got.Cast[0] != "Kristen Bell" {
		t.Errorf("movie details: runtime %d, cert %q, cast %v", got.RuntimeMinutes, got.Certification, got.Cast)
	}
	if !got.HasBackdrop {
		t.Error("movie should have a backdrop")
	}
	if _, err := os.Stat(filepath.Join(e.BackdropsDir, "1.jpg")); err != nil {
		t.Errorf("backdrop file missing: %v", err)
	}

	// Episodes share the SHOW's entry — one TV lookup, one poster download,
	// one season fetch for both episodes.
	if len(client.tvCalls) != 1 {
		t.Errorf("TV lookups = %v, want exactly one for the show", client.tvCalls)
	}
	if client.posterHits != 2 { // one per distinct poster path (movie + show)
		t.Errorf("poster downloads = %d, want 2", client.posterHits)
	}
	if client.backdropHits != 2 { // likewise, one per distinct backdrop path
		t.Errorf("backdrop downloads = %d, want 2", client.backdropHits)
	}
	// One detail call per TITLE, not per item: two episodes share the show's.
	if len(client.detailCalls) != 2 {
		t.Errorf("detail fetches = %v, want one movie + one show", client.detailCalls)
	}
	if len(client.seasonCalls) != 1 || client.seasonCalls[0] != "60625/1" {
		t.Errorf("season fetches = %v, want exactly one for 60625/1", client.seasonCalls)
	}
	// Genre lists are fetched once per kind, not per item.
	if len(client.genreCalls) != 2 {
		t.Errorf("genre list fetches = %v, want one movie + one tv", client.genreCalls)
	}

	// Episode 1: per-episode fields + still on disk.
	ep1 := lib.saved[2]
	if ep1 == nil || ep1.TMDBID != 60625 || ep1.EpisodeTitle != "Pilot" || ep1.EpisodeOverview != "Rick moves in..." {
		t.Errorf("episode 1 enrichment: %+v", ep1)
	}
	if !ep1.HasStill {
		t.Error("episode 1 should have a still")
	}
	if _, err := os.Stat(filepath.Join(e.StillsDir, "2.jpg")); err != nil {
		t.Errorf("episode 1 still missing: %v", err)
	}
	if len(ep1.Genres) != 2 || ep1.Genres[1] != "Comedy" {
		t.Errorf("episode genres (from tv list): %v", ep1.Genres)
	}
	// An episode carries the SHOW's facts: the tv runtime is per episode,
	// and the certification is the show's.
	if ep1.RuntimeMinutes != 22 || ep1.Certification != "TV-14" || !ep1.HasBackdrop {
		t.Errorf("episode 1 show-level facts: %+v", ep1)
	}

	// Episode 2 has no still_path in the season payload: fields present,
	// has_still honest.
	ep2 := lib.saved[3]
	if ep2 == nil || ep2.EpisodeTitle != "Lawnmower Dog" || ep2.HasStill {
		t.Errorf("episode 2 enrichment: %+v", ep2)
	}
	if client.stillHits != 1 {
		t.Errorf("still downloads = %d, want 1", client.stillHits)
	}

	// Unknown kinds and TMDB misses are silently skipped (retried next scan).
	if _, ok := lib.saved[4]; ok {
		t.Error("unknown item was enriched")
	}
	if _, ok := lib.saved[5]; ok {
		t.Error("unmatched item was persisted")
	}
	// Movie search hit the movie endpoint (with the year), not TV.
	if len(client.movieCalls) != 2 { // Frozen + No Such Film
		t.Errorf("movie lookups = %v", client.movieCalls)
	}
}

// TestEnrichEpisodeMissingFromSeason: the season exists but lacks the
// episode number (e.g. a mislabeled file) — show-level enrichment is still
// persisted, without episode fields. A season TMDB doesn't have at all (nil
// payload) behaves the same.
func TestEnrichEpisodeMissingFromSeason(t *testing.T) {
	client := &fakeClient{
		results: map[string]*Result{
			"Some Show": {ID: 7, Title: "Some Show", GenreIDs: []int64{18}},
		},
		genreLists: map[string]map[int64]string{"tv": {18: "Drama"}},
		seasons: map[string]*Season{
			"7/1": {Episodes: []Episode{{Number: 1, Title: "One"}}},
		},
	}
	lib := &fakeLibrary{
		items: []store.Item{
			item(1, model.Identity{Kind: "episode", Title: "Some Show", Season: 1, Episode: 99}),
			item(2, model.Identity{Kind: "episode", Title: "Some Show", Season: 5, Episode: 1}), // no such season
		},
		saved: map[int64]*model.Enrichment{},
	}
	e := &Enricher{Client: client, Library: lib, PostersDir: t.TempDir(), StillsDir: t.TempDir()}
	n, err := e.EnrichAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("enriched %d, want 2 (missing episodes still get show-level data)", n)
	}
	for _, id := range []int64{1, 2} {
		got := lib.saved[id]
		if got == nil || got.TMDBID != 7 || got.EpisodeTitle != "" || got.HasStill {
			t.Errorf("item %d: %+v", id, got)
		}
		if len(got.Genres) != 1 || got.Genres[0] != "Drama" {
			t.Errorf("item %d genres: %v", id, got.Genres)
		}
	}
}

// TestEnrichDetailFetchErrorSkips: the same discipline for the detail call —
// a row stamped at the current version without the runtime, certification and
// cast would never be retried, so the item is skipped and the failure is
// cached: one attempt per title per run.
func TestEnrichDetailFetchErrorSkips(t *testing.T) {
	client := &fakeClient{
		results:   map[string]*Result{"Some Show": {ID: 7, Title: "Some Show"}},
		detailErr: fmt.Errorf("tmdb down"),
	}
	lib := &fakeLibrary{
		items: []store.Item{
			item(1, model.Identity{Kind: "episode", Title: "Some Show", Season: 1, Episode: 1}),
			item(2, model.Identity{Kind: "episode", Title: "Some Show", Season: 1, Episode: 2}),
		},
		saved: map[int64]*model.Enrichment{},
	}
	e := &Enricher{Client: client, Library: lib,
		PostersDir: t.TempDir(), StillsDir: t.TempDir(), BackdropsDir: t.TempDir()}
	n, err := e.EnrichAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(lib.saved) != 0 {
		t.Errorf("enriched %d (saved %v), want none on detail fetch failure", n, lib.saved)
	}
	if len(client.detailCalls) != 1 {
		t.Errorf("detail fetch attempts = %v, want the failure cached after one", client.detailCalls)
	}
}

// TestEnrichSeasonFetchErrorSkips: a failing season fetch must NOT persist a
// half-enriched row (which would never be retried) — the item is skipped
// and picked up again next scan. The failure is cached: one attempt per
// show-season per run.
func TestEnrichSeasonFetchErrorSkips(t *testing.T) {
	client := &fakeClient{
		results: map[string]*Result{
			"Some Show": {ID: 7, Title: "Some Show"},
		},
		seasonErr: fmt.Errorf("tmdb down"),
	}
	lib := &fakeLibrary{
		items: []store.Item{
			item(1, model.Identity{Kind: "episode", Title: "Some Show", Season: 1, Episode: 1}),
			item(2, model.Identity{Kind: "episode", Title: "Some Show", Season: 1, Episode: 2}),
		},
		saved: map[int64]*model.Enrichment{},
	}
	e := &Enricher{Client: client, Library: lib, PostersDir: t.TempDir(), StillsDir: t.TempDir()}
	n, err := e.EnrichAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(lib.saved) != 0 {
		t.Errorf("enriched %d (saved %v), want none on season fetch failure", n, lib.saved)
	}
	if len(client.seasonCalls) != 1 {
		t.Errorf("season fetch attempts = %v, want the failure cached after one", client.seasonCalls)
	}
}

// TestMapResult pins the search-result-plus-details -> enrichment mapping:
// what a document is allowed to say about a title, and what it must leave
// unsaid when TMDB did not know it.
func TestMapResult(t *testing.T) {
	matrix := &Result{ID: 603, Title: "The Matrix", Year: 1999, Overview: "There is no spoon."}
	for _, tc := range []struct {
		name                   string
		res                    *Result
		det                    *Details
		hasPoster, hasBackdrop bool
		want                   model.Enrichment
	}{
		{
			name: "the whole title",
			res:  matrix,
			det: &Details{RuntimeMinutes: 136, Certification: "R",
				Cast: []string{"Keanu Reeves", "Laurence Fishburne"}},
			hasPoster: true, hasBackdrop: true,
			want: model.Enrichment{Version: EnrichmentVersion, TMDBID: 603, Title: "The Matrix",
				Year: 1999, Overview: "There is no spoon.", HasPoster: true, HasBackdrop: true,
				RuntimeMinutes: 136, Certification: "R",
				Cast: []string{"Keanu Reeves", "Laurence Fishburne"}},
		},
		{
			// No detail call answered: the three facts stay absent rather
			// than being written as zero, "" and [].
			name: "no details at all",
			res:  matrix, det: nil, hasPoster: true,
			want: model.Enrichment{Version: EnrichmentVersion, TMDBID: 603, Title: "The Matrix",
				Year: 1999, Overview: "There is no spoon.", HasPoster: true},
		},
		{
			// TMDB has no US entry for plenty of titles; the runtime and the
			// cast it does know still land.
			name: "details without a certification",
			res:  matrix,
			det:  &Details{RuntimeMinutes: 136, Cast: []string{"Keanu Reeves"}},
			want: model.Enrichment{Version: EnrichmentVersion, TMDBID: 603, Title: "The Matrix",
				Year: 1999, Overview: "There is no spoon.",
				RuntimeMinutes: 136, Cast: []string{"Keanu Reeves"}},
		},
		{
			// A show whose backdrop downloaded but whose poster did not:
			// each flag says what is actually on disk.
			name:        "backdrop without a poster",
			res:         &Result{ID: 2316, Title: "The Office"},
			det:         &Details{RuntimeMinutes: 22, Certification: "TV-14"},
			hasBackdrop: true,
			want: model.Enrichment{Version: EnrichmentVersion, TMDBID: 2316, Title: "The Office",
				HasBackdrop: true, RuntimeMinutes: 22, Certification: "TV-14"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mapResult(tc.res, tc.det, tc.hasPoster, tc.hasBackdrop)
			if !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("mapResult = %+v, want %+v", *got, tc.want)
			}
		})
	}
}

// TestDetailsPayload pins the other half of the mapping, from TMDB's own
// bytes: which of the two spellings a kind reads, that only the US rating
// counts, and that the cast is cut to the few names a page bills.
func TestDetailsPayload(t *testing.T) {
	for _, tc := range []struct {
		name, kind, payload string
		want                Details
	}{
		{
			name: "a film reads runtime and release_dates",
			kind: "movie",
			// The US entry's first release often carries no certification
			// at all; the rating is on a later one. Another country's is
			// never the answer.
			payload: `{"runtime": 136, "episode_run_time": [99], "release_dates": {"results": [
				{"iso_3166_1": "GB", "release_dates": [{"certification": "15"}]},
				{"iso_3166_1": "US", "release_dates": [{"certification": ""}, {"certification": "R"}]}]}}`,
			want: Details{RuntimeMinutes: 136, Certification: "R"},
		},
		{
			name: "a show reads episode_run_time and content_ratings",
			kind: "tv",
			payload: `{"runtime": 136, "episode_run_time": [22], "content_ratings": {"results": [
				{"iso_3166_1": "AU", "rating": "M"}, {"iso_3166_1": "US", "rating": "TV-14"}]}}`,
			want: Details{RuntimeMinutes: 22, Certification: "TV-14"},
		},
		{
			name: "a title TMDB rates nowhere has no certification",
			kind: "movie", payload: `{"runtime": 90}`,
			want: Details{RuntimeMinutes: 90},
		},
		{
			name: "the cast is cut to what a page bills",
			kind: "movie",
			payload: `{"credits": {"cast": [{"name": "One"}, {"name": "Two"}, {"name": "Three"},
				{"name": "Four"}, {"name": "Five"}, {"name": "Six"}, {"name": "Seven"}]}}`,
			want: Details{Cast: []string{"One", "Two", "Three", "Four", "Five"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dr detailsResponse
			if err := json.Unmarshal([]byte(tc.payload), &dr); err != nil {
				t.Fatal(err)
			}
			if got := dr.details(tc.kind); !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("details(%q) = %+v, want %+v", tc.kind, *got, tc.want)
			}
		})
	}
}
