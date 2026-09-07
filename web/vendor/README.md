# Vendored browser libraries

The reader pane (`web/reader.js`) runs on epub.js, which needs JSZip to
open the book's zip. Both are vendored here so the LAN-only engine loads
nothing from the internet at read time (the service worker precaches them
with the rest of the shell). Pinned versions, taken from the npm registry
tarballs whose `dist.integrity` the registry publishes:

| File | Package | Version | Licence | Tarball integrity (npm) |
|---|---|---|---|---|
| `epub.min.js` | `epubjs` (`dist/epub.min.js`) | 0.3.93 | BSD-2-Clause — `LICENSE-epubjs.txt` | `sha512-c06pNSdBxcXv3dZSbXAVLE1/pmleRhOT6mXNZo6INKmvuKpYB65MwU/lO7830czCtjIiK9i+KR+3S+p0wtljrw==` |
| `jszip.min.js` | `jszip` (`dist/jszip.min.js`) | 3.10.1 | MIT or GPL-3.0-or-later — `LICENSE-jszip.markdown` | `sha512-xXDvecyTpGLrqFrvkrUSoxxfJI5AH7U8zxxtVclpsUtMCq4JQ290LY8AW5c7Ggnr/Y/oK+bQMbqK2qmtk3pN4g==` |

File checksums (sha256) as vendored:

```
06eae15745107b4aa508c95538275251f69bfb9f1175621fc458d9f42ed082d4  epub.min.js
acc7e41455a80765b5fd9c7ee1b8078a6d160bbbca455aeae854de65c947d59e  jszip.min.js
```

To upgrade: fetch the new tarball (`https://registry.npmjs.org/<pkg>/-/<pkg>-<ver>.tgz`),
check its integrity against `https://registry.npmjs.org/<pkg>/<ver>`,
copy `package/dist/<file>` here, update this table, and bump the service
worker cache version in `web/sw.js`. Load order in `index.html` is
JSZip first, then epub.js (the UMD build reads the `JSZip` global), then
`reader.js`.
