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
		// Show directory but no episode marker anywhere: still an episode of
		// that show (episode 0 = unnumbered), never a top-level tile of its own.
		{
			"Shows/Making Of/behind the scenes.mkv",
			model.Identity{Kind: "episode", Title: "Making Of"},
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

// TestIdentifyV3Grouping covers the v3 rule that nothing under a category
// directory stays unidentified — the noise on the main grid was files the
// path could place all along.
func TestIdentifyV3Grouping(t *testing.T) {
	cases := []struct {
		key  string
		want model.Identity
	}{
		// Bonus material belongs to its show, and must NOT masquerade as the
		// real episode whose number it carries.
		{
			"csi-fs/Shows/The Office/Featurettes/Featurettes/Season 1/Deleted Scenes/S01E01 Pilot Deleted Scenes.mkv",
			model.Identity{Kind: "extra", Title: "The Office", Season: 1, Episode: 1},
		},
		{
			"csi-fs/Shows/Gravity Falls/Extras/Featurettes/Between the Pines/Between the Pines.mkv",
			model.Identity{Kind: "extra", Title: "Gravity Falls"},
		},
		// A film's extras attach to the film, year included.
		{
			"csi-fs/Movies/Frozen (2013)/Extras/Sing-Along.mkv",
			model.Identity{Kind: "extra", Title: "Frozen", Year: 2013},
		},
		// Title-named episodes in a flat show directory: unnumbered episodes.
		{
			"csi-fs/Shows/Animated Hero Classics/Abraham Lincoln.mp4",
			model.Identity{Kind: "episode", Title: "Animated Hero Classics"},
		},
		// Disc rips under a season directory: the season still places them.
		{
			"csi-fs/Shows/MASH/Season 6/B9_t01.mkv",
			model.Identity{Kind: "episode", Title: "MASH", Season: 6},
		},
		// Disc-order prefix inside a show directory is an episode number.
		{
			"csi-fs/Shows/Ninjago/Season 3/01 - The Surge.mkv",
			model.Identity{Kind: "episode", Title: "Ninjago", Season: 3, Episode: 1},
		},
		// A DECIMAL season directory is a fan-numbered interstitial block, not
		// season 4: folding it into season 4 would collide with the real
		// episodes 1-5 there. With no season to place them, the block's disc
		// ordinals are not episode numbers either — several such blocks would
		// all claim S00E01, S00E02 and interleave.
		{
			"csi-fs/Shows/Ninjago/Season 04.2 - Chen Mini-Movies (2015)/01 - Chen's New Chair.mkv",
			model.Identity{Kind: "episode", Title: "Ninjago"},
		},
		// A year-less file under Movies/ is still that movie — with a title
		// directory, or sitting loose in the category directory.
		{
			"csi-fs/Movies/Avalon/Avalon.mp4",
			model.Identity{Kind: "movie", Title: "Avalon"},
		},
		{
			"csi-fs/Movies/Cars.mkv",
			model.Identity{Kind: "movie", Title: "Cars"},
		},
		{
			"csi-fs/Documentaries/The Yule Log/The Yule Log.mp4",
			model.Identity{Kind: "movie", Title: "The Yule Log"},
		},
		// A leading YEAR is not an episode number.
		{
			"csi-fs/Shows/Some Show/2001 A Space Odyssey.mkv",
			model.Identity{Kind: "episode", Title: "Some Show"},
		},
		// "Specials" is season 0 of a show by convention, not bonus material.
		{
			"csi-fs/Shows/Foo/Specials/S00E02 Christmas.mkv",
			model.Identity{Kind: "episode", Title: "Foo", Season: 0, Episode: 2},
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

// TestIdentifyAudioAndText covers the v4 grammars — Audiobooks/, Music/,
// Books/ — including the awkward shapes: no part number, a single-file book
// filed directly under its author, a nesting prefix above the category, and
// a year on the title directory.
func TestIdentifyAudioAndText(t *testing.T) {
	cases := []struct {
		key  string
		want model.Identity
	}{
		// Audiobooks: author / title / numbered parts.
		{
			"Audiobooks/Brandon Sanderson/The Way of Kings/03 - Chapter Three.m4b",
			model.Identity{Kind: "audiobook_part", Author: "Brandon Sanderson", Title: "The Way of Kings", Part: 3},
		},
		{
			"Audiobooks/Brandon Sanderson/The Way of Kings/Part 3.mp3",
			model.Identity{Kind: "audiobook_part", Author: "Brandon Sanderson", Title: "The Way of Kings", Part: 3},
		},
		{
			"Audiobooks/Brandon Sanderson/The Way of Kings/03.mp3",
			model.Identity{Kind: "audiobook_part", Author: "Brandon Sanderson", Title: "The Way of Kings", Part: 3},
		},
		// No number anywhere in the part's name: Part 0, still a part.
		{
			"Audiobooks/Brandon Sanderson/The Way of Kings/Epilogue.m4b",
			model.Identity{Kind: "audiobook_part", Author: "Brandon Sanderson", Title: "The Way of Kings"},
		},
		// A single-file book directly under the author directory.
		{
			"Audiobooks/Ursula K. Le Guin/The Dispossessed.m4b",
			model.Identity{Kind: "audiobook_part", Author: "Ursula K. Le Guin", Title: "The Dispossessed"},
		},
		// Nesting prefix above the category, a year on the title directory,
		// and a disc directory below it that the grammar walks past.
		{
			"csi-fs/Audio/Audiobooks/Frank Herbert/Dune (1965)/Disc 2/07 Track 7.flac",
			model.Identity{Kind: "audiobook_part", Author: "Frank Herbert", Title: "Dune", Year: 1965, Part: 7},
		},
		// Nested category directories, the way "tv/Shows/..." is tolerated.
		{
			"Audiobooks/Audiobooks/Some Author/Some Book/01.mp3",
			model.Identity{Kind: "audiobook_part", Author: "Some Author", Title: "Some Book", Part: 1},
		},
		// Loose file in the category directory: the filename is all we have,
		// but it is still an audiobook — never "unknown" under a category.
		{
			"Audiobooks/Orphan Recording.mp3",
			model.Identity{Kind: "audiobook_part", Title: "Orphan Recording"},
		},
		// A movie-looking name under Audiobooks/ is an audiobook: the
		// category outranks every filename rule.
		{
			"Audiobooks/Arthur C. Clarke/2001 A Space Odyssey (1968)/2001.A.Space.Odyssey.S01E01.mp3",
			model.Identity{Kind: "audiobook_part", Author: "Arthur C. Clarke", Title: "2001 A Space Odyssey", Year: 1968},
		},
		// Music: the album is the work; the artist is the author; the track
		// number is the part. The track's own title is not kept.
		{
			"Music/Radiohead/OK Computer (1997)/01 Airbag.flac",
			model.Identity{Kind: "track", Author: "Radiohead", Title: "OK Computer", Year: 1997, Part: 1},
		},
		{
			"music/Radiohead/OK Computer (1997)/Hidden Track.flac",
			model.Identity{Kind: "track", Author: "Radiohead", Title: "OK Computer", Year: 1997},
		},
		// A loose track under the artist: the file names the work.
		{
			"Music/Radiohead/Spectre.mp3",
			model.Identity{Kind: "track", Author: "Radiohead", Title: "Spectre"},
		},
		// Books: a bare .epub under the author, or a title directory.
		{
			"Books/Ursula K. Le Guin/The Left Hand of Darkness (1969).epub",
			model.Identity{Kind: "book", Author: "Ursula K. Le Guin", Title: "The Left Hand of Darkness", Year: 1969},
		},
		{
			"Books/Ursula K. Le Guin/The Left Hand of Darkness/left-hand.epub",
			model.Identity{Kind: "book", Author: "Ursula K. Le Guin", Title: "The Left Hand of Darkness"},
		},
		// A leading number on a book's filename is part of its title, not a
		// part number: "1984" is the book.
		{
			"Books/George Orwell/1984.epub",
			model.Identity{Kind: "book", Author: "George Orwell", Title: "1984"},
		},
		// Author directories keep their dots and hyphens (they are spelled
		// that way), underscores become spaces.
		{
			"Books/Jean-Paul_Sartre/Nausea.epub",
			model.Identity{Kind: "book", Author: "Jean-Paul Sartre", Title: "Nausea"},
		},
		// A year-like leading number in a part filename is not a part number.
		{
			"Audiobooks/Arthur C. Clarke/Odyssey/2001 A Space Odyssey.mp3",
			model.Identity{Kind: "audiobook_part", Author: "Arthur C. Clarke", Title: "Odyssey"},
		},
	}
	for _, c := range cases {
		if got := Identify(c.key); got != c.want {
			t.Errorf("Identify(%q) = %+v, want %+v", c.key, got, c.want)
		}
	}
}

func TestPartNumber(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"03 - Chapter Three", 3}, {"Part 3", 3}, {"03", 3}, {"pt.12 The End", 12},
		{"Disc 2", 2}, {"Track_07", 7}, {"Chapter Three", 0}, {"Epilogue", 0},
		{"2001 A Space Odyssey", 0}, {"1984", 0}, {"100 Years", 100},
	} {
		if got := partNumber(tc.in); got != tc.want {
			t.Errorf("partNumber(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The medium table decides what the scan admits at all, case-insensitively.
func TestMediumOf(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"Movies/Cars.MKV", model.MediumVideo},
		{"Audiobooks/A/B/01.m4b", model.MediumAudio},
		{"Music/A/B/01.flac", model.MediumAudio},
		{"Books/A/B.epub", model.MediumText},
		{"Books/A/B.pdf", ""}, // a later bead
		{"Movies/Cars/poster.jpg", ""},
		{"Movies/Cars/Cars.en.srt", ""}, // sidecars are matched separately
	} {
		if got := mediumOf(tc.key); got != tc.want {
			t.Errorf("mediumOf(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
