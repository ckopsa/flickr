package scanner

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"

	"flickr/internal/model"
)

// buildPDF assembles a classic PDF in memory: the header, objects 1..n in
// order (each given as its body without the "N 0 obj"/"endobj" wrapper), a
// correct xref table and a trailer carrying the given entries — the shape
// every writer produces. No fixtures on disk.
func buildPDF(objects []string, trailer string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d %s >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, trailer, xref)
	return b.Bytes()
}

func probePDF(t *testing.T, pdf []byte) *model.MediaInfo {
	t.Helper()
	info, err := ProbePDF(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// The proper path: trailer → Catalog → Pages → Count, and the Info
// dictionary's title and author (literal strings, escapes included).
func TestProbePDFClassic(t *testing.T) {
	pdf := buildPDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R 4 0 R 5 0 R] /Count 3 >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>`,
		`<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>`,
		`<< /Title (Dune \(1965\)) /Author (Frank\040Herbert) /Producer (hand) >>`,
	}, `/Root 1 0 R /Info 6 0 R`)
	info := probePDF(t, pdf)
	if info.Medium != model.MediumText || info.Container != "pdf" || info.PageCount != 3 {
		t.Errorf("shape: %+v", info)
	}
	// A PDF has pages, not sections, and no clock: nothing the decision
	// engine or the EPUB reader reads is set.
	if info.Sections != 0 || len(info.Chapters) != 0 || info.DurationSeconds != 0 || info.VideoCodec != "" || info.Width != 0 {
		t.Errorf("pdf must carry no section or stream fields: %+v", info)
	}
	if d := info.Document; d == nil || d.Title != "Dune (1965)" || d.Creator != "Frank Herbert" || d.Language != "" {
		t.Errorf("document: %+v", info.Document)
	}
}

// Strings come in three spellings: UTF-16BE behind a byte-order mark in a hex
// string, an indirect reference to a string object, and plain bytes. An Info
// dictionary with neither title nor author yields no Document at all.
func TestProbePDFStrings(t *testing.T) {
	pdf := buildPDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [] /Count 1 >>`,
		`<< /Title <FEFF00440075006E0065> /Author 4 0 R >>`,
		`(Ursula K. Le Guin)`,
	}, `/Root 1 0 R /Info 3 0 R`)
	info := probePDF(t, pdf)
	if d := info.Document; d == nil || d.Title != "Dune" || d.Creator != "Ursula K. Le Guin" {
		t.Errorf("document: %+v", info.Document)
	}

	pdf = buildPDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [] /Count 1 >>`,
		`<< /Producer (nothing to say) >>`,
	}, `/Root 1 0 R /Info 3 0 R`)
	if info := probePDF(t, pdf); info.Document != nil || info.PageCount != 1 {
		t.Errorf("no title, no author: %+v %+v", info, info.Document)
	}
}

func deflate(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// PDF 1.5: the cross-reference is a FlateDecode stream with a PNG predictor,
// and the Catalog, the page tree root and the Info dictionary all live inside
// a compressed object stream. Nothing probing wants is in plain text.
func TestProbePDFObjectStreams(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	off1 := b.Len()
	b.WriteString("1 0 obj\n<< /Type /Page /Parent 3 0 R >>\nendobj\n")

	// Object stream 4 holds objects 2 (Catalog), 3 (Pages) and 5 (Info).
	members := []struct {
		num  int
		body string
	}{
		{2, `<< /Type /Catalog /Pages 3 0 R >>`},
		{3, `<< /Type /Pages /Kids [1 0 R] /Count 12 >>`},
		{5, `<< /Title (Stream Book) /Author (A. Writer) >>`},
	}
	var header, data strings.Builder
	for _, m := range members {
		fmt.Fprintf(&header, "%d %d ", m.num, data.Len())
		data.WriteString(m.body + " ")
	}
	stm := deflate(t, []byte(header.String()+data.String()))
	off4 := b.Len()
	fmt.Fprintf(&b, "4 0 obj\n<< /Type /ObjStm /N %d /First %d /Length %d /Filter /FlateDecode >>\nstream\r\n", len(members), header.Len(), len(stm))
	b.Write(stm)
	b.WriteString("\r\nendstream\nendobj\n")

	// The xref stream (object 6): /W [1 4 2] rows, PNG "Up" predictor.
	off6 := b.Len()
	row := func(typ int, f2, f3 int) []byte {
		return []byte{byte(typ), byte(f2 >> 24), byte(f2 >> 16), byte(f2 >> 8), byte(f2), byte(f3 >> 8), byte(f3)}
	}
	rows := [][]byte{
		row(0, 0, 65535), // 0: free
		row(1, off1, 0),  // 1: the page, in the file
		row(2, 4, 0),     // 2: Catalog, member 0 of stream 4
		row(2, 4, 1),     // 3: Pages, member 1
		row(1, off4, 0),  // 4: the object stream
		row(2, 4, 2),     // 5: Info, member 2
		row(1, off6, 0),  // 6: this xref stream
	}
	var raw []byte
	prev := make([]byte, 7)
	for _, r := range rows {
		raw = append(raw, 2) // filter type: Up
		for i := range r {
			raw = append(raw, r[i]-prev[i])
		}
		prev = r
	}
	xs := deflate(t, raw)
	fmt.Fprintf(&b, "6 0 obj\n<< /Type /XRef /Size 7 /W [1 4 2] /Root 2 0 R /Info 5 0 R /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 7 >> /Length %d >>\nstream\n", len(xs))
	b.Write(xs)
	fmt.Fprintf(&b, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", off6)

	info := probePDF(t, b.Bytes())
	if info.PageCount != 12 {
		t.Errorf("page count = %d, want 12 (through the xref stream and the object stream)", info.PageCount)
	}
	if d := info.Document; d == nil || d.Title != "Stream Book" || d.Creator != "A. Writer" {
		t.Errorf("document: %+v", info.Document)
	}
}

// A cross-reference that lies (every offset off by the length of a comment
// slipped in after the header) is abandoned for the scan: the Catalog is
// found by its type, the trailer by its keyword.
func TestProbePDFBrokenXref(t *testing.T) {
	pdf := buildPDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [3 0 R] /Count 41 >>`,
		`<< /Type /Page /Parent 2 0 R >>`,
		`<< /Title (Shifted) >>`,
	}, `/Root 1 0 R /Info 4 0 R`)
	pdf = bytes.Replace(pdf, []byte("%PDF-1.4\n"), []byte("%PDF-1.4\n%"+strings.Repeat("x", 100)+"\n"), 1)
	info := probePDF(t, pdf)
	if info.PageCount != 41 {
		t.Errorf("page count = %d, want 41 via the scan", info.PageCount)
	}
	if d := info.Document; d == nil || d.Title != "Shifted" {
		t.Errorf("document: %+v", info.Document)
	}
}

// No trailer at all: the largest /Count of any page tree node is the root's;
// with no page tree, the page objects are counted; with nothing, 0 and no
// error. Only a missing header is a failure.
func TestProbePDFFallbacks(t *testing.T) {
	pages := "%PDF-1.4\n" +
		"2 0 obj\n<< /Type /Pages /Parent 1 0 R /Kids [] /Count 5 >>\nendobj\n" +
		"1 0 obj\n<< /Type /Pages /Kids [2 0 R] /Count 7 >>\nendobj\n"
	if info := probePDF(t, []byte(pages)); info.PageCount != 7 || info.Document != nil {
		t.Errorf("page tree without a trailer: %+v", info)
	}
	leaves := "%PDF-1.4\n" +
		"1 0 obj\n<< /Type /Page /Contents 3 0 R >>\nendobj\n" +
		"2 0 obj\n<< /Type /Page /Contents 3 0 R >>\nendobj\n" +
		"3 0 obj\n<< /Length 5 >>\nstream\nq Q q\nendstream\nendobj\n"
	if info := probePDF(t, []byte(leaves)); info.PageCount != 2 {
		t.Errorf("counted page objects: %+v", info)
	}
	if info := probePDF(t, []byte("%PDF-1.7\n%%EOF\n")); info.PageCount != 0 || info.Container != "pdf" || info.Document != nil {
		t.Errorf("header only: %+v", info)
	}
	if _, err := ProbePDF(strings.NewReader("PK\x03\x04 not a pdf"), 14); err == nil {
		t.Error("a zip is not a PDF")
	}
}

// Incremental updates append a new definition of an object and a new xref
// section pointing at it, with /Prev leading back to the old one: the newest
// wins, for the offset and for the count.
func TestProbePDFIncrementalUpdate(t *testing.T) {
	pdf := buildPDF([]string{
		`<< /Type /Catalog /Pages 2 0 R >>`,
		`<< /Type /Pages /Kids [] /Count 10 >>`,
	}, `/Root 1 0 R`)
	prev := bytes.LastIndex(pdf, []byte("xref\n"))
	var b bytes.Buffer
	b.Write(pdf)
	off := b.Len()
	b.WriteString("2 0 obj\n<< /Type /Pages /Kids [] /Count 11 >>\nendobj\n")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n2 1\n%010d 00000 n \ntrailer\n<< /Size 3 /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n", off, prev, xref)
	if info := probePDF(t, b.Bytes()); info.PageCount != 11 {
		t.Errorf("page count = %d, want the updated 11", info.PageCount)
	}
}

// The dictionary readers match whole names and read the value forms probing
// meets; the string decoders take every spelling of a PDF text string.
func TestPDFDictReaders(t *testing.T) {
	dict := `<< /Type /Pages /Pages 2 0 R /Count 3 /W [1 2 1] /Filter [/FlateDecode] /Sub << /Predictor 12 /Columns 4 >> /Name /Value#20 >>`
	if n := dictName(dict, "Type"); n != "Pages" {
		t.Errorf("Type = %q", n)
	}
	if _, ok := dictInt(dict, "Page"); ok {
		t.Error("/Page must not match /Pages")
	}
	if n, ok := dictRef(dict, "Pages"); !ok || n != 2 {
		t.Errorf("Pages ref = %d %v", n, ok)
	}
	if c, ok := dictInt(dict, "Count"); !ok || c != 3 {
		t.Errorf("Count = %d %v", c, ok)
	}
	if w := dictInts(dict, "W"); len(w) != 3 || w[0] != 1 || w[1] != 2 || w[2] != 1 {
		t.Errorf("W = %v", w)
	}
	if f := dictNames(dict, "Filter"); len(f) != 1 || f[0] != "FlateDecode" {
		t.Errorf("Filter = %v", f)
	}
	if p, ok := dictInt(dictSub(dict, "Sub"), "Predictor"); !ok || p != 12 {
		t.Errorf("Sub/Predictor = %d %v", p, ok)
	}
	for _, tc := range []struct{ in, want string }{
		{`(plain)`, "plain"},
		{`(nested (parens) kept)`, "nested (parens) kept"},
		{`(tab\tand\\slash\051)`, "tab and\\slash)"},
		{`(line\` + "\n" + `continued)`, "linecontinued"},
		{`<48656C6C6F>`, "Hello"},
		{`<FEFF00E9>`, "é"},
		{`(caf\351)`, "café"}, // PDFDocEncoding read as Latin-1
		{`<< /not /a /string >>`, ""},
	} {
		if got := decodeText(stringBytes([]byte(tc.in))); got != tc.want {
			t.Errorf("string %s = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ProbeText routes on the extension: a PDF to the PDF prober, anything else
// to the EPUB reader.
func TestProbeTextRoutes(t *testing.T) {
	pdf := buildPDF([]string{`<< /Type /Catalog /Pages 2 0 R >>`, `<< /Type /Pages /Kids [] /Count 2 >>`}, `/Root 1 0 R`)
	info, err := ProbeText(bytes.NewReader(pdf), int64(len(pdf)), "Books/A/Paper.PDF")
	if err != nil || info.Container != "pdf" || info.PageCount != 2 {
		t.Errorf("pdf route: %+v %v", info, err)
	}
	r := buildEpub(t, map[string]string{
		"META-INF/container.xml": containerXMLDoc,
		"OEBPS/content.opf":      bareOPF,
	})
	info, err = ProbeText(r, r.Size(), "Books/A/Book.epub")
	if err != nil || info.Container != "epub" || info.Sections != 2 || info.PageCount != 0 {
		t.Errorf("epub route: %+v %v", info, err)
	}
	if _, err := ProbeText(bytes.NewReader(pdf), int64(len(pdf)), "Books/A/Book.epub"); err == nil {
		t.Error("a PDF handed to the EPUB reader must fail, not pass")
	}
}
