package scanner

import (
	"testing"

	"flickr/internal/model"
)

// TestIdentifyDirectoryAware covers the v2 path-aware rules with
// real-shaped bucket keys.
func TestIdentifyDirectoryAware(t *testing.T) {
	cases := []struct {
		key  string
		want model.Identity
	}{
		{
			"csi-fs/Adult/Shows/Rick and Morty/Rick and Morty S01E02.mkv",
			model.Identity{Kind: "episode", Title: "Rick and Morty", Season: 1, Episode: 2},
		},
		{
			"csi-fs/Movies/Frozen (2013)/Frozen (2013).mp4",
			model.Identity{Kind: "movie", Title: "Frozen", Year: 2013},
		},
		// Filename yields nothing — the Movies/<Name (Year)>/ parent rescues it.
		{
			"csi-fs/Movies/Frozen (2013)/movie.mp4",
			model.Identity{Kind: "movie", Title: "Frozen", Year: 2013},
		},
		// Season directory supplies the season for a lone episode marker.
		{
			"media/Shows/The Wire/Season 2/E05 - All Due Respect.mkv",
			model.Identity{Kind: "episode", Title: "The Wire", Season: 2, Episode: 5},
		},
		// "Series <n>" spelling and NxM episode numbering.
		{
			"Shows/Doctor Who/Series 4/4x08 Silence in the Library.mkv",
			model.Identity{Kind: "episode", Title: "Doctor Who", Season: 4, Episode: 8},
		},
		// Show directory carrying a year: split into title + year.
		{
			"Shows/Doctor Who (2005)/Season 1/Ep 3.mkv",
			model.Identity{Kind: "episode", Title: "Doctor Who", Year: 2005, Season: 1, Episode: 3},
		},
		// SxxEyy in the filename wins over the season directory.
		{
			"tv/Shows/Foo/Season 1/Foo.S02E09.mkv",
			model.Identity{Kind: "episode", Title: "Foo", Season: 2, Episode: 9},
		},
		// Show directory but no episode marker anywhere: unknown, not a
		// fabricated episode.
		{
			"Shows/Making Of/behind the scenes.mkv",
			model.Identity{Kind: "unknown", Title: "behind the scenes"},
		},
		// Loose markers must NOT fire outside a show directory.
		{
			"home_videos/beach_trip_day2.mp4",
			model.Identity{Kind: "unknown", Title: "beach trip day2"},
		},
		// No category directory at all: unchanged v1 behavior.
		{
			"random/file.mkv",
			model.Identity{Kind: "unknown", Title: "file"},
		},
	}
	for _, c := range cases {
		if got := Identify(c.key); got != c.want {
			t.Errorf("Identify(%q) = %+v, want %+v", c.key, got, c.want)
		}
	}
}

func TestIdentifyEpisode(t *testing.T) {
	id := Identify("tv/The.Expanse.S03E07.1080p.WEB-DL.mkv")
	if id.Kind != "episode" || id.Title != "The Expanse" || id.Season != 3 || id.Episode != 7 {
		t.Errorf("got %+v", id)
	}
}

func TestIdentifyMovieWithYear(t *testing.T) {
	id := Identify("movies/Blade Runner 2049 (2017) [2160p].mkv")
	if id.Kind != "movie" || id.Title != "Blade Runner 2049" || id.Year != 2017 {
		t.Errorf("got %+v", id)
	}
}

func TestIdentifyDottedMovie(t *testing.T) {
	id := Identify("Movies/Heat.1995.Remastered.1080p.mkv")
	if id.Kind != "movie" || id.Title != "Heat" || id.Year != 1995 {
		t.Errorf("got %+v", id)
	}
}

func TestIdentifyUnknownFallsBackToCleanedName(t *testing.T) {
	id := Identify("home_videos/beach_trip_day2.mp4")
	if id.Kind != "unknown" || id.Title != "beach trip day2" {
		t.Errorf("got %+v", id)
	}
}

func TestIdentifyIsDeterministic(t *testing.T) {
	a := Identify("tv/Foo.S01E01.mkv")
	b := Identify("tv/Foo.S01E01.mkv")
	if a != b {
		t.Errorf("identification must be deterministic: %+v vs %+v", a, b)
	}
}
