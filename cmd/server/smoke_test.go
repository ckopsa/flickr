//go:build smoke

package main

// The browser smoke's server half. `go test -tags smoke -run TestSmokeServe
// ./cmd/server` with SMOKE_ADDR set stands the fixture library of
// hyper_test.go up on a real port — the same twelve items the goldens are
// written from — and keeps serving until SIGTERM, so scripts/browser-smoke.mjs
// can open the page in a real Chromium and walk every screen. Nothing here
// shells out: the media it serves is handed in (SMOKE_MEDIA, a directory the
// node side fills), and the book bytes are made in memory.
//
// What stands in for the bucket:
//   - presign answers /smoke/media/film.webm for a video, /smoke/media/track.wav
//     for audio, so a play's URL is a real file this server holds;
//   - /api/items/{id}/book is answered here with a generated EPUB or PDF,
//     since the real handler reads S3;
//   - the fixture's video and audio rows are rewritten to a shape the browser
//     direct-plays (mp4 / h264 / aac, flac) and six seconds long, the length
//     of the files served — a transcode would need the ffmpeg this box lacks.
//
// Without SMOKE_ADDR the test skips, and without the build tag it does not
// exist, so `go test ./...` never sees it.

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"flickr/internal/model"
)

func TestSmokeServe(t *testing.T) {
	addr := os.Getenv("SMOKE_ADDR")
	if addr == "" {
		t.Skip("SMOKE_ADDR not set: the browser smoke's server is not wanted")
	}
	media := os.Getenv("SMOKE_MEDIA")
	if media == "" {
		t.Fatal("SMOKE_MEDIA must name the directory holding film.webm and track.wav")
	}

	srv, mux := fixtureServer(t)
	shortenFixture(t, srv)
	base := "http://" + addr
	srv.baseURL = base
	srv.presign = func(_ context.Context, key string) (string, error) {
		if strings.HasSuffix(key, ".flac") || strings.HasSuffix(key, ".m4b") {
			return base + "/smoke/media/track.wav", nil
		}
		return base + "/smoke/media/film.webm", nil
	}

	// routes() serves the shell from ./web, relative to the working
	// directory, and `go test` runs in cmd/server.
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		t.Fatal(err)
	}

	epub := makeEPUB(t)
	pdf := makePDF(400)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range")
		switch {
		case r.Method == http.MethodOptions:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/smoke/media/"):
			http.ServeFile(w, r, filepath.Join(media, filepath.Base(r.URL.Path)))
		case r.URL.Path == fmt.Sprintf("/api/items/%d/book", idHillHouse):
			w.Header().Set("Content-Type", "application/epub+zip")
			http.ServeContent(w, r, "book.epub", time.Time{}, bytes.NewReader(epub))
		case r.URL.Path == fmt.Sprintf("/api/items/%d/book", idFlatland):
			w.Header().Set("Content-Type", "application/pdf")
			http.ServeContent(w, r, "book.pdf", time.Time{}, bytes.NewReader(pdf))
		default:
			mux.ServeHTTP(w, r)
		}
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	hs := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	t.Logf("smoke server on %s", base)
	if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

// shortenFixture makes every video and audio row six seconds long and
// direct-playable in a browser, matching the files the node side generates.
// Identity, enrichment and arrival dates are untouched (UpsertBatch keeps
// them), so the documents read as the goldens do apart from the lengths.
func shortenFixture(t *testing.T, s *server) {
	t.Helper()
	items, err := s.library.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	for i := range items {
		mi := items[i].MediaInfo
		if mi == nil {
			continue
		}
		switch mi.Medium {
		case model.MediumVideo:
			mi.Container, mi.VideoCodec, mi.AudioCodec = "mp4", "h264", "aac"
			mi.AudioChannels = 2
			mi.DurationSeconds = 6
			mi.Chapters = []model.Chapter{{StartSeconds: 0, Title: "Start"}, {StartSeconds: 3, Title: "Middle"}}
		case model.MediumAudio:
			mi.Container, mi.AudioCodec = "flac", "flac"
			mi.DurationSeconds = 6
		}
	}
	if err := s.library.UpsertBatch(items); err != nil {
		t.Fatal(err)
	}
}

// makeEPUB builds a small EPUB 3 whose spine has the eight sections the
// fixture's Hill House row names, so the reading session's contents and the
// book agree.
func makeEPUB(t *testing.T) []byte {
	t.Helper()
	titles := []string{"Cover", "Title Page", "Chapter One", "Chapter Two", "The Hill", "The Tower", "The Cellar", "Afterword"}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string, stored bool) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if stored {
			hdr.Method = zip.Store
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("mimetype", "application/epub+zip", true)
	add("META-INF/container.xml", `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`, false)
	var manifest, spine, nav strings.Builder
	for i, title := range titles {
		id := fmt.Sprintf("s%d", i+1)
		manifest.WriteString(fmt.Sprintf(`<item id="%s" href="%s.xhtml" media-type="application/xhtml+xml"/>`, id, id))
		spine.WriteString(fmt.Sprintf(`<itemref idref="%s"/>`, id))
		nav.WriteString(fmt.Sprintf(`<li><a href="%s.xhtml">%s</a></li>`, id, title))
		var paras strings.Builder
		for p := 0; p < 12; p++ {
			paras.WriteString(fmt.Sprintf("<p>Section %d, paragraph %d. No live organism can continue for long to exist sanely under conditions of absolute reality.</p>\n", i+1, p+1))
		}
		add("OEBPS/"+id+".xhtml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>%s</title></head>
<body><h1>%s</h1>
%s</body></html>`, title, title, paras.String()), false)
	}
	add("OEBPS/nav.xhtml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>Contents</title></head>
<body><nav epub:type="toc"><ol>%s</ol></nav></body></html>`, nav.String()), false)
	add("OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:smoke-hill-house</dc:identifier>
    <dc:title>The Haunting of Hill House</dc:title>
    <dc:creator>Shirley Jackson</dc:creator>
    <dc:language>en</dc:language>
    <meta property="dcterms:modified">2026-09-07T00:00:00Z</meta>
  </metadata>
  <manifest>%s<item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/></manifest>
  <spine>%s</spine>
</package>`, manifest.String(), spine.String()), false)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makePDF writes a PDF 1.4 of n one-line pages by hand, with a correct
// cross-reference table, which is all pdf.js asks for.
func makePDF(n int) []byte {
	var b bytes.Buffer
	var offsets []int
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.4\n")
	// 1 catalog, 2 pages, 3 font, then per page: page (4+2i) and content (5+2i)
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	var kids strings.Builder
	for i := 0; i < n; i++ {
		kids.WriteString(fmt.Sprintf("%d 0 R ", 4+2*i))
	}
	obj(fmt.Sprintf("<< /Type /Pages /Kids [ %s] /Count %d >>", kids.String(), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (Flatland, page %d of %d) Tj ET", i+1, n)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
