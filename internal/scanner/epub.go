package scanner

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"

	"flickr/internal/model"
)

// An EPUB is a zip with a fixed doorway: META-INF/container.xml names the
// package document (the OPF), and the OPF carries the metadata, the manifest
// (every file in the book) and the spine (the manifest items in reading
// order). Probing reads exactly that — no rendering, no full-text pass — so a
// text item gets the same shape of MediaInfo a film does: a container, a
// list of navigation points, and what the file says about itself.

// containerXML is META-INF/container.xml: rootfile paths, one per rendition
// (the first is the default).
type containerXML struct {
	Rootfiles []struct {
		FullPath  string `xml:"full-path,attr"`
		MediaType string `xml:"media-type,attr"`
	} `xml:"rootfiles>rootfile"`
}

// opfPackage is the package document. Only the parts probing needs are
// mapped; the namespace prefixes (dc:, opf:) are matched by local name.
type opfPackage struct {
	Metadata struct {
		Title    []string `xml:"title"`
		Creator  []string `xml:"creator"`
		Language []string `xml:"language"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
	Spine struct {
		TOC      string `xml:"toc,attr"` // EPUB 2: manifest id of the NCX
		ItemRefs []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

// ncxDoc is the EPUB 2 navigation file: a tree of navPoints, each labelling
// a content document. Nested points are flattened — the spine is flat too.
type ncxDoc struct {
	NavPoints []ncxNavPoint `xml:"navMap>navPoint"`
}

type ncxNavPoint struct {
	Label   string `xml:"navLabel>text"`
	Content struct {
		Src string `xml:"src,attr"`
	} `xml:"content"`
	Children []ncxNavPoint `xml:"navPoint"`
}

// ProbeEpub reads an EPUB's package metadata from a zip held in r (size
// bytes) and maps it to a text MediaInfo: Medium "text", Container "epub",
// no duration, Sections = spine length, one Chapter per spine item in reading
// order. Chapter titles come from the book's own table of contents — the
// EPUB 3 nav document or the EPUB 2 NCX — when the TOC labels that spine
// item; otherwise the spine idref stands in, so a section is never nameless.
// A malformed or missing TOC costs only titles, never the probe.
func ProbeEpub(r io.ReaderAt, size int64) (*model.MediaInfo, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("epub: not a zip: %w", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	var container containerXML
	if err := readXML(files, "META-INF/container.xml", &container); err != nil {
		return nil, fmt.Errorf("epub: %w", err)
	}
	if len(container.Rootfiles) == 0 || container.Rootfiles[0].FullPath == "" {
		return nil, fmt.Errorf("epub: container.xml names no rootfile")
	}
	opfPath := container.Rootfiles[0].FullPath
	var pkg opfPackage
	if err := readXML(files, opfPath, &pkg); err != nil {
		return nil, fmt.Errorf("epub: %w", err)
	}
	if len(pkg.Spine.ItemRefs) == 0 {
		return nil, fmt.Errorf("epub: %s has an empty spine", opfPath)
	}

	// Manifest hrefs are relative to the OPF's own directory; resolve them
	// once so spine items and TOC targets meet on the same zip paths.
	opfDir := path.Dir(opfPath)
	hrefByID := map[string]string{}
	var navPath, ncxPath string
	for _, it := range pkg.Manifest.Items {
		p := resolveHref(opfDir, it.Href)
		hrefByID[it.ID] = p
		if strings.Contains(" "+it.Properties+" ", " nav ") {
			navPath = p
		}
		if it.ID == pkg.Spine.TOC || it.MediaType == "application/x-dtbncx+xml" {
			ncxPath = p
		}
	}

	titles := map[string]string{} // resolved content path → TOC label
	if navPath != "" {
		readNavTitles(files, navPath, titles)
	}
	if len(titles) == 0 && ncxPath != "" {
		readNCXTitles(files, ncxPath, titles)
	}

	info := &model.MediaInfo{
		Medium:    model.MediumText,
		Container: "epub",
		Sections:  len(pkg.Spine.ItemRefs),
	}
	for _, ref := range pkg.Spine.ItemRefs {
		title := titles[hrefByID[ref.IDRef]]
		if title == "" {
			title = ref.IDRef
		}
		info.Chapters = append(info.Chapters, model.Chapter{Title: title})
	}
	doc := &model.Document{
		Title:    first(pkg.Metadata.Title),
		Creator:  first(pkg.Metadata.Creator),
		Language: first(pkg.Metadata.Language),
	}
	if *doc != (model.Document{}) {
		info.Document = doc
	}
	return info, nil
}

// readXML decodes one zip member into v. Missing member = error; the caller
// decides whether that is fatal.
func readXML(files map[string]*zip.File, name string, v any) error {
	f := files[name]
	if f == nil {
		return fmt.Errorf("%s missing", name)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer rc.Close()
	if err := xml.NewDecoder(rc).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// readNavTitles fills titles from an EPUB 3 nav document: the links inside
// the <nav epub:type="toc"> element (or, when no nav is typed, the first
// <nav>), keyed by their target document without its fragment. The first
// label for a document wins — a chapter with sub-headings is named by the
// chapter. XHTML is tokenized leniently (HTML entities, unclosed void
// elements) since real books are not always well-formed XML.
func readNavTitles(files map[string]*zip.File, navPath string, titles map[string]string) {
	f := files[navPath]
	if f == nil {
		return
	}
	rc, err := f.Open()
	if err != nil {
		return
	}
	defer rc.Close()
	dec := xml.NewDecoder(rc)
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.AutoClose = xml.HTMLAutoClose

	// One link list per <nav> element, in document order; chosen below.
	type navLinks struct {
		isTOC bool
		links map[string]string
	}
	var navs []*navLinks
	var cur *navLinks
	navDir := path.Dir(navPath)
	depth := 0      // element nesting inside cur, the <nav> counted
	var href string // link being read, "" outside an <a>
	var label strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if cur == nil {
				if t.Name.Local == "nav" {
					cur = &navLinks{isTOC: strings.Contains(attr(t, "type"), "toc"), links: map[string]string{}}
					navs = append(navs, cur)
					depth = 1 // the <nav> itself; its own close brings this to 0
				}
				continue
			}
			depth++
			if t.Name.Local == "a" {
				href = attr(t, "href")
				label.Reset()
			}
		case xml.CharData:
			if cur != nil && href != "" {
				label.Write(t)
			}
		case xml.EndElement:
			if cur == nil {
				continue
			}
			if t.Name.Local == "a" && href != "" {
				target, _, _ := strings.Cut(href, "#")
				key := resolveHref(navDir, target)
				if text := strings.Join(strings.Fields(label.String()), " "); text != "" {
					if _, dup := cur.links[key]; !dup {
						cur.links[key] = text
					}
				}
				href = ""
			}
			depth--
			if depth <= 0 {
				cur = nil // </nav>
			}
		}
	}
	var chosen *navLinks
	for _, n := range navs {
		if n.isTOC {
			chosen = n
			break
		}
	}
	if chosen == nil && len(navs) > 0 {
		chosen = navs[0]
	}
	if chosen == nil {
		return
	}
	for k, v := range chosen.links {
		titles[k] = v
	}
}

// readNCXTitles fills titles from an EPUB 2 NCX, flattening nested navPoints.
func readNCXTitles(files map[string]*zip.File, ncxPath string, titles map[string]string) {
	var ncx ncxDoc
	if err := readXML(files, ncxPath, &ncx); err != nil {
		return
	}
	ncxDir := path.Dir(ncxPath)
	var walk func(points []ncxNavPoint)
	walk = func(points []ncxNavPoint) {
		for _, p := range points {
			target, _, _ := strings.Cut(p.Content.Src, "#")
			key := resolveHref(ncxDir, target)
			if text := strings.Join(strings.Fields(p.Label), " "); text != "" {
				if _, dup := titles[key]; !dup {
					titles[key] = text
				}
			}
			walk(p.Children)
		}
	}
	walk(ncx.NavPoints)
}

// resolveHref joins a manifest/TOC href onto the directory of the document
// that carries it, yielding the zip member path. Percent-escapes are left
// alone: both sides of every lookup go through this same function.
func resolveHref(dir, href string) string {
	if dir == "." || dir == "" {
		return path.Clean(href)
	}
	return path.Clean(path.Join(dir, href))
}

// attr returns an attribute by local name, ignoring namespace prefixes
// (epub:type and plain type both answer to "type").
func attr(el xml.StartElement, name string) string {
	for _, a := range el.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func first(ss []string) string {
	for _, s := range ss {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}
