package scanner

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"flickr/internal/model"
)

// A PDF is a graph of numbered objects with a cross-reference table at the
// end saying where each one starts: the trailer names the Catalog, the
// Catalog names the root of the page tree, and the root's /Count is the page
// count. Probing follows exactly that path — no rendering, no fonts, no
// content streams, no external tool — with the tolerance real files need:
// the cross-reference may be a compressed stream (PDF 1.5) instead of a
// table, the objects may sit inside object streams, the offsets may be wrong
// (files edited by hand or by careless tools) and the trailer may be missing
// altogether. When the path breaks the file is scanned for the page tree
// instead, and when even that finds nothing the count is 0 — unknown, never
// a failed probe: the reader (pdf.js) counts for itself, the prober's count
// is for progress_text and tiles.

const (
	pdfHeaderWindow = 1024      // the spec lets %PDF- sit anywhere in the first 1024 bytes
	pdfTailWindow   = 2048      // startxref lives in the last few lines
	pdfObjectWindow = 64 << 10  // first read of an object; grown until it ends
	pdfObjectMax    = 64 << 20  // an object longer than this is not one probing wants
	pdfScanMax      = 256 << 20 // the whole-file fallback reads the file into memory
	pdfXrefMax      = 64        // xref sections followed through /Prev before giving up
	pdfDepthMax     = 16        // object resolutions nested inside one another
)

// ProbePDF reads a PDF's page count and Info dictionary from r (size bytes)
// and maps them to a text MediaInfo: Medium "text", Container "pdf",
// PageCount (0 when the file would not say) and Document{Title, Creator}
// from the Info dictionary's /Title and /Author when it carries them. The
// only error is "not a PDF": a file with the header but a page tree probing
// cannot find is a PDF of unknown length, not a broken one.
func ProbePDF(r io.ReaderAt, size int64) (*model.MediaInfo, error) {
	head := readAt(r, 0, min(size, pdfHeaderWindow))
	if !bytes.Contains(head, []byte("%PDF-")) {
		return nil, errors.New("pdf: no %PDF- header")
	}
	d := &pdfDoc{r: r, size: size, xref: map[int]xrefEntry{}, objstms: map[int]map[int]string{}}
	d.loadXrefChain()
	info := &model.MediaInfo{Medium: model.MediumText, Container: "pdf", PageCount: d.pageCount()}
	if doc := d.document(); doc != nil {
		info.Document = doc
	}
	return info, nil
}

// xrefEntry is where one object lives: at a byte offset (type 1), or as the
// index-th member of an object stream (type 2).
type xrefEntry struct {
	offset   int64
	inStream bool
	stream   int
	index    int
}

// pdfObject is one object as read: its text (a dictionary for everything
// probing looks at; a bare value for an indirect string or integer) and its
// raw stream data when it has one.
type pdfObject struct {
	dict   string
	stream []byte
}

type pdfDoc struct {
	r    io.ReaderAt
	size int64
	// The trailer route: the cross-reference chain from startxref, newest
	// section first, each object number kept from the first section that
	// names it (the newest); the trailer dictionaries in the same order.
	xref     map[int]xrefEntry
	trailers []string
	objstms  map[int]map[int]string // parsed object streams: stream → member number → body
	depth    int
	// The scan route, taken when the chain is missing or lies: every object
	// found by reading the whole file, last definition winning, object
	// stream members included; the trailers found the same way, newest first.
	scanned      bool
	bodies       map[int]*pdfObject
	scanTrailers []string
}

// --- the cross-reference chain -----------------------------------------------

func (d *pdfDoc) loadXrefChain() {
	tail := readAt(d.r, max(0, d.size-pdfTailWindow), pdfTailWindow)
	i := bytes.LastIndex(tail, []byte("startxref"))
	if i < 0 {
		return
	}
	start, ok := leadingInt(string(tail[i+len("startxref"):]))
	if !ok {
		return
	}
	seen := map[int64]bool{}
	queue := []int64{int64(start)}
	for len(queue) > 0 && len(seen) < pdfXrefMax {
		off := queue[0]
		queue = queue[1:]
		if off <= 0 || off >= d.size || seen[off] {
			continue
		}
		seen[off] = true
		trailer, next := d.loadXrefSection(off)
		if trailer == "" {
			continue
		}
		d.trailers = append(d.trailers, trailer)
		queue = append(queue, next...)
	}
}

// loadXrefSection reads one cross-reference section — a classic table or an
// xref stream object — into d.xref and returns its trailer dictionary and
// the sections it points on to (a hybrid table's /XRefStm first, then /Prev,
// so the first-seen rule keeps the right entry). "" when nothing readable is
// at off.
func (d *pdfDoc) loadXrefSection(off int64) (trailer string, next []int64) {
	win := readAt(d.r, off, pdfObjectWindow)
	t := bytes.TrimLeft(win, " \t\r\n\f\x00")
	if bytes.HasPrefix(t, []byte("xref")) {
		return d.loadXrefTable(off + int64(len(win)-len(t)))
	}
	obj, err := d.readObjectAt(off, -1)
	if err != nil || obj.stream == nil || dictName(obj.dict, "Type") != "XRef" {
		return "", nil
	}
	if err := d.loadXrefStream(obj); err != nil {
		return "", nil
	}
	if p, ok := dictInt(obj.dict, "Prev"); ok {
		next = append(next, int64(p))
	}
	return obj.dict, next
}

// loadXrefTable parses "xref <subsections> trailer <<…>>" at off. Entries
// are whitespace-separated tokens (the spec's fixed 20-byte lines are the
// common case, but not every writer counts bytes), so they are tokenized
// rather than sliced.
func (d *pdfDoc) loadXrefTable(off int64) (string, []int64) {
	var buf []byte
	var ti int
	for n := int64(pdfObjectWindow); ; n *= 4 {
		buf = readAt(d.r, off, n)
		ti = bytes.Index(buf, []byte("trailer"))
		if ti >= 0 {
			if end := dictEnd(buf, ti+len("trailer")); end > 0 {
				buf = buf[:end]
				break
			}
		}
		if int64(len(buf)) < n || n >= pdfObjectMax {
			return "", nil
		}
	}
	toks := strings.Fields(string(buf[len("xref"):ti]))
	for i := 0; i+1 < len(toks); {
		start, e1 := strconv.Atoi(toks[i])
		count, e2 := strconv.Atoi(toks[i+1])
		if e1 != nil || e2 != nil || start < 0 || count < 0 {
			break
		}
		i += 2
		for k := 0; k < count && i+2 < len(toks); k++ {
			o, err := strconv.ParseInt(toks[i], 10, 64)
			typ := toks[i+2]
			i += 3
			if err != nil || typ != "n" {
				continue
			}
			if _, dup := d.xref[start+k]; !dup {
				d.xref[start+k] = xrefEntry{offset: o}
			}
		}
	}
	trailer := string(buf[ti+len("trailer"):])
	var next []int64
	if s, ok := dictInt(trailer, "XRefStm"); ok {
		next = append(next, int64(s))
	}
	if p, ok := dictInt(trailer, "Prev"); ok {
		next = append(next, int64(p))
	}
	return trailer, next
}

// loadXrefStream parses a PDF 1.5 cross-reference stream: rows of /W-sized
// big-endian fields (type, offset-or-stream, generation-or-index) covering
// the object ranges /Index lists (default: 0 to /Size).
func (d *pdfDoc) loadXrefStream(obj *pdfObject) error {
	data, err := decodeStream(obj.dict, obj.stream)
	if err != nil {
		return err
	}
	w := dictInts(obj.dict, "W")
	for len(w) < 3 {
		w = append(w, 0)
	}
	rowLen := 0
	for _, x := range w[:3] {
		if x < 0 || x > 8 {
			return fmt.Errorf("pdf: xref stream /W %v", w)
		}
		rowLen += x
	}
	if rowLen == 0 {
		return errors.New("pdf: xref stream without /W")
	}
	index := dictInts(obj.dict, "Index")
	if len(index) < 2 {
		size, _ := dictInt(obj.dict, "Size")
		index = []int{0, size}
	}
	pos := 0
	for i := 0; i+1 < len(index); i += 2 {
		start, count := index[i], index[i+1]
		for k := 0; k < count; k++ {
			if pos+rowLen > len(data) {
				return nil // a short stream: what it did say still counts
			}
			row := data[pos : pos+rowLen]
			pos += rowLen
			typ := 1 // /W [0 …]: every row is an in-use entry
			if w[0] > 0 {
				typ = int(beUint(row[:w[0]]))
			}
			f2 := beUint(row[w[0] : w[0]+w[1]])
			f3 := beUint(row[w[0]+w[1] : rowLen])
			num := start + k
			if _, dup := d.xref[num]; dup {
				continue
			}
			switch typ {
			case 1:
				d.xref[num] = xrefEntry{offset: int64(f2)}
			case 2:
				d.xref[num] = xrefEntry{inStream: true, stream: int(f2), index: int(f3)}
			}
		}
	}
	return nil
}

// --- objects -----------------------------------------------------------------

var objHeaderRe = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+obj\b`)

// readObjectAt reads the object at off, which must be number want (-1 for
// any): a wrong number means the cross-reference lied about the offset.
func (d *pdfDoc) readObjectAt(off int64, want int) (*pdfObject, error) {
	if off < 0 || off >= d.size {
		return nil, fmt.Errorf("pdf: offset %d outside the file", off)
	}
	for n := int64(pdfObjectWindow); ; n *= 4 {
		win := readAt(d.r, off, n)
		m := objHeaderRe.FindSubmatchIndex(win)
		if m == nil {
			return nil, fmt.Errorf("pdf: no object at offset %d", off)
		}
		if want >= 0 {
			if num, _ := strconv.Atoi(string(win[m[2]:m[3]])); num != want {
				return nil, fmt.Errorf("pdf: object %d expected at offset %d, found %d", want, off, num)
			}
		}
		if obj, ok := parseObject(win[m[1]:]); ok {
			return obj, nil
		}
		if int64(len(win)) < n || n >= pdfObjectMax {
			return nil, fmt.Errorf("pdf: object at offset %d never ends", off)
		}
	}
}

// parseObject reads an object's body (the bytes after "N G obj"): its
// dictionary or bare value, and its stream data up to endstream. ok=false
// when the body is not complete within b.
func parseObject(b []byte) (*pdfObject, bool) {
	i := skipWS(b, 0)
	if !bytes.HasPrefix(b[i:], []byte("<<")) {
		// A bare value (an indirect integer or string): runs to endobj.
		e := bytes.Index(b, []byte("endobj"))
		if e < 0 {
			return nil, false
		}
		return &pdfObject{dict: strings.TrimSpace(string(b[:e]))}, true
	}
	end := dictEnd(b, i)
	if end < 0 {
		return nil, false
	}
	obj := &pdfObject{dict: string(b[i:end])}
	j := skipWS(b, end)
	if !bytes.HasPrefix(b[j:], []byte("stream")) {
		return obj, true
	}
	j += len("stream")
	if j < len(b) && b[j] == '\r' {
		j++
	}
	if j < len(b) && b[j] == '\n' {
		j++
	}
	// The data runs to endstream. /Length would say so too, but it may be an
	// indirect object of its own (and is wrong in the files that get here);
	// the keyword cannot occur inside compressed data.
	e := bytes.Index(b[j:], []byte("endstream"))
	if e < 0 {
		return nil, false
	}
	obj.stream = bytes.TrimRight(b[j:j+e], "\r\n")
	return obj, true
}

// resolve finds object num: through the cross-reference when it has the
// object and tells the truth, else by scanning the file.
func (d *pdfDoc) resolve(num int) *pdfObject {
	if d.depth >= pdfDepthMax {
		return nil
	}
	d.depth++
	defer func() { d.depth-- }()
	if e, ok := d.xref[num]; ok {
		if e.inStream {
			if body := d.objStmMember(e.stream, num); body != "" {
				return &pdfObject{dict: body}
			}
		} else if obj, err := d.readObjectAt(e.offset, num); err == nil {
			return obj
		}
	}
	d.scanAll()
	return d.bodies[num]
}

// objStmMember is object num's body from object stream stm, parsed once.
func (d *pdfDoc) objStmMember(stm, num int) string {
	members, ok := d.objstms[stm]
	if !ok {
		members = map[int]string{}
		d.objstms[stm] = members // registered first: a stream naming itself asks once
		if obj := d.resolve(stm); obj != nil && obj.stream != nil {
			parseObjStm(obj, members)
		}
	}
	return members[num]
}

// parseObjStm splits an object stream into its members: /N pairs of
// "number offset" head the decoded data, offsets relative to /First.
func parseObjStm(obj *pdfObject, into map[int]string) {
	data, err := decodeStream(obj.dict, obj.stream)
	if err != nil {
		return
	}
	n, _ := dictInt(obj.dict, "N")
	first, _ := dictInt(obj.dict, "First")
	if first <= 0 || first > len(data) {
		return
	}
	toks := strings.Fields(string(data[:first]))
	type pair struct{ num, off int }
	var pairs []pair
	for i := 0; i+1 < len(toks) && len(pairs) < n; i += 2 {
		num, e1 := strconv.Atoi(toks[i])
		off, e2 := strconv.Atoi(toks[i+1])
		if e1 != nil || e2 != nil {
			break
		}
		pairs = append(pairs, pair{num, off})
	}
	for i, p := range pairs {
		start, end := first+p.off, len(data)
		if i+1 < len(pairs) {
			end = first + pairs[i+1].off
		}
		if start < 0 || start > end || end > len(data) {
			continue
		}
		into[p.num] = strings.TrimSpace(string(data[start:end]))
	}
}

// --- the scan route ------------------------------------------------------------

var objStartRe = regexp.MustCompile(`(?:^|[^0-9])(\d+)\s+(\d+)\s+obj\b`)

// scanAll reads the whole file (once, and only up to pdfScanMax bytes) and
// indexes every "N G obj" it finds, last definition winning — the order an
// incremental update relies on — with object stream members joining at the
// stream's position; and every trailer, newest first.
func (d *pdfDoc) scanAll() {
	if d.scanned {
		return
	}
	d.scanned = true
	d.bodies = map[int]*pdfObject{}
	if d.size > pdfScanMax {
		return
	}
	buf := readAt(d.r, 0, d.size)
	locs := objStartRe.FindAllSubmatchIndex(buf, -1)
	for i, m := range locs {
		num, err := strconv.Atoi(string(buf[m[2]:m[3]]))
		if err != nil {
			continue
		}
		end := len(buf)
		if i+1 < len(locs) {
			end = locs[i+1][2]
		}
		obj, ok := parseObject(buf[m[1]:end])
		if !ok {
			continue
		}
		d.bodies[num] = obj
		if obj.stream != nil && dictName(obj.dict, "Type") == "ObjStm" {
			members := map[int]string{}
			parseObjStm(obj, members)
			for n, body := range members {
				d.bodies[n] = &pdfObject{dict: body}
			}
		}
		if obj.stream != nil && dictName(obj.dict, "Type") == "XRef" {
			d.scanTrailers = append(d.scanTrailers, obj.dict)
		}
	}
	for from := 0; ; {
		i := bytes.Index(buf[from:], []byte("trailer"))
		if i < 0 {
			break
		}
		from += i + len("trailer")
		if end := dictEnd(buf, from); end > 0 {
			d.scanTrailers = append(d.scanTrailers, string(buf[from:end]))
			from = end
		}
	}
	slices.Reverse(d.scanTrailers)
}

// trailerRef is the object number a trailer names under key (/Root, /Info):
// the chain's trailers first, the scanned ones when the chain has none.
func (d *pdfDoc) trailerRef(key string) (int, bool) {
	for _, t := range d.trailers {
		if n, ok := dictRef(t, key); ok {
			return n, true
		}
	}
	d.scanAll()
	for _, t := range d.scanTrailers {
		if n, ok := dictRef(t, key); ok {
			return n, true
		}
	}
	return 0, false
}

// --- what probing wants -------------------------------------------------------

// pageCount walks trailer → Catalog → /Pages → /Count; failing that it finds
// the catalog by its /Type; failing that the largest /Count of any page tree
// node (the root's counts every leaf beneath it); failing that it counts the
// page objects themselves. 0 when the file yields none of these.
func (d *pdfDoc) pageCount() int {
	if root, ok := d.trailerRef("Root"); ok {
		if n := d.countViaCatalog(d.resolve(root)); n > 0 {
			return n
		}
	}
	d.scanAll()
	nums := slices.Sorted(maps.Keys(d.bodies))
	for _, num := range nums {
		if dictName(d.bodies[num].dict, "Type") == "Catalog" {
			if n := d.countViaCatalog(d.bodies[num]); n > 0 {
				return n
			}
		}
	}
	best, pages := 0, 0
	for _, num := range nums {
		switch dictName(d.bodies[num].dict, "Type") {
		case "Pages":
			if c, ok := dictInt(d.bodies[num].dict, "Count"); ok && c > best {
				best = c
			}
		case "Page":
			pages++
		}
	}
	if best > 0 {
		return best
	}
	return pages
}

func (d *pdfDoc) countViaCatalog(cat *pdfObject) int {
	if cat == nil {
		return 0
	}
	ref, ok := dictRef(cat.dict, "Pages")
	if !ok {
		return 0
	}
	root := d.resolve(ref)
	if root == nil {
		return 0
	}
	if c, ok := dictInt(root.dict, "Count"); ok && c > 0 {
		return c
	}
	return 0
}

// document is the Info dictionary's /Title and /Author, or nil when the
// file names neither.
func (d *pdfDoc) document() *model.Document {
	ref, ok := d.trailerRef("Info")
	if !ok {
		return nil
	}
	info := d.resolve(ref)
	if info == nil {
		return nil
	}
	doc := &model.Document{Title: d.textOf(info.dict, "Title"), Creator: d.textOf(info.dict, "Author")}
	if *doc == (model.Document{}) {
		return nil
	}
	return doc
}

// textOf is the decoded string under key: a literal (…), a hex <…>, or an
// indirect reference to an object holding one.
func (d *pdfDoc) textOf(dict, key string) string {
	i := keyIndex(dict, key)
	if i < 0 {
		return ""
	}
	v := strings.TrimLeft(dict[i:], " \t\r\n\f\x00")
	if n, ok := dictRef(dict, key); ok {
		if obj := d.resolve(n); obj != nil {
			v = obj.dict
		} else {
			return ""
		}
	}
	return decodeText(stringBytes([]byte(v)))
}

// --- dictionary reading --------------------------------------------------------
//
// Probing never needs a full object parser: the handful of keys it reads
// are found by name in the dictionary's text and their values read from
// there. Nested dictionaries can shadow a key in theory; in the objects
// probing looks at (Catalog, Pages, Info, XRef, ObjStm) they do not.

const pdfDelim = " \t\r\n\f\x00()<>[]{}/%"

// keyIndexes are the indexes just past every "/key" in dict — the key as a
// whole name, so /Page does not match /Pages. A name VALUE spelled like the
// key (/Type /Pages in a page tree node) is a candidate too, so the readers
// try each until one is followed by a value of the shape they want.
func keyIndexes(dict, key string) []int {
	needle := "/" + key
	var out []int
	for from := 0; ; {
		i := strings.Index(dict[from:], needle)
		if i < 0 {
			return out
		}
		end := from + i + len(needle)
		if end == len(dict) || strings.IndexByte(pdfDelim, dict[end]) >= 0 {
			out = append(out, end)
		}
		from = end
	}
}

// keyIndex is the first of keyIndexes, or -1.
func keyIndex(dict, key string) int {
	if is := keyIndexes(dict, key); len(is) > 0 {
		return is[0]
	}
	return -1
}

var (
	refRe  = regexp.MustCompile(`^\s*(\d+)\s+\d+\s+R\b`)
	nameRe = regexp.MustCompile(`^\s*/([^ \t\r\n\f\x00()<>\[\]{}/%]+)`)
	arrRe  = regexp.MustCompile(`^\s*\[([^\]]*)\]`)
)

func dictInt(dict, key string) (int, bool) {
	for _, i := range keyIndexes(dict, key) {
		if n, ok := leadingInt(dict[i:]); ok {
			return n, true
		}
	}
	return 0, false
}

func dictRef(dict, key string) (int, bool) {
	for _, i := range keyIndexes(dict, key) {
		if m := refRe.FindStringSubmatch(dict[i:]); m != nil {
			n, err := strconv.Atoi(m[1])
			return n, err == nil
		}
	}
	return 0, false
}

func dictName(dict, key string) string {
	for _, i := range keyIndexes(dict, key) {
		if m := nameRe.FindStringSubmatch(dict[i:]); m != nil {
			return m[1]
		}
	}
	return ""
}

// dictNames reads a name or an array of names (/Filter takes both forms).
func dictNames(dict, key string) []string {
	for _, i := range keyIndexes(dict, key) {
		if m := arrRe.FindStringSubmatch(dict[i:]); m != nil {
			var names []string
			for _, f := range strings.Fields(strings.ReplaceAll(m[1], "/", " /")) {
				if n, ok := strings.CutPrefix(f, "/"); ok && n != "" {
					names = append(names, n)
				}
			}
			return names
		}
	}
	if n := dictName(dict, key); n != "" {
		return []string{n}
	}
	return nil
}

func dictInts(dict, key string) []int {
	for _, i := range keyIndexes(dict, key) {
		m := arrRe.FindStringSubmatch(dict[i:])
		if m == nil {
			continue
		}
		var out []int
		for _, f := range strings.Fields(m[1]) {
			n, err := strconv.Atoi(f)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}
	return nil
}

// dictSub is the nested dictionary under key ("" when the value is not an
// inline dictionary).
func dictSub(dict, key string) string {
	for _, i := range keyIndexes(dict, key) {
		if end := dictEnd([]byte(dict), i); end > 0 {
			return dict[i:end]
		}
	}
	return ""
}

// leadingInt parses the unsigned integer that starts s after whitespace.
func leadingInt(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\r\n\f\x00")
	n := 0
	for i := 0; i < len(s) && i < 18; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return n, i > 0
		}
		n = n*10 + int(c-'0')
	}
	return n, len(s) > 0
}

// dictEnd is the index just past the "<<…>>" that starts (after whitespace)
// at from, nesting, strings and comments respected; -1 when it does not
// close within b.
func dictEnd(b []byte, from int) int {
	i := skipWS(b, from)
	if !bytes.HasPrefix(b[i:], []byte("<<")) {
		return -1
	}
	depth := 0
	for ; i < len(b); i++ {
		switch b[i] {
		case '<':
			if i+1 < len(b) && b[i+1] == '<' {
				depth++
				i++
			} else { // a hex string
				k := bytes.IndexByte(b[i+1:], '>')
				if k < 0 {
					return -1
				}
				i += k + 1
			}
		case '>':
			if i+1 < len(b) && b[i+1] == '>' {
				depth--
				i++
				if depth == 0 {
					return i + 1
				}
			}
		case '(':
			k := literalEnd(b, i)
			if k < 0 {
				return -1
			}
			i = k - 1
		case '%':
			for i < len(b) && b[i] != '\n' && b[i] != '\r' {
				i++
			}
		}
	}
	return -1
}

// literalEnd is the index just past the ")" closing the literal string
// that opens at b[i]; -1 when unclosed.
func literalEnd(b []byte, i int) int {
	depth := 0
	for k := i; k < len(b); k++ {
		switch b[k] {
		case '\\':
			k++
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return k + 1
			}
		}
	}
	return -1
}

func skipWS(b []byte, i int) int {
	for i < len(b) && strings.IndexByte(" \t\r\n\f\x00", b[i]) >= 0 {
		i++
	}
	return i
}

// --- strings ---------------------------------------------------------------------

// stringBytes decodes the string object that starts b: a literal "(…)" with
// its escapes, or a hex "<…>". Anything else yields nothing.
func stringBytes(b []byte) []byte {
	b = b[skipWS(b, 0):]
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '(':
		end := literalEnd(b, 0)
		if end < 0 {
			end = len(b)
		}
		return unescapeLiteral(b[1:max(1, end-1)])
	case '<':
		if len(b) > 1 && b[1] == '<' {
			return nil
		}
		end := bytes.IndexByte(b, '>')
		if end < 0 {
			end = len(b)
		}
		return hexBytes(b[1:end])
	}
	return nil
}

func unescapeLiteral(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		switch c = b[i]; c {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case '\r': // a line continuation, \r\n or \r
			if i+1 < len(b) && b[i+1] == '\n' {
				i++
			}
		case '\n':
		default:
			if c >= '0' && c <= '7' { // up to three octal digits
				n := 0
				k := 0
				for ; k < 3 && i+k < len(b) && b[i+k] >= '0' && b[i+k] <= '7'; k++ {
					n = n*8 + int(b[i+k]-'0')
				}
				i += k - 1
				out = append(out, byte(n))
			} else {
				out = append(out, c) // \( \) \\ and anything the spec ignores the backslash of
			}
		}
	}
	return out
}

func hexBytes(b []byte) []byte {
	var digits []byte
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			digits = append(digits, c-'0')
		case c >= 'a' && c <= 'f':
			digits = append(digits, c-'a'+10)
		case c >= 'A' && c <= 'F':
			digits = append(digits, c-'A'+10)
		}
	}
	if len(digits)%2 == 1 {
		digits = append(digits, 0) // a trailing lone digit reads as if followed by 0
	}
	out := make([]byte, len(digits)/2)
	for i := range out {
		out[i] = digits[2*i]<<4 | digits[2*i+1]
	}
	return out
}

// decodeText turns a PDF text string's bytes into a Go string: UTF-16BE
// behind its byte-order mark, UTF-8 behind its (or when valid, as many
// writers emit it regardless), else PDFDocEncoding read as Latin-1 — the
// same for every character a title is likely to hold. Whitespace runs are
// collapsed and NULs dropped.
func decodeText(b []byte) string {
	var s string
	switch {
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
		s = string(utf16.Decode(u))
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		s = string(b[3:])
	case utf8.Valid(b):
		s = string(b)
	default:
		rs := make([]rune, len(b))
		for i, c := range b {
			rs[i] = rune(c)
		}
		s = string(rs)
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\x00", "")), " ")
}

// --- streams ---------------------------------------------------------------------

// decodeStream applies the stream's filter — none, or FlateDecode with an
// optional PNG predictor, which is everything cross-reference and object
// streams use in practice. Any other filter is an error: probing would
// rather scan than guess.
func decodeStream(dict string, data []byte) ([]byte, error) {
	filters := dictNames(dict, "Filter")
	if len(filters) == 0 {
		return data, nil
	}
	if len(filters) > 1 || (filters[0] != "FlateDecode" && filters[0] != "Fl") {
		return nil, fmt.Errorf("pdf: unsupported stream filter %v", filters)
	}
	out, err := inflate(data)
	if err != nil {
		return nil, err
	}
	parms := dictSub(dict, "DecodeParms")
	if pred, ok := dictInt(parms, "Predictor"); ok && pred >= 10 {
		columns, colors, bpc := 1, 1, 8
		if c, ok := dictInt(parms, "Columns"); ok && c > 0 {
			columns = c
		}
		if c, ok := dictInt(parms, "Colors"); ok && c > 0 {
			colors = c
		}
		if c, ok := dictInt(parms, "BitsPerComponent"); ok && c > 0 {
			bpc = c
		}
		out = unpredictPNG(out, columns, colors, bpc)
	}
	return out, nil
}

// inflate decompresses zlib-wrapped or raw deflate data, keeping whatever
// came out before a truncated stream gave up.
func inflate(data []byte) ([]byte, error) {
	data = bytes.TrimLeft(data, " \t\r\n")
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		out, _ := io.ReadAll(zr)
		if len(out) > 0 {
			return out, nil
		}
	}
	out, err := io.ReadAll(flate.NewReader(bytes.NewReader(data)))
	if len(out) > 0 {
		return out, nil
	}
	if err == nil {
		err = errors.New("pdf: empty stream")
	}
	return nil, err
}

// unpredictPNG undoes the PNG row filters (predictor 10–15): every row is a
// filter-type byte and the filtered bytes of one row of columns samples.
func unpredictPNG(data []byte, columns, colors, bpc int) []byte {
	bpp := max(1, (colors*bpc+7)/8)
	rowLen := (columns*colors*bpc + 7) / 8
	out := make([]byte, 0, len(data))
	prev := make([]byte, rowLen)
	for pos := 0; pos+1+rowLen <= len(data); pos += 1 + rowLen {
		ft := data[pos]
		row := slices.Clone(data[pos+1 : pos+1+rowLen])
		for i := range row {
			var a, c byte
			if i >= bpp {
				a, c = row[i-bpp], prev[i-bpp]
			}
			b := prev[i]
			switch ft {
			case 1:
				row[i] += a
			case 2:
				row[i] += b
			case 3:
				row[i] += byte((int(a) + int(b)) / 2)
			case 4:
				row[i] += paeth(a, b, c)
			}
		}
		out = append(out, row...)
		prev = row
	}
	return out
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	if pa <= pb && pa <= pc {
		return a
	}
	if pb <= pc {
		return b
	}
	return c
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func beUint(b []byte) uint64 {
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	return n
}

// readAt reads up to n bytes at off, returning what was there (a short read
// at the end of the file is not an error to probing).
func readAt(r io.ReaderAt, off, n int64) []byte {
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	got, _ := r.ReadAt(buf, off)
	return buf[:got]
}
