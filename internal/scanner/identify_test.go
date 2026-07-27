package scanner

import "testing"

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
