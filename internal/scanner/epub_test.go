package scanner

import (
	"archive/zip"
	"bytes"
	"reflect"
	"testing"

	"flickr/internal/model"
)

// buildEpub assembles an in-memory EPUB from member name → content. No
// fixtures on disk: every test names exactly the files it is about.
func buildEpub(t *testing.T, members map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range members {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

const containerXMLDoc = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

// A minimal package: two spine items, no NCX, no nav document.
const bareOPF = `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" xmlns:dc="http://purl.org/dc/elements/1.1/" version="3.0">
  <metadata>
    <dc:title>The Left Hand of Darkness</dc:title>
    <dc:creator>Ursula K. Le Guin</dc:creator>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
    <item id="css" href="style.css" media-type="text/css"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
  </spine>
</package>`

func TestProbeEpubBare(t *testing.T) {
	r := buildEpub(t, map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": containerXMLDoc,
		"OEBPS/content.opf":      bareOPF,
		"OEBPS/ch1.xhtml":        "<html/>",
		"OEBPS/ch2.xhtml":        "<html/>",
	})
	info, err := ProbeEpub(r, r.Size())
	if err != nil {
		t.Fatal(err)
	}
	if info.Medium != model.MediumText || info.Container != "epub" || info.DurationSeconds != 0 {
		t.Errorf("shape: %+v", info)
	}
	// A book has no clock: nothing the decision engine reads is set.
	if info.VideoCodec != "" || info.AudioCodec != "" || info.Width != 0 || info.Height != 0 {
		t.Errorf("text item must carry no stream fields: %+v", info)
	}
	if info.Sections != 2 {
		t.Errorf("sections = %d, want 2 (the spine length)", info.Sections)
	}
	// No TOC anywhere: the spine idrefs stand in as section titles.
	want := []model.Chapter{{Title: "ch1"}, {Title: "ch2"}}
	if !reflect.DeepEqual(info.Chapters, want) {
		t.Errorf("chapters = %+v, want %+v", info.Chapters, want)
	}
	if d := info.Document; d == nil || d.Title != "The Left Hand of Darkness" || d.Creator != "Ursula K. Le Guin" || d.Language != "en" {
		t.Errorf("document metadata: %+v", info.Document)
	}
}

// EPUB 3: section titles come from the nav document's toc, resolved against
// the nav's own directory, fragments dropped, and a spine item the TOC skips
// keeps its idref. Real nav files use HTML entities and are not always
// well-formed XML — the parser must not choke on that.
func TestProbeEpubNavTitles(t *testing.T) {
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Nav Book</dc:title></metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="cover" href="text/cover.xhtml" media-type="application/xhtml+xml"/>
    <item id="c1" href="text/c1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="text/c2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="cover"/>
    <itemref idref="c1"/>
    <itemref idref="c2"/>
  </spine>
</package>`
	nav := `<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
<head><title>Contents</title><meta charset="utf-8"></head>
<body>
  <nav epub:type="landmarks"><ol><li><a href="text/cover.xhtml">Begin Reading</a></li></ol></nav>
  <nav epub:type="toc">
    <h1>Contents</h1>
    <ol>
      <li><a href="text/c1.xhtml">Chapter&nbsp;One: <em>Arrival</em></a>
        <ol><li><a href="text/c1.xhtml#part2">A sub-heading</a></li></ol>
      </li>
      <li><a href="text/c2.xhtml#top">Chapter Two</a></li>
    </ol>
  </nav>
</body>
</html>`
	r := buildEpub(t, map[string]string{
		"META-INF/container.xml": containerXMLDoc,
		"OEBPS/content.opf":      opf,
		"OEBPS/nav.xhtml":        nav,
	})
	info, err := ProbeEpub(r, r.Size())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Chapter{{Title: "cover"}, {Title: "Chapter One: Arrival"}, {Title: "Chapter Two"}}
	if !reflect.DeepEqual(info.Chapters, want) {
		t.Errorf("chapters = %+v, want %+v", info.Chapters, want)
	}
	if info.Document == nil || info.Document.Title != "Nav Book" || info.Document.Creator != "" {
		t.Errorf("document: %+v", info.Document)
	}
}

// EPUB 2: no nav document, so the NCX named by spine/@toc labels the
// sections; nested navPoints are flattened onto their documents.
func TestProbeEpubNCXTitles(t *testing.T) {
	opf := `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Old Book</dc:title></metadata>
  <manifest>
    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
    <item id="a" href="a.html" media-type="application/xhtml+xml"/>
    <item id="b" href="b.html" media-type="application/xhtml+xml"/>
  </manifest>
  <spine toc="ncx">
    <itemref idref="a"/>
    <itemref idref="b"/>
  </spine>
</package>`
	ncx := `<?xml version="1.0"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <navMap>
    <navPoint id="n1"><navLabel><text>Preface</text></navLabel><content src="a.html"/>
      <navPoint id="n2"><navLabel><text>Part I</text></navLabel><content src="b.html#p1"/></navPoint>
    </navPoint>
  </navMap>
</ncx>`
	r := buildEpub(t, map[string]string{
		"META-INF/container.xml": containerXMLDoc,
		"OEBPS/content.opf":      opf,
		"OEBPS/toc.ncx":          ncx,
	})
	info, err := ProbeEpub(r, r.Size())
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Chapter{{Title: "Preface"}, {Title: "Part I"}}
	if !reflect.DeepEqual(info.Chapters, want) {
		t.Errorf("chapters = %+v, want %+v", info.Chapters, want)
	}
}

func TestProbeEpubRejectsBrokenPackages(t *testing.T) {
	cases := []struct {
		name    string
		members map[string]string
	}{
		{"no container.xml", map[string]string{"mimetype": "application/epub+zip"}},
		{"container names a missing OPF", map[string]string{"META-INF/container.xml": containerXMLDoc}},
		{"empty spine", map[string]string{
			"META-INF/container.xml": containerXMLDoc,
			"OEBPS/content.opf":      `<package><metadata/><manifest/><spine/></package>`,
		}},
	}
	for _, c := range cases {
		r := buildEpub(t, c.members)
		if _, err := ProbeEpub(r, r.Size()); err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
	// Not a zip at all.
	junk := bytes.NewReader([]byte("this is not an epub"))
	if _, err := ProbeEpub(junk, junk.Size()); err == nil {
		t.Error("plain bytes: expected an error")
	}
}
