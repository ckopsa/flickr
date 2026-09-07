# Vendored browser libraries

The reader panes run on vendored libraries so the LAN-only engine loads
nothing from the internet at read time (the service worker precaches them
with the rest of the shell): epub.js, which needs JSZip to open the book's
zip, for the EPUB pane (`web/reader.js`); pdf.js for the PDF pane
(`web/pdfreader.js`). Pinned versions, taken from the npm registry
tarballs whose `dist.integrity` the registry publishes:

| File | Package | Version | Licence | Tarball integrity (npm) |
|---|---|---|---|---|
| `epub.min.js` | `epubjs` (`dist/epub.min.js`) | 0.3.93 | BSD-2-Clause — `LICENSE-epubjs.txt` | `sha512-c06pNSdBxcXv3dZSbXAVLE1/pmleRhOT6mXNZo6INKmvuKpYB65MwU/lO7830czCtjIiK9i+KR+3S+p0wtljrw==` |
| `jszip.min.js` | `jszip` (`dist/jszip.min.js`) | 3.10.1 | MIT or GPL-3.0-or-later — `LICENSE-jszip.markdown` | `sha512-xXDvecyTpGLrqFrvkrUSoxxfJI5AH7U8zxxtVclpsUtMCq4JQ290LY8AW5c7Ggnr/Y/oK+bQMbqK2qmtk3pN4g==` |
| `pdf.min.mjs` | `pdfjs-dist` (`legacy/build/pdf.min.mjs`) | 4.10.38 | Apache-2.0 — `LICENSE-pdfjs.txt` | `sha512-/Y3fcFrXEAsMjJXeL9J8+ZG9U01LbuWaYypvDW2ycW1jL269L3js3DVBjDJ0Up9Np1uqDXsDrRihHANhZOlwdQ==` |
| `pdf.worker.min.mjs` | `pdfjs-dist` (`legacy/build/pdf.worker.min.mjs`) | 4.10.38 | Apache-2.0 — `LICENSE-pdfjs.txt` | (same tarball) |

File checksums (sha256) as vendored:

```
06eae15745107b4aa508c95538275251f69bfb9f1175621fc458d9f42ed082d4  epub.min.js
acc7e41455a80765b5fd9c7ee1b8078a6d160bbbca455aeae854de65c947d59e  jszip.min.js
44ec6f011027ee77791386b66c14876a5fc29e20bf0433c07c6726fff7212b72  pdf.min.mjs
bd88805178a26c729db8c0107a5b630cb900ec070f4d8c7529a3e45530afd41d  pdf.worker.min.mjs
```

To upgrade: fetch the new tarball (`https://registry.npmjs.org/<pkg>/-/<pkg>-<ver>.tgz`),
check its integrity against `https://registry.npmjs.org/<pkg>/<ver>`,
copy the files here, update this table, and bump the service worker cache
version in `web/sw.js`. Load order in `index.html` is JSZip first, then
epub.js (the UMD build reads the `JSZip` global), then pdf.js, then
`pdfreader.js`, then `reader.js` (the facade over both panes).

pdf.js 4.x ships only ES modules (`.mjs`): `pdf.min.mjs` is loaded with
`<script type="module">` — no bundler, and it sets `window.pdfjsLib` when it
runs, which is after the page is parsed — and the worker is loaded by pdf.js
itself as a module worker from `GlobalWorkerOptions.workerSrc`, which
`pdfreader.js` points at `/vendor/pdf.worker.min.mjs`. The `legacy` build is
the one vendored (the same code transpiled for older browsers, ~45 KB
larger). Not vendored: pdf.js's `cmaps/` and `standard_fonts/` directories,
which it consults for CJK-encoded text and for fonts a PDF uses without
embedding; a page needing them still draws, with substituted glyphs and a
console warning.
