package passage

import (
	"testing"
)

// The cases below are web/passage_test.mjs's, under the same names, so the
// two files can be read side by side: this package is that file's grammar
// half in Go (the clock predicates and the mark state stay in the browser,
// which owns the device). A subtest named there and missing here is a case
// the server does not answer for.

// p is a passage record with every field absent except the overrides, the
// way the JS tests' P() is.
func p(fs ...func(*Passage)) *Passage {
	out := &Passage{}
	for _, f := range fs {
		f(out)
	}
	return out
}

func t_(v float64) func(*Passage)     { return func(p *Passage) { p.T = &v } }
func end(v float64) func(*Passage)    { return func(p *Passage) { p.End = &v } }
func until(v int64) func(*Passage)    { return func(p *Passage) { p.Until = &v } }
func untilEp(v string) func(*Passage) { return func(p *Passage) { p.UntilEp = v } }
func ep(v string) func(*Passage)      { return func(p *Passage) { p.Ep = v } }
func from(v string) func(*Passage)    { return func(p *Passage) { p.From = v } }
func to(v string) func(*Passage)      { return func(p *Passage) { p.To = v } }

// same compares two passages field by field, following the pointers, so a
// failure names the field rather than an address.
func same(t *testing.T, got, want *Passage) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Errorf("passage = %s, want %s", show(got), show(want))
		}
		return
	}
	if show(got) != show(want) {
		t.Errorf("passage = %s, want %s", show(got), show(want))
	}
}

func show(p *Passage) string {
	if p == nil {
		return "nil"
	}
	s := "{"
	if p.T != nil {
		s += "t=" + Seconds(*p.T) + " "
	}
	if p.End != nil {
		s += "end=" + Seconds(*p.End) + " "
	}
	if p.Until != nil {
		s += "until=" + Seconds(float64(*p.Until)) + " "
	}
	if p.UntilEp != "" {
		s += "untilEp=" + p.UntilEp + " "
	}
	if p.Ep != "" {
		s += "ep=" + p.Ep + " "
	}
	if p.From != "" {
		s += "from=" + p.From + " "
	}
	if p.To != "" {
		s += "to=" + p.To + " "
	}
	return s + "}"
}

func TestSplitHashSeparatesThePathFromItsQuery(t *testing.T) {
	for _, tc := range []struct{ hash, path, query string }{
		{"#/item/51?t=4740&end=5070", "#/item/51", "t=4740&end=5070"},
		{"#/item/51", "#/item/51", ""},
		{"#/show/Some%3FTitle", "#/show/Some%3FTitle", ""},
		{"#/show/Ninjago?ep=S02E05&t=60", "#/show/Ninjago", "ep=S02E05&t=60"},
		{"", "", ""},
	} {
		path, query := Split(tc.hash)
		if path != tc.path || query != tc.query {
			t.Errorf("Split(%q) = %q, %q; want %q, %q", tc.hash, path, query, tc.path, tc.query)
		}
	}
}

func TestParsePassage(t *testing.T) {
	t.Run("a scene", func(t *testing.T) {
		same(t, Parse("t=4740&end=5070"), p(t_(4740), end(5070)))
		same(t, Parse("?t=4740&end=5070"), p(t_(4740), end(5070)))
	})

	t.Run("decimals are seconds too", func(t *testing.T) {
		got := Parse("t=79.5&end=330.25")
		if *got.T != 79.5 || *got.End != 330.25 {
			t.Errorf("got %s", show(got))
		}
	})

	t.Run("an episode run by item id", func(t *testing.T) {
		same(t, Parse("t=120&until=61"), p(t_(120), until(61)))
		same(t, Parse("until=61&end=900"), p(end(900), until(61)))
	})

	t.Run("the show form keeps ep and an episode-code until as written", func(t *testing.T) {
		same(t, Parse("ep=S02E05&t=60&end=300"), p(t_(60), end(300), ep("S02E05")))
		same(t, Parse("ep=s2e5&until=S02E07"), p(ep("s2e5"), untilEp("S02E07")))
		same(t, Parse("ep=nonsense"), p(ep("nonsense"))) // judged at resolution, not here
	})

	t.Run("text locators are preserved, decoded, and not timed", func(t *testing.T) {
		got := Parse("from=epubcfi(%2F6%2F4!%2F4%2F2)&to=ch03")
		same(t, got, p(from("epubcfi(/6/4!/4/2)"), to("ch03")))
		if got.IsTimed() {
			t.Error("a text passage is not timed")
		}
		if !Parse("t=1").IsTimed() || !Parse("until=9").IsTimed() {
			t.Error("t and until are timed")
		}
		if (*Passage)(nil).IsTimed() {
			t.Error("no passage is not timed")
		}
	})

	t.Run("nothing there is null, garbage is absent", func(t *testing.T) {
		same(t, Parse(""), nil)
		same(t, Parse("foo=bar"), nil)
		same(t, Parse("t=abc&end=-5"), nil)
		same(t, Parse("t=abc&end=90"), p(end(90)))
		same(t, Parse("t=&end=&until=&ep="), nil)
	})

	t.Run("an end at or before the start is dropped", func(t *testing.T) {
		if got := Parse("t=100&end=100"); got.End != nil {
			t.Errorf("end = %s", show(got))
		}
		got := Parse("t=100&end=90")
		if got.End != nil || *got.T != 100 {
			t.Errorf("got %s", show(got))
		}
	})
}

func TestPassageQueryRoundTripsAndIsStable(t *testing.T) {
	for _, q := range []string{
		"?t=4740&end=5070", "?t=120&until=61", "?end=900&until=61",
		"?t=79.5&end=330.25", "?from=epubcfi(%2F6%2F4!%2F4%2F2)&to=ch03",
		"?t=60&end=300&until=S02E07&ep=S02E05",
	} {
		if got := Parse(q).Query(); got != q {
			t.Errorf("round trip of %q = %q", q, got)
		}
	}
	if got := (*Passage)(nil).Query(); got != "" {
		t.Errorf("no passage spells %q", got)
	}
	if got := Parse("foo=1").Query(); got != "" {
		t.Errorf("no passage spells %q", got)
	}
	// field order is fixed regardless of input order
	if got := Parse("until=61&t=120").Query(); got != "?t=120&until=61" {
		t.Errorf("field order = %q", got)
	}
}

func TestPassageForNextKeepsUntilAndDropsTheFirstItemsTEnd(t *testing.T) {
	same(t, Parse("t=120&end=900&until=61").ForNext(), p(until(61)))
	if got := Parse("t=120&until=61").ForNext().Query(); got != "?until=61" {
		t.Errorf("next = %q", got)
	}
	same(t, Parse("t=120&end=900").ForNext(), nil)
	same(t, (*Passage)(nil).ForNext(), nil)
}

// ── show-addressed passages ─────────────────────────────────────────────

// a show's members as the library lists them: zero season/episode omitted,
// bonus material carrying an episode-shaped name
var showMembers = []Member{
	{ID: 40, Kind: "episode", Season: 1, Episode: 1},
	{ID: 41, Kind: "episode", Season: 1, Episode: 2},
	{ID: 58, Kind: "episode", Season: 2, Episode: 5},
	{ID: 59, Kind: "episode", Season: 2, Episode: 6},
	{ID: 60, Kind: "episode", Season: 2, Episode: 7},
	{ID: 61, Kind: "episode"},                      // unnumbered rip
	{ID: 70, Kind: "extra", Season: 2, Episode: 5}, // deleted scene named S02E05
}

func TestParseEpisodeCodeReadsSxxEyyInAnyCaseAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Episode
		ok   bool
	}{
		{"S02E05", Episode{2, 5}, true},
		{"s2e5", Episode{2, 5}, true},
		{" S10E123 ", Episode{10, 123}, true},
		{"E05", Episode{}, false},
		{"S02E05x", Episode{}, false},
		{"", Episode{}, false},
	} {
		got, ok := ParseEpisodeCode(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseEpisodeCode(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFindEpisodeMatchesNumbersNeverBonusMaterial(t *testing.T) {
	id := func(m *Member) int64 {
		if m == nil {
			return 0
		}
		return m.ID
	}
	if got := id(FindEpisode(showMembers, Episode{2, 5})); got != 58 {
		t.Errorf("S02E05 = %d", got)
	}
	if got := id(FindEpisode(showMembers, Episode{1, 1})); got != 40 {
		t.Errorf("S01E01 = %d", got)
	}
	if got := id(FindEpisode(showMembers, Episode{2, 9})); got != 0 {
		t.Errorf("S02E09 = %d", got)
	}
	if got := id(FindEpisode(showMembers, Episode{0, 0})); got != 61 {
		t.Errorf("an unnumbered file is S00E00: %d", got)
	}
	if got := id(FindEpisode(nil, Episode{2, 5})); got != 0 {
		t.Errorf("an empty show = %d", got)
	}
}

func TestResolveShowPassage(t *testing.T) {
	t.Run("a scene of one episode becomes the item form", func(t *testing.T) {
		item, got, err := ResolveShow(Parse("ep=S02E05&t=60&end=300"), showMembers)
		if err != nil {
			t.Fatal(err)
		}
		if item.ID != 58 {
			t.Errorf("item = %d", item.ID)
		}
		same(t, got, p(t_(60), end(300)))
		if q := got.Query(); q != "?t=60&end=300" {
			t.Errorf("query = %q", q)
		}
	})

	t.Run("a run resolves until to the last episode's id", func(t *testing.T) {
		item, got, err := ResolveShow(Parse("ep=s2e5&t=120&until=S02E07"), showMembers)
		if err != nil {
			t.Fatal(err)
		}
		if item.ID != 58 {
			t.Errorf("item = %d", item.ID)
		}
		same(t, got, p(t_(120), until(60)))
		// an item-id until passes through untouched
		_, got, err = ResolveShow(Parse("ep=S02E05&until=60"), showMembers)
		if err != nil || *got.Until != 60 {
			t.Errorf("until = %s (%v)", show(got), err)
		}
	})

	t.Run("ep alone is a plain episode route", func(t *testing.T) {
		item, got, err := ResolveShow(Parse("ep=S01E02"), showMembers)
		if err != nil {
			t.Fatal(err)
		}
		if item.ID != 41 {
			t.Errorf("item = %d", item.ID)
		}
		if q := got.Query(); q != "" {
			t.Errorf("query = %q", q)
		}
		if got.IsTimed() {
			t.Error("ep alone is not a timed passage")
		}
	})

	t.Run("an ep that names no episode is one sentence, no item", func(t *testing.T) {
		for _, tc := range []struct{ query, want string }{
			{"ep=S02E09&t=60", "This show has no episode S02E09."},
			{"ep=finale&t=60", `"finale" is not an episode code like S02E05.`},
			{"ep=S02E05&until=S02E99", "This show has no episode S02E99 to run until."},
			{"t=60", `"" is not an episode code like S02E05.`}, // no ep at all
		} {
			item, got, err := ResolveShow(Parse(tc.query), showMembers)
			if item != nil || got != nil {
				t.Errorf("%s resolved to %v %s", tc.query, item, show(got))
			}
			if err == nil || err.Error() != tc.want {
				t.Errorf("%s: error = %v, want %q", tc.query, err, tc.want)
			}
		}
	})
}

// ── text locators ───────────────────────────────────────────────────────

func TestParseLocatorReadsTheSpellingsAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Locator
	}{
		{"cfi:epubcfi(/6/14!/4/2/1:0)", Locator{Kind: "cfi", CFI: "epubcfi(/6/14!/4/2/1:0)"}},
		{"epubcfi(/6/14!/4/2)", Locator{Kind: "cfi", CFI: "epubcfi(/6/14!/4/2)"}}, // bare, as progress reports it
		{"ch:7", Locator{Kind: "ch", N: 7}},
		{"CH:7", Locator{Kind: "ch", N: 7}},
		{"pct:0.34", Locator{Kind: "pct", F: 0.34}},
		{"pct:1", Locator{Kind: "pct", F: 1}},
		{"pct:0", Locator{Kind: "pct", F: 0}},
		{"pct:.5", Locator{Kind: "pct", F: 0.5}},
		{" ch:3 ", Locator{Kind: "ch", N: 3}},
		{"pg:213", Locator{Kind: "pg", N: 213}},
		{"PG:1", Locator{Kind: "pg", N: 1}},
	} {
		got := ParseLocator(tc.in)
		if got == nil || *got != tc.want {
			t.Errorf("ParseLocator(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{
		"ch:0", "ch:-1", "ch:3.5", "ch:", "pct:1.5", "pct:-0.1", "pct:abc", "cfi:nope",
		"ch03", "page:12", "pg:0", "pg:-2", "pg:3.5", "pg:", "pg:abc", "p:12", "", "cfi:",
	} {
		if got := ParseLocator(bad); got != nil {
			t.Errorf("%q should not parse, got %v", bad, got)
		}
	}
}

func TestFormatLocatorIsTheCanonicalSpellingAndRoundTrips(t *testing.T) {
	for _, s := range []string{"cfi:epubcfi(/6/14!/4/2/1:0)", "ch:7", "pct:0.34", "pg:213"} {
		if got := ParseLocator(s).String(); got != s {
			t.Errorf("round trip of %q = %q", s, got)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"PG:9", "pg:9"},
		{"epubcfi(/6/2!/4)", "cfi:epubcfi(/6/2!/4)"},
		{"CH:7", "ch:7"},
	} {
		if got := ParseLocator(tc.in).String(); got != tc.want {
			t.Errorf("%q spells %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (*Locator)(nil).String(); got != "" {
		t.Errorf("no locator spells %q", got)
	}
	if got := (&Locator{Kind: "nonsense"}).String(); got != "" {
		t.Errorf("a nonsense locator spells %q", got)
	}
}

func TestSectionFromCFIReadsTheSpineStep(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"epubcfi(/6/14!/4/2/1:0)", 7},
		{"epubcfi(/6/14[ch07]!/4/2/1:0)", 7},
		{"epubcfi(/6/2!/4/2)", 1},
		{" epubcfi(/6/40!/4) ", 20},
		{"epubcfi(/6/13!/4)", 0}, // odd: not an element step
		{"epubcfi(/6)", 0},
		{"/6/14!/4", 0},
		{"", 0},
	} {
		if got := SectionFromCFI(tc.in); got != tc.want {
			t.Errorf("SectionFromCFI(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestLocatorSectionLandsEveryKindInASection(t *testing.T) {
	for _, tc := range []struct {
		in       string
		sections int
		want     int
	}{
		{"ch:3", 12, 3},
		{"ch:99", 12, 12},
		{"pct:0", 12, 1},
		{"pct:0.5", 12, 7},
		{"pct:1", 12, 12},
		{"cfi:epubcfi(/6/14!/4/2)", 12, 7},
		{"cfi:epubcfi(/6/14!/4/2)", 3, 3},
		{"cfi:epubcfi(/6)", 12, 0},
		{"ch:3", 0, 0},
		{"pg:3", 12, 0}, // a page says nothing about sections
	} {
		if got := ParseLocator(tc.in).Section(tc.sections); got != tc.want {
			t.Errorf("%q in a book of %d = %d, want %d", tc.in, tc.sections, got, tc.want)
		}
	}
	if got := (*Locator)(nil).Section(12); got != 0 {
		t.Errorf("no locator lands in %d", got)
	}
}

func TestLocatorPageLandsEveryKindOnAPage(t *testing.T) {
	for _, tc := range []struct {
		in    string
		pages int
		want  int
	}{
		{"pg:213", 400, 213},
		{"pg:999", 400, 400},
		{"pct:0", 400, 1},
		{"pct:0.5", 400, 201},
		{"pct:1", 400, 400},
		{"pg:3", 0, 0},                      // the file would not say how many pages
		{"ch:3", 400, 0},                    // a section says nothing about pages
		{"cfi:epubcfi(/6/14!/4/2)", 400, 0}, // nor does a CFI
	} {
		if got := ParseLocator(tc.in).Page(tc.pages); got != tc.want {
			t.Errorf("%q in a PDF of %d pages = %d, want %d", tc.in, tc.pages, got, tc.want)
		}
	}
	if got := (*Locator)(nil).Page(400); got != 0 {
		t.Errorf("no locator lands on page %d", got)
	}
}

func TestIsTextPassageFromOrToAndNeverATimedOne(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{"from=ch:3&to=ch:4", true},
		{"from=ch:3", true},
		{"to=pct:0.5", true},
		{"t=10&end=20", false},
		{"until=9", false},
	} {
		if got := Parse(tc.query).IsText(); got != tc.want {
			t.Errorf("IsText(%q) = %v", tc.query, got)
		}
	}
	if (*Passage)(nil).IsText() {
		t.Error("no passage is not a text passage")
	}
	// the two grammars ride the same query without meeting
	got := Parse("from=ch:3&to=ch:4")
	if got.IsTimed() {
		t.Error("a text passage is not timed")
	}
	if q := got.Query(); q != "?from=ch%3A3&to=ch%3A4" {
		t.Errorf("query = %q", q)
	}
	same(t, Parse(got.Query()), got)
}
