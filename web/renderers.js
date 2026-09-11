// flickr — the renderers: one per document kind, each a PURE FUNCTION of the
// envelope (docs/hypermedia.md §The client).
//
//   library   the banded rows, their headings and the genre chips
//   search    the same tiles, in the groups the search answered
//   lines     the dialogue a search found: the words, where, and when
//   work      the show's episode list, the album's or audiobook's pane, the book's
//   artist    one name's shelf of records
//   item      the detail pane
//   session   the player chrome: now playing, the marks, the end-of-passage
//             panel, up next
//   continue  the resume shelf
//
// Each takes the document and answers an HTML STRING the kernel mounts. A
// string rather than DOM for three reasons: node --test can assert on it with
// no DOM at all (web/render_test.mjs runs them over the very goldens the Go
// handlers write), a renderer that cannot reach into the page cannot keep
// state, and mounting is then the kernel's single job.
//
// THE TWO RULES, and web/render_test.mjs holds both:
//
//  1. A renderer NEVER composes a URL. Every address it emits is
//     `links[rel].href` or `actions[name].href`, copied out of the document —
//     there is not one '/api/' literal in this file, and the test greps for
//     it. What a renderer does compose is a HASH: the hash grammar is the
//     client's own (README "Deep links and passages"), and hashFor below is
//     the whole of it.
//  2. Every action the document offers becomes a control carrying the
//     document's own LABEL — except the ones a screen uses without drawing
//     (SILENT below). A missing action is a control that is not there; an
//     `unavailable` entry is the reason, shown as the control's title.
//
// Unknown fields are ignored: a document that grows a field renders as it did.
(function (root) {
  'use strict';

  // Actions a screen acts on without drawing a button: `progress` is the
  // address the player's heartbeat writes to, not something a person presses.
  const SILENT = new Set(['progress']);

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, c =>
      ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  function fmtTime(s) {
    s = Math.max(0, Math.floor(Number(s) || 0));
    const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
    return (h ? h + ':' + String(m).padStart(2, '0') : m) + ':' + String(sec).padStart(2, '0');
  }

  // How long a title runs, said the way a person says it: "1h 42m", "45m".
  // Minutes, because minutes is the unit the enrichment carries.
  function fmtRuntime(min) {
    const m = Math.floor(Number(min) || 0);
    if (m <= 0) return '';
    const h = Math.floor(m / 60), rest = m % 60;
    if (!h) return rest + 'm';
    return rest ? h + 'h ' + rest + 'm' : h + 'h';
  }

  // --- the document, read ------------------------------------------------------
  // One reach per relation, so a document that does not carry one reads as
  // absent rather than throwing halfway down a chain.
  function link(doc, rel) { return (doc && doc.links && doc.links[rel]) || null; }
  function href(doc, rel) { const l = link(doc, rel); return (l && l.href) || ''; }
  function action(doc, name) { return (doc && doc.actions && doc.actions[name]) || null; }
  function why(doc, name) {
    const u = doc && doc.unavailable && doc.unavailable[name];
    return (u && u.reason) || '';
  }
  // The picture a document shows itself by, in the order a screen wants it:
  // its own artwork relation, then whichever of the three kinds of picture
  // this thing has. Never a guess — every one of them is a link or nothing.
  function artwork(doc) {
    return href(doc, 'artwork') || href(doc, 'still') || href(doc, 'poster') || href(doc, 'cover');
  }

  // --- the hash grammar (the client's own; README "Deep links and passages") ---
  //
  // A hash is not an address: the addresses in this file are all copied out
  // of the document, and these four functions spell the ROUTE the browser
  // shows for one. idIn reads the identifier back out of an href the document
  // gave — it composes nothing, it only recognises the last segment of an
  // item's own address, which is the number `#/item/<id>` is spelled with.
  function idIn(href) { return String(href || '').split('/').pop(); }
  function itemHash(id, query) { return '#/item/' + id + (query || ''); }
  function showHash(title) { return '#/show/' + encodeURIComponent(title); }
  function artistHash(name) { return '#/artist/' + encodeURIComponent(name); }
  // What was searched for is IN the address, so a page of results is a place
  // a person can send somebody rather than a gesture they have to repeat.
  function searchHash(q) { return '#/search/' + encodeURIComponent(q); }
  // Where a tile goes when it is tapped: an artist's shelf, a show's episode
  // list, or the member the tile opens at (`item_id` — the film itself, track
  // one, the book).
  function hashFor(tile) {
    if (!tile) return '#/';
    if (tile.kind === 'artist') return artistHash(tile.title);
    if (tile.work_kind === 'show') return showHash(tile.title);
    return itemHash(tile.item_id);
  }

  // --- controls ----------------------------------------------------------------

  // control is the one place a button is made from an action. The kernel
  // listens for [data-act] and follows the href and method written here, so
  // no screen anywhere composes a request.
  function control(name, act, opts) {
    if (!act) return '';
    const o = opts || {};
    // `data` is what the button sends BACK: a value the action's own `input`
    // named as a literal rather than as a type, written on the button for the
    // kernel to make the body out of. Nothing here invents one.
    const data = o.data || {};
    return '<button class="act' + (o.cls ? ' ' + esc(o.cls) : '') + '"' +
      ' data-act="' + esc(name) + '"' +
      ' data-href="' + esc(act.href) + '"' +
      ' data-method="' + esc(act.method || 'POST') + '"' +
      Object.keys(data).map(k => ' data-' + esc(k) + '="' + esc(data[k]) + '"').join('') +
      (o.title ? ' title="' + esc(o.title) + '"' : '') +
      (o.hidden ? ' hidden' : '') +
      (o.id ? ' id="' + esc(o.id) + '"' : '') +
      '>' + esc(o.label || act.label || name) + '</button>';
  }

  // The tick, and the way back off it — the item's `watched` action, drawn on
  // its own page and on every row in a list.
  //
  // Which WAY the press goes is the server's: the label says it in words
  // ("Mark watched", "Mark unwatched") and the action's `input` carries the
  // value to send back, so all that is decided here is the mark drawn on the
  // button — a ✓ to tick a thing off, a ↺ to undo one. `wide` is the page's
  // shape, which has room for the words beside the mark; a row has not.
  function watchedControl(doc, cls, wide) {
    const act = action(doc, 'watched');
    if (!act) return '';
    const want = String((act.input && act.input.watched) || 'true') !== 'false';
    const mark = want ? '✓' : '↺';
    return control('watched', act, {
      cls: cls, title: act.label,
      data: { watched: want ? 'true' : 'false' },
      label: wide ? mark + ' ' + (act.label || '') : mark,
    });
  }

  // The bookmark: My List, pressed from wherever the thing is seen. Which
  // way the press goes is the server's — a work carries `save` or `unsave`,
  // never both — so all that is decided here is the mark on the button: a +
  // to put a title by, a ✓ once it is on the list. `wide` is a page's shape,
  // which has room for the server's own words beside the mark; a tile has not.
  function bookmarkControl(doc, cls, wide) {
    const put = action(doc, 'save');
    const act = put || action(doc, 'unsave');
    if (!act) return '';
    const mark = put ? '+' : '✓';
    return control(put ? 'save' : 'unsave', act, {
      cls: cls, title: act.label,
      label: wide ? mark + ' ' + (act.label || '') : mark,
    });
  }

  // The controls a document offers that nothing above has drawn already.
  // `drawn` is the set of action names the renderer handled itself.
  function restControls(doc, drawn) {
    const acts = (doc && doc.actions) || {};
    const skip = new Set(drawn || []);
    return Object.keys(acts).sort()
      .filter(n => !SILENT.has(n) && !skip.has(n))
      .map(n => control(n, acts[n])).join('');
  }

  // A navigation control: a hash, never an address.
  function nav(hash, label, cls, extra) {
    return '<a class="' + esc(cls || '') + '" href="' + esc(hash) + '"' +
      (extra || '') + '>' + esc(label) + '</a>';
  }

  function backTo(hash, label) {
    return '<button class="back" data-nav="' + esc(hash) + '">← ' + esc(label) + '</button>';
  }

  // --- tiles -------------------------------------------------------------------

  // One tile. The picture is the document's `links.artwork`; a tile with none
  // is a styled text tile, decided here rather than after a 404.
  //
  // A tile says a title and ONE line under it — the document's own `subtitle`,
  // which is words about the thing. What the FILE is (`tech`) is not a reason
  // to pick a tile, so it lives on the item's page now; the text tile falls
  // back to it only when the document gave it nothing else to show.
  function card(t, cls) {
    const art = artwork(t);
    const title = t.title || '';
    // The corner of the poster: how many episodes of this show have just
    // arrived. The count is the document's own (`new_episodes`), so nothing
    // here decides what "new" is — a tile without one wears no badge.
    const badge = t.new_episodes > 0
      ? '<span class="new-badge">' + esc(t.new_episodes + ' new') + '</span>' : '';
    // The other corner: the tile's own save or unsave, drawn as a control
    // INSIDE the control the tile is — the kernel reads the innermost
    // [data-act], so pressing it puts the title by rather than opening it. It
    // is quiet until the tile is under the pointer or the ring (index.html).
    const mark = bookmarkControl(t, 'tile-save');
    const wrap = art
      ? '<div class="poster-wrap">' + mark +
        '<img loading="lazy" alt="" src="' + esc(art) + '">' + badge + '</div>'
      : '<div class="poster-wrap text-tile">' + mark + badge + '<div class="tile-title">' + esc(title) +
        '</div><div class="tile-sub">' + esc(t.subtitle || t.tech || '') + '</div></div>';
    // A tile is a div, so the keyboard would walk straight past it: tabindex
    // puts it in the tab order and the role says what activating it does.
    // The scrub sheets ride along where the document offered them: a pointer
    // resting on the tile plays them, which is the kernel's affair, and the
    // address is the document's own — copied, never composed.
    const trick = href(t, 'trickplay');
    return '<div class="' + esc(cls || 'card') + '" tabindex="0" role="link"' +
      ' data-nav="' + esc(hashFor(t)) + '"' +
      (trick ? ' data-trickplay="' + esc(trick) + '"' : '') + '>' +
      wrap +
      '<div class="c-title">' + esc(title) + '</div>' +
      (t.subtitle ? '<div class="c-sub">' + esc(t.subtitle) + '</div>' : '') +
      '</div>';
  }

  // --- library -----------------------------------------------------------------

  // A tile matches the search box and the genre chip. Both only FILTER what
  // the document already listed, in the order it listed it: no grouping, no
  // sorting and no counting happens on this side any more.
  function matches(t, state) {
    const s = state || {};
    if (s.genre && !(t.genres || []).includes(s.genre)) return false;
    if (s.q && !String(t.search || '').includes(s.q)) return false;
    return true;
  }

  // One headed row of tiles. A heading with nothing under it is never drawn,
  // so the caller filters first and only builds a section it has tiles for.
  function bandSection(heading, tiles, cls, attr) {
    return '<section class="band"' + (attr || '') + '>' +
      (heading ? '<h3>' + esc(heading) + '</h3>' : '') +
      '<div class="' + cls + '">' + tiles.map(t => card(t)).join('') + '</div>' +
      '</section>';
  }

  // What the box is filtering the shelf by, said over the shelf it is
  // filtering. The box keeps its words on the way back from a results page,
  // and a home with half its titles missing has to say why — and carry the
  // one way out of it.
  function filterChip(state) {
    const q = (state && state.q) || '';
    if (!q) return '';
    return '<div id="lib-filter">' +
      '<button data-clear-q="1">Filtering by “' + esc(q) + '” <span aria-hidden="true">×</span></button>' +
      '</div>';
  }

  // The library is ROWS, not one wall: what is new, then a section per band
  // the document names — `bands` is an ordered list of {key, title} and every
  // tile says which band it sits in, so nothing is grouped or sorted here.
  // The search box and the genre chip filter INSIDE the sections, and a
  // section their filter empties is not drawn at all.
  function libraryGrid(doc, state) {
    const chip = filterChip(state);
    const tiles = (doc && doc.items) || [];
    if (!tiles.length) return { html: chip + '<div id="empty">No items yet — run a scan.</div>', count: 0 };
    const kept = tiles.filter(t => matches(t, state));
    if (!kept.length) return { html: chip + '<div id="empty">Nothing matches your search.</div>', count: 0 };

    let html = chip;
    // What this profile put by for later leads the rows — it is the one shelf
    // they wrote themselves, and it sits under the resume shelf the kernel
    // paints above the grid. The library document carries it, so the home
    // draws it without a second fetch.
    const mine = ((doc && doc.list) || []).filter(t => matches(t, state));
    if (mine.length) {
      html += bandSection('My List', mine, 'band-row', ' id="list-row"');
    }
    // Then what arrived lately: the document's own selection of the same
    // tiles, drawn as a row rather than a grid.
    const recent = ((doc && doc.recently_added) || []).filter(t => matches(t, state));
    if (recent.length) {
      html += bandSection('Recently added', recent, 'band-row', ' id="recent-row"');
    }
    // Then the shows something landed in this fortnight — the server's own
    // selection again, each tile already carrying the count its badge draws.
    const fresh = ((doc && doc.new_episodes) || []).filter(t => matches(t, state));
    if (fresh.length) {
      html += bandSection('New episodes', fresh, 'band-row', ' id="new-row"');
    }
    // Then what this profile might like, under the server's own heading —
    // "Because you watched Frozen" is a sentence about a title, and the
    // document writes it: nothing here scores or names anything.
    const because = (doc && doc.because) || null;
    const picks = ((because && because.items) || []).filter(t => matches(t, state));
    if (picks.length) {
      html += bandSection(because.title, picks, 'band-row', ' id="because-row"');
    }
    const named = {};
    for (const b of (doc.bands || [])) {
      named[b.key] = true;
      const inBand = kept.filter(t => t.band === b.key);
      if (inBand.length) {
        html += bandSection(b.title, inBand, 'band-grid', ' data-band="' + esc(b.key) + '"');
      }
    }
    // A tile whose band the document did not name is still drawn, under no
    // heading, at the end: no tile is ever invisible because a name moved.
    const rest = kept.filter(t => !named[t.band]);
    if (rest.length) html += bandSection('', rest, 'band-grid', ' data-band=""');
    return { html: html, count: kept.length };
  }

  function genreRow(doc, state) {
    const facet = (doc && doc.facets && doc.facets.genre) || [];
    if (!facet.length) return '<div id="genre-row" hidden></div>';
    const active = (state && state.genre) || null;
    const chips = [null].concat(facet.map(f => f.value)).map(g =>
      '<button data-genre="' + esc(g == null ? '' : g) + '"' +
      (g === active ? ' class="active"' : '') + '>' + esc(g == null ? 'All' : g) + '</button>').join('');
    return '<div id="genre-row">' + chips + '</div>';
  }

  // --- the hero ----------------------------------------------------------------

  // What the home leads with: the one thing this profile would put on now.
  // The resume shelf's first entry when there is one — it already knows the
  // verb and the place — else what arrived lately, else the first tile. All
  // three are the server's own orderings; nothing here sorts or scores.
  function heroPick(doc, cont) {
    return ((cont && cont.items) || [])[0] ||
      ((doc && doc.recently_added) || [])[0] ||
      ((doc && doc.items) || [])[0] || null;
  }

  // The banner: the entry's backdrop (its poster or cover when the document
  // gave it no backdrop), the title, one line about it, and — when the entry
  // carries one — its own resume/play action. The whole banner is the
  // control, the way a resume row is: the action rides it and the hash beside
  // it says where it is taken, so the place to pick up from is read on the
  // item's own page. An entry with no action is a plain link to it.
  function hero(doc, cont) {
    const e = heroPick(doc, cont);
    if (!e) return '';
    const art = href(e, 'backdrop') || artwork(e);
    const title = e.work_title || e.title || '';
    const line = e.overview || e.subtitle || (e.label && e.label !== title ? e.label : '');
    const act = action(e, 'resume') || action(e, 'play') || action(e, 'read');
    const name = action(e, 'resume') ? 'resume' : action(e, 'play') ? 'play' : 'read';
    // A resume entry is an item document and says so; a tile says which route
    // its kind spells.
    const hash = e.kind === 'item' ? itemHash(e.id) : hashFor(e);
    return '<section id="hero" tabindex="0" role="' + (act ? 'button' : 'link') + '"' +
      (act ? ' data-act="' + esc(name) + '" data-href="' + esc(act.href) + '"' +
             ' data-method="' + esc(act.method || 'POST') + '"' : '') +
      ' data-nav="' + esc(hash) + '">' +
      (art ? '<div class="hero-art"><img loading="lazy" alt="" src="' + esc(art) + '"></div>' : '') +
      '<div class="hero-body">' +
        '<h2 class="hero-title">' + esc(title) + '</h2>' +
        (line ? '<p class="hero-line">' + esc(line) + '</p>' : '') +
        (act ? '<span class="hero-act">' + esc(act.label) + '</span>' : '') +
      '</div></section>';
  }

  // The home screen, top to bottom: the hero, the chips, the resume shelf the
  // kernel fills in (#cw), then the banded sections. There is no tile count
  // any more — "6 titles" told nobody anything a headed row does not.
  //
  // `cont` is the resume shelf's document, which arrives on its own errand:
  // the kernel repaints #hero-slot when it lands, so the hero leads with what
  // is in progress the moment there is one.
  function library(doc, state, cont) {
    const s = state || {};
    return '<div id="lib-note"' + (s.note ? '' : ' hidden') + '>' + esc(s.note || '') + '</div>' +
      '<div id="hero-slot">' + hero(doc, cont) + '</div>' +
      genreRow(doc, s) +
      '<div id="cw" hidden></div>' +
      '<div id="grid">' + libraryGrid(doc, s).html + '</div>';
  }

  // --- search ------------------------------------------------------------------

  // The results: a headed row per group the document names, in the document's
  // own order, each drawn with the very tile the library draws — a hit is a
  // work, an artist's shelf or a member, and all three already say where they
  // go, so the artists row needs nothing of its own. Nothing is grouped,
  // counted or sorted here, and a group the server left out is a heading that
  // is never drawn.
  function search(doc) {
    if (!doc) return '';
    const q = doc.query || '';
    const head = backTo('#/', 'Library') +
      '<h2 id="search-title">' + esc(q ? 'Results for \u201c' + q + '\u201d' : 'Search') + '</h2>';
    const groups = (doc.groups || []).filter(g => (g.items || []).length);
    if (!groups.length) {
      return head + '<div id="empty">Nothing in the library matches.</div>';
    }
    return head + '<div id="search-results">' +
      groups.map(g => g.key === 'lines'
        // A line of dialogue is not a tile: it is words, and where they are
        // said. Same heading, same order, its own row.
        ? '<section class="band" data-group="' + esc(g.key) + '">' +
            '<h3>' + esc(g.title) + '</h3>' + lines(g.items) + '</section>'
        : bandSection(g.title, g.items, 'band-grid',
            ' data-group="' + esc(g.key) + '"')).join('') +
      '</div>';
  }

  // --- dialogue ----------------------------------------------------------------

  // The lines a search found, drawn the same way wherever they are found: on
  // the results page under their own heading, and under the finder on an
  // item's page. Each row is the words, what says them, and when \u2014 and a tap
  // is a PASSAGE, so the scene plays and stops rather than the film running
  // on from a seek.
  //
  // The passage is the server's (`passage`: the seconds either side of the
  // line it chose); what is composed here is the hash that spells it, which
  // is the client's own grammar.
  function cueHash(en) {
    const p = (en && en.passage) || {};
    const q = [];
    if (p.t != null) q.push('t=' + p.t);
    if (p.end != null) q.push('end=' + p.end);
    return itemHash(en && en.item_id, q.length ? '?' + q.join('&') : '');
  }

  function lineRow(en) {
    const where = en.subtitle || en.label || en.title || '';
    return '<button class="line" data-nav="' + esc(cueHash(en)) + '">' +
      '<span class="line-text">' + esc(en.text || '') + '</span>' +
      '<span class="line-where">' + esc(where) + '</span>' +
      '<span class="line-time">' + fmtTime(en.start) + '</span>' +
      '</button>';
  }

  // The rows, from a list of hits or from the `lines` document the finder's
  // own address answers \u2014 a search that found nothing says so, because an
  // empty box under a question reads as a page that is still thinking.
  function lines(hits) {
    const rows = Array.isArray(hits) ? hits : ((hits && hits.items) || []);
    if (!rows.length) {
      const asked = !Array.isArray(hits) && hits && hits.query;
      return asked ? '<div class="lines-empty">Nothing is said like that.</div>' : '';
    }
    return '<div class="lines">' + rows.map(en => lineRow(en)).join('') + '</div>';
  }

  // The finder itself: a box over the file's own words. It is drawn only
  // where the document offers `lines` \u2014 a file nobody has transcribed has
  // nothing to search \u2014 and the address it asks is that relation's, carried
  // on the form for the kernel to follow.
  function linesBox(doc) {
    const l = link(doc, 'lines');
    if (!l) return '';
    const label = l.title || 'Find in dialogue';
    return '<div id="detail-lines">' +
      '<form id="lines-form" data-href="' + esc(l.href) + '">' +
        '<label for="lines-q">' + esc(label) + '</label>' +
        '<input id="lines-q" name="q" type="search" autocomplete="off"' +
          ' placeholder="a line you remember">' +
        '<button type="submit" class="act">Find</button>' +
      '</form>' +
      '<div id="lines-hits"></div>' +
      '</div>';
  }

  // --- continue ----------------------------------------------------------------

  // The resume shelf. Every row is an item envelope the server filled in: the
  // work's title over the member's label, a bar at `percent`, a picture that
  // is a LINK (the browser used to try /poster and fall back to /cover on the
  // 404), and one action — `resume`, whatever verb the medium uses.
  function continueShelf(doc) {
    const rows = (doc && doc.items) || [];
    if (!rows.length) return '';
    const cards = rows.map(en => {
      const art = artwork(en);
      const title = en.work_title || en.title || '';
      const sub = en.label && en.label !== title ? en.label : '';
      const pct = Math.max(0, Math.min(100, Number(en.percent) || 0));
      const wrap = art
        ? '<div class="poster-wrap"><img loading="lazy" alt="" src="' + esc(art) + '"></div>'
        : '<div class="poster-wrap text-tile"><div class="tile-title">' + esc(title) + '</div>' +
          (sub ? '<div class="tile-sub">' + esc(sub) + '</div>' : '') + '</div>';
      // The row is its own control: the action's href and method ride on it,
      // and the hash beside them says WHERE it is taken — at the item's own
      // page, whose document carries the place to pick up from.
      const act = action(en, 'resume');
      const drop = action(en, 'forget');
      return '<div class="cw-card" tabindex="0" role="button"' +
        (act ? ' data-act="resume" data-href="' + esc(act.href) + '"' +
               ' data-method="' + esc(act.method || 'POST') + '"' : '') +
        ' data-nav="' + esc(itemHash(en.id)) + '" data-autoplay="1">' +
        // The × in the corner: the row's own `forget` action, drawn as a
        // control INSIDE the control the row is — the kernel reads the
        // innermost [data-act], so it drops the row rather than resuming it.
        control('forget', drop, { cls: 'cw-forget', label: '×', title: drop && drop.label }) +
        wrap +
        '<div class="cw-bar"><div style="width:' + pct.toFixed(1) + '%"></div></div>' +
        '<div class="c-title">' + esc(title) + '</div>' +
        (sub ? '<div class="c-sub">' + esc(sub) + '</div>' : '') +
        (act ? '<div class="cw-act">' + esc(act.label) + '</div>' : '') +
        '</div>';
    }).join('');
    return '<h3>' + esc(doc.title || 'Continue watching') + '</h3>' +
      '<div id="cw-row">' + cards + '</div>';
  }

  // --- work --------------------------------------------------------------------

  // How far into one member the profile stands, as a percentage of its own
  // clock: the two numbers the document already carries, divided. Zero when
  // it carried neither, and then no bar is drawn.
  function rowPercent(m) {
    const at = (m && m.resume && Number(m.resume.position_seconds)) || 0;
    const dur = Number(m && m.duration_seconds) || 0;
    if (!(at > 0 && dur > 0)) return 0;
    return Math.max(0, Math.min(100, at / dur * 100));
  }

  // One member as a row in a list: its label, its grey line, and — for an
  // episode TMDB knows — its still and its synopsis.
  function memberRow(m, opts) {
    const o = opts || {};
    const still = href(m, 'still');
    // `label` is the server's own name for this member in its work — "S03E22 ·
    // Beach Games", "Track 7 · Karma Police", "Part 2" — so nothing is
    // appended to it here.
    // Under the label goes how long the thing runs, when the document says so
    // — not what the file is. A row is chosen by what it IS.
    // Two marks of a place, both the document's own: a tick when the server
    // says `watched` (it is not read off `resume` — a watched thing has none
    // left), and a thin bar at how far in `resume` stands.
    const pct = rowPercent(m);
    const meta = '<div class="title">' +
      (m.watched ? '<span class="tick" title="Watched">✓</span> ' : '') +
      esc(m.label || m.title || '') + '</div>' +
      (m.duration_seconds ? '<div class="meta">' + esc(fmtTime(m.duration_seconds)) + '</div>' : '') +
      (pct ? '<div class="row-bar"><div style="width:' + pct.toFixed(1) + '%"></div></div>' : '') +
      (m.overview ? '<div class="overview">' + esc(m.overview) + '</div>' : '');
    const cls = 'item' + (still ? ' enriched' : '') + (o.current ? ' current' : '') +
      (m.watched ? ' watched' : '');
    // The tick sits at the end of the row, where a person ticks an episode
    // off. It is a control inside a control: the kernel reads the innermost
    // [data-act], so pressing it marks the row rather than opening it.
    return '<div class="' + cls + '" tabindex="0" role="link"' +
      ' data-nav="' + esc(itemHash(m.id)) + '">' +
      (still ? '<img class="still" loading="lazy" alt="" src="' + esc(still) + '">' : '') +
      '<div>' + meta + '</div>' +
      watchedControl(m, 'watch') +
      '</div>';
  }

  function members(doc) { return (doc && doc.members) || []; }
  function walked(doc) { return members(doc).filter(m => !m.extra); }
  function extras(doc) { return members(doc).filter(m => m.extra); }

  // The episodes in seasons, in the document's own order. The member says
  // which season it is in (`season`, off its identity), so the browser reads
  // no episode number out of a label; a member without one falls in a single
  // unnumbered group, which is what a show whose files never said looks like.
  function seasons(list) {
    const out = [], by = {};
    for (const m of list) {
      const n = Number(m.season) > 0 ? Number(m.season) : 0;
      if (!by[n]) { by[n] = { season: n, items: [] }; out.push(by[n]); }
      by[n].items.push(m);
    }
    return out;
  }

  // A season as a disclosure. More than one and they all take the same
  // `name`, which is the browser's OWN exclusive accordion: opening a season
  // closes the last, so seasons read as tabs with the client keeping no
  // state at all. The season holding Next up is the one that starts open.
  function seasonBlocks(list, openID) {
    const groups = seasons(list);
    const many = groups.length > 1;
    // Which one starts open: the season Next up is in, or — when nothing
    // names one — the first. With a single season there is nothing to close.
    let lead = groups.findIndex(g => g.items.some(m => String(m.id) === String(openID)));
    if (lead < 0) lead = 0;
    return groups.map((g, i) => {
      const label = g.season > 0 ? 'Season ' + g.season : 'Episodes';
      return '<details class="season"' + (many ? ' name="season"' : '') +
        (!many || i === lead ? ' open' : '') + '>' +
        '<summary>' + esc(label) +
          '<span class="season-count">' + esc(g.items.length) + '</span></summary>' +
        '<div class="season-items">' +
          g.items.map(m => memberRow(m, { current: String(m.id) === String(openID) })).join('') +
        '</div></details>';
    }).join('');
  }

  // The member `progress.next` names, matched in the document's own members
  // by the address the document gave for it. Nothing is composed: an href is
  // compared with an href.
  function memberAt(doc, addr) {
    return members(doc).filter(m => m.self === addr)[0] || null;
  }

  // What a show leads with: the one episode this profile would put on now —
  // its still, the server's name for it, and the work's OWN play action, in
  // the words the server chose ("▶ Resume"). A show nobody has started
  // carries no `progress`, and then there is no card and the action stays
  // where it was.
  function nextUp(doc) {
    const next = doc && doc.progress && doc.progress.next;
    if (!next || !next.href) return '';
    const m = memberAt(doc, next.href);
    const art = (m && artwork(m)) || artwork(doc);
    const hash = m ? itemHash(m.id) : '';
    return '<div id="next-up">' +
      (art ? '<div class="nu-art"' + (hash ? ' data-nav="' + esc(hash) + '"' : '') + '>' +
        '<img loading="lazy" alt="" src="' + esc(art) + '"></div>' : '') +
      '<div class="nu-body">' +
        '<div class="nu-lead">Next up</div>' +
        '<div class="nu-title">' + esc(next.title || (m && m.label) || '') + '</div>' +
        (m && m.overview ? '<div class="nu-overview">' + esc(m.overview) + '</div>' : '') +
        '<div class="nu-act">' + primary(doc) + '</div>' +
      '</div></div>';
  }

  // The show pane: where this profile stands, the episode to put on now, the
  // seasons, then the bonus material under its own heading — all in the
  // document's order, and episodes told from extras by the member's own
  // `extra` flag rather than by the browser reading an identity.
  function showPane(doc) {
    const eps = walked(doc), bonus = extras(doc);
    const drawn = ['play', 'read', 'passage', 'save', 'unsave'];
    const lead = nextUp(doc);
    const nextID = lead ? idIn((doc.progress.next || {}).href) : '';
    return backTo('#/', 'Library') +
      '<h2 id="show-title">' + esc(doc.title || '') + '</h2>' +
      // Where they stand, in the server's own words, under the title. It is
      // a fact about the show, not a warning, so it reads as one.
      (doc.progress && doc.progress.text
        ? '<div id="show-progress">' + esc(doc.progress.text) + '</div>' : '') +
      lead +
      // The primary rides the Next up card when there is one, so the work's
      // play action is drawn once either way. The bookmark stays here beside
      // it: putting a show by is a thing done to the show, not to an episode.
      '<div id="work-actions">' + (lead ? '' : primary(doc)) +
        bookmarkControl(doc, 'watch-wide', true) + restControls(doc, drawn) +
        passagePicker(doc) + '</div>' +
      '<div id="episode-list"' + (eps.length ? '' : ' hidden') + '>' +
        seasonBlocks(eps, nextID) + '</div>' +
      '<div id="extras-head"' + (bonus.length ? '' : ' hidden') + '>Extras</div>' +
      '<div id="extras-list"' + (bonus.length ? '' : ' hidden') + '>' +
        bonus.map(m => memberRow(m)).join('') + '</div>';
  }

  // Minting a passage from a work: the two ends are picked from `places` —
  // the tokens the grammar reads, each with the label a chip shows in its
  // stead — and the action's own href takes them. The client spells neither
  // the tokens nor the address; it hands both back exactly as they came.
  function passagePicker(doc) {
    const act = action(doc, 'passage');
    if (!act) return '';
    const places = doc.places || [];
    const options = end => '<select id="passage-' + end + '" data-input="' + end + '">' +
      '<option value="">' + (end === 'from' ? 'From the start' : 'To the end') + '</option>' +
      places.map(p => '<option value="' + esc(p.token) + '">' + esc(p.label) + '</option>').join('') +
      '</select>';
    return '<div id="passage-picker">' +
      (places.length ? options('from') + options('to') : '') +
      control('passage', act) +
      '<div id="passage-link" hidden><span id="passage-link-note">Passage link</span>' +
        '<code id="passage-link-url"></code></div>' +
      '</div>';
  }

  // The one action a work leads with, in the words the server chose: "▶ Play",
  // "▶ Resume", "Read". Absent when the work affords neither, and then the
  // reason the document gives is shown in its place.
  function primary(doc) {
    const act = action(doc, 'play') || action(doc, 'read');
    if (act) {
      const name = action(doc, 'play') ? 'play' : 'read';
      return control(name, act, { cls: 'primary', id: 'detail-play' });
    }
    const reason = why(doc, 'play') || why(doc, 'read');
    return reason ? '<div class="unavailable">' + esc(reason) + '</div>' : '';
  }

  // A record's pane (an album, an audiobook) and a book's: the cover, who
  // made it, the one action, and the parts or tracks in the work's order.
  function recordPane(doc) {
    const art = artwork(doc);
    const list = walked(doc);
    const many = list.length > 1;
    const drawn = ['play', 'read', 'passage', 'save', 'unsave'];
    return backTo('#/', 'Library') +
      '<div id="audio-pane"><div id="audio-head">' +
        (art ? '<img id="audio-cover" alt="" src="' + esc(art) + '">' : '') +
        '<div><div id="audio-title">' + esc(doc.title || '') +
          (doc.year ? ' (' + esc(doc.year) + ')' : '') + '</div>' +
          '<div id="audio-author">' + esc(doc.author || '') + '</div>' +
          '<div id="audio-part">' + esc((doc.progress && doc.progress.text) || '') + '</div>' +
          '<div id="work-actions">' + primary(doc) +
            bookmarkControl(doc, 'watch-wide', true) + restControls(doc, drawn) +
            passagePicker(doc) + '</div>' +
        '</div></div>' +
        '<div id="audio-list-head"' + (many ? '' : ' hidden') + '>' +
          esc(many ? list.length + ' ' + (doc.work_kind === 'album' ? 'tracks' : 'parts') : '') + '</div>' +
        '<div id="audio-list"' + (many ? '' : ' hidden') + '>' +
          list.map(m => memberRow(m)).join('') + '</div>' +
      '</div>';
  }

  function work(doc, state) {
    if (!doc) return '';
    return doc.work_kind === 'show' ? showPane(doc) : recordPane(doc, state);
  }

  // --- artist ------------------------------------------------------------------

  function artist(doc) {
    if (!doc) return '';
    const art = artwork(doc);
    const n = doc.album_count || 0, tracks = doc.track_count || 0;
    return backTo('#/', 'Library') +
      '<div id="artist-head">' +
        (art ? '<img id="artist-cover" alt="" src="' + esc(art) + '">' : '') +
        '<div><h2 id="artist-name">' + esc(doc.name || doc.title || '') + '</h2>' +
        '<div id="artist-sub">' + esc(n + ' album' + (n === 1 ? '' : 's') +
          ' · ' + tracks + ' track' + (tracks === 1 ? '' : 's')) + '</div></div>' +
      '</div>' +
      '<div id="artist-albums">' + ((doc.albums || []).map(a => card(a)).join('')) + '</div>';
  }

  // --- more like this ----------------------------------------------------------

  // The work document's own `similar` tiles, drawn with the library's card at
  // the foot of a member's page. The row is the server's — which titles are
  // alike, and how many of them — so this is a heading and a filter of one.
  function similarRow(wk) {
    const tiles = (wk && wk.similar) || [];
    if (!tiles.length) return '';
    return bandSection('More like this', tiles, 'band-row', ' id="similar-row"');
  }

  // --- item --------------------------------------------------------------------

  // The detail pane. `ctx.work` is the work document the kernel fetched by
  // following this item's own `links.work`: what a member needs to say about
  // the thing it belongs to — where Back goes, the record's other tracks, a
  // film's bonus material — without the item document restating it.
  function item(doc, ctx) {
    if (!doc) return '';
    const c = ctx || {};
    const wk = c.work || null;
    const art = artwork(doc) || (wk ? artwork(wk) : '');
    const back = href(doc, 'backdrop');
    // Play stands alone up here, with the tick beside it. The curatorial
    // actions are drawn too — but inside the Details disclosure, so `drawn`
    // names all of them as handled.
    const drawn = ['play', 'read', 'watched'].concat(ADMIN);
    const prev = link(doc, 'prev'), next = link(doc, 'next');
    const bonus = wk && wk.work_kind !== 'show' ? extras(wk) : [];
    const siblings = wk && wk.work_kind !== 'show' ? walked(wk) : [];

    return backTo(backHash(doc, wk), backLabel(doc, wk)) +
      '<div id="detail-info">' +
        // The picture the title is known by, behind the head and faded into
        // the page. It is a link like every other; a document without one is
        // a head on the flat page, which is what it always was.
        (back ? '<div id="detail-backdrop"><img loading="lazy" alt="" src="' + esc(back) + '"></div>' : '') +
        (art ? '<img id="detail-poster" alt="" src="' + esc(art) + '">' : '') +
        '<div id="detail-body">' +
          '<h2><span id="detail-title">' + esc(doc.title || '') + '</span> ' +
            '<span id="detail-year">' + esc(detailSub(doc, wk)) + '</span></h2>' +
          detailMeta(doc, wk) +
          '<p id="detail-overview"' + (doc.overview ? '' : ' hidden') + '>' + esc(doc.overview || '') + '</p>' +
          castLine(doc) +
          primary(doc) + watchedControl(doc, 'watch-wide', true) + restControls(doc, drawn) +
          chapterList(doc, c) +
          '<div id="detail-epnav"' + (prev || next ? '' : ' hidden') + '>' +
            (prev ? nav(itemHash(idIn(prev.href)), '⏮ Prev: ' + (prev.title || ''), '', '') : '') +
            (next ? nav(itemHash(idIn(next.href)), 'Next: ' + (next.title || '') + ' ⏭', '', '') : '') +
          '</div>' +
          '<div id="detail-extras"' + (bonus.length ? '' : ' hidden') + '>' +
            (bonus.length ? '<span class="label">Extras</span>' : '') +
            bonus.map(m => nav(itemHash(m.id), m.label || m.title || '', '')).join('') +
          '</div>' +
        '</div>' +
      '</div>' +
      (siblings.length > 1
        ? '<div id="audio-pane"><div id="audio-list-head">' +
            esc(siblings.length + ' ' + (wk.work_kind === 'album' ? 'tracks' : 'parts')) + '</div>' +
          '<div id="audio-list">' +
            siblings.map(m => memberRow(m, { current: m.id === doc.id })).join('') +
          '</div></div>'
        : '') +
      linesBox(doc) +
      details(doc) +
      similarRow(wk);
  }

  // The chapters, as a list one can look at rather than a row of times: the
  // name, when it starts, and a frame of the film cut out of the trickplay
  // sheets — the same pictures the scrub bar previews with. A tap plays from
  // there, which is the seek it always was.
  //
  // The sheets come from `ctx.trickplay`, the item's own trickplay index, one
  // relation the kernel followed. A file with no sheets — too short, or not
  // generated yet — is the list of names it has always been.
  const CHAPTER_THUMB_WIDTH = 160; // px; the sheets' own tiles are wider

  function chapterList(doc, ctx) {
    if (!doc || doc.medium === 'text') return '';
    const chapters = (doc.media_info && doc.media_info.chapters) || [];
    if (!chapters.length) return '';
    const idx = (ctx && ctx.trickplay) || null;
    return '<div id="detail-chapters">' + chapters.map(ch => {
      const tile = tileAt(idx, ch.start_seconds);
      return '<button class="chapter" data-seek="' + esc(ch.start_seconds) + '">' +
        (tile ? '<span class="ch-thumb" style="' + esc(tileStyle(tile, CHAPTER_THUMB_WIDTH)) + '"></span>' : '') +
        '<span class="ch-what">' +
          '<span class="ch-title">' + esc(ch.title || 'Chapter') + '</span>' +
          '<span class="ch-time">' + fmtTime(ch.start_seconds) + '</span>' +
        '</span></button>';
    }).join('') + '</div>';
  }

  // Which frame of which sheet stands for a moment, and where it sits in that
  // sheet. The index is a document like any other — its `sheet_hrefs` are the
  // server's addresses — and what happens here is arithmetic, not an address.
  function tileAt(idx, seconds) {
    const sheets = (idx && idx.sheet_hrefs) || [];
    if (!sheets.length || !idx.cols || !idx.rows || !idx.interval_seconds || !idx.tile_width) return null;
    const per = idx.cols * idx.rows;
    const frame = Math.min(Math.max(0, Math.floor(seconds / idx.interval_seconds)), sheets.length * per - 1);
    const tile = frame % per;
    const w = idx.tile_width, h = idx.tile_height || 180;
    return {
      href: sheets[Math.floor(frame / per)], w, h,
      x: -(tile % idx.cols) * w, y: -Math.floor(tile / idx.cols) * h,
      sheetW: idx.cols * w, sheetH: idx.rows * h,
    };
  }

  // One tile, drawn at the width the page has room for: the whole sheet is
  // scaled by the same factor, so the frame lands where it does at full size.
  function tileStyle(tile, width) {
    const s = width / tile.w;
    const px = n => Math.round(n * s) + 'px';
    return 'width:' + px(tile.w) + ';height:' + px(tile.h) + ';' +
      'background-image:url(' + tile.href + ');' +
      'background-size:' + px(tile.sheetW) + ' ' + px(tile.sheetH) + ';' +
      'background-position:' + px(tile.x) + ' ' + px(tile.y);
  }

  // The genres this title is filed under: the work's list when the kernel has
  // the work document, else the item's own enrichment, which is where the
  // item keeps what TMDB said. The first list that has anything in it wins —
  // works publish an empty one rather than none.
  function genresOf(doc, wk) {
    for (const g of [doc && doc.genres, wk && wk.genres,
                     doc && doc.enrichment && doc.enrichment.genres]) {
      if (Array.isArray(g) && g.length) return g;
    }
    return [];
  }

  // What the title IS, in one line under it: the certification as a badge,
  // how long it runs, and the genres as chips. Each is left out when the
  // document does not carry it — the line is facts, not a form with holes.
  function detailMeta(doc, wk) {
    const parts = [];
    if (doc.certification) parts.push('<span class="cert">' + esc(doc.certification) + '</span>');
    const runtime = fmtRuntime(doc.runtime_minutes);
    if (runtime) parts.push('<span class="runtime">' + esc(runtime) + '</span>');
    for (const g of genresOf(doc, wk)) parts.push('<span class="chip">' + esc(g) + '</span>');
    return parts.length ? '<div id="detail-meta">' + parts.join('') + '</div>' : '';
  }

  // Who is in it, in the order TMDB billed them. The server already cut the
  // list to the few names a page shows, so nothing is trimmed here.
  function castLine(doc) {
    const cast = (doc && doc.cast) || [];
    if (!cast.length) return '';
    return '<div id="detail-cast">With: ' + esc(cast.join(', ')) + '</div>';
  }

  // What the file IS, folded away at the foot of the page: the tech line, the
  // object it was read from, the identity the key was mapped to, and the probe
  // itself. Collapsed, because it answers "why does this transcode?" rather
  // than "what is this?" — and it is the one place that still says any of it.
  function details(doc) {
    const rows = [];
    if (doc.tech) rows.push(['Tech', doc.tech]);
    const file = doc.object_key || doc.label || '';
    if (file) rows.push(['File', file]);
    if (doc.identity && doc.identity.kind) rows.push(['Identity', doc.identity.kind]);
    for (const r of mediaFacts(doc.media_info)) rows.push(r);
    const admin = adminBlock(doc);
    if (!rows.length && !admin) return '';
    return '<details id="detail-details"><summary>Details</summary>' +
      (rows.length ? '<dl>' + rows.map(r =>
        '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>').join('') + '</dl>' : '') +
      admin +
      '</details>';
  }

  // The curatorial actions: what a person does to the RECORD, not to the
  // thing. They belong beside what the file IS rather than beside Play —
  // nobody opened the page to re-probe a file — so they live inside the
  // disclosure, and an unavailable one is its reason in words.
  const ADMIN = ['identity', 'reprobe', 'enrich'];

  function adminBlock(doc) {
    const parts = ADMIN.map(n => {
      const act = action(doc, n);
      if (act) return n === 'identity' ? identityForm(act, doc.identity) : control(n, act);
      const reason = why(doc, n);
      return reason ? '<div class="unavailable">' + esc(reason) + '</div>' : '';
    }).join('');
    return parts ? '<div id="detail-admin">' + parts + '</div>' : '';
  }

  // The identity form, drawn from the action's own INPUT SKETCH: one field
  // per entry, the sketch's type as the control's type ('number?' is an
  // optional number), and the current value from the document's `identity`.
  // Nothing here is a list of an identity's fields learned by heart — a
  // sketch that grows a field grows the form — and the submit is the
  // kernel's: this file only says what to send and where.
  const IDENTITY_ORDER = ['kind', 'title', 'year', 'season', 'episode', 'part', 'author', 'track_title'];

  // The sketch's fields in the order a person fills them in, with anything
  // the sketch has grown since after them.
  function sketchFields(sketch) {
    const keys = Object.keys(sketch || {});
    const known = IDENTITY_ORDER.filter(k => keys.includes(k));
    return known.concat(keys.filter(k => !known.includes(k)).sort());
  }

  function fieldLabel(k) {
    const s = String(k).replace(/_/g, ' ');
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  function identityForm(act, current) {
    if (!act) return '';
    const sketch = act.input || {};
    const cur = current || {};
    const fields = sketchFields(sketch).map(k => {
      const t = String(sketch[k] || 'string');
      const optional = t.endsWith('?');
      const type = t.replace(/\?$/, '') === 'number' ? 'number' : 'text';
      const v = cur[k];
      return '<label><span>' + esc(fieldLabel(k)) + '</span>' +
        '<input name="' + esc(k) + '" type="' + type + '"' + (optional ? '' : ' required') +
        ' value="' + esc(v == null ? '' : v) + '"></label>';
    }).join('');
    // data-act, data-href and data-method are what every control carries; the
    // form adds a BODY, which is why the kernel submits it rather than
    // clicking it.
    return '<form id="identity-form" class="actform" data-act="identity"' +
      ' data-href="' + esc(act.href) + '"' +
      ' data-method="' + esc(act.method || 'POST') + '">' +
      fields +
      '<button type="submit" class="act">' + esc(act.label || 'identity') + '</button>' +
      '</form>';
  }

  // The probe, said as it came: every field media_info carries, a list said as
  // how many of it there are. Nothing is named here, so a field the probe
  // grows shows up without this file learning it.
  function mediaFacts(info) {
    if (!info || typeof info !== 'object') return [];
    return Object.keys(info).map(k => {
      const v = info[k];
      if (Array.isArray(v)) return [k.replace(/_/g, ' '), String(v.length)];
      if (v == null || typeof v === 'object') return null;
      return [k.replace(/_/g, ' '), String(v)];
    }).filter(Boolean);
  }

  // Back from an episode goes to its show, from a track to its artist's
  // shelf, and from anything else to the grid — all three read off the work
  // document, none of them guessed at.
  function backHash(doc, wk) {
    if (!wk) return '#/';
    if (wk.work_kind === 'show') return showHash(wk.title);
    if (wk.work_kind === 'album' && wk.author) return artistHash(wk.author);
    return '#/';
  }
  function backLabel(doc, wk) {
    if (!wk) return 'Library';
    if (wk.work_kind === 'show') return wk.title || 'Back';
    if (wk.work_kind === 'album' && wk.author) return wk.author;
    return 'Library';
  }
  // The parenthesis after a title: the year the document gives, and for a
  // record the author beside it.
  function detailSub(doc, wk) {
    const parts = [];
    if (doc.year) parts.push('(' + doc.year + ')');
    if (wk && wk.author && wk.work_kind !== 'show') parts.push(wk.author);
    return parts.join(' · ');
  }

  // --- session -----------------------------------------------------------------

  // The player chrome. A READING session has none — the reader pane is its
  // own device and the kernel hands the document straight to it — so this
  // answers '' and the kernel knows to show the book instead.
  //
  // #device-slot is where the kernel puts the ONE persistent device element:
  // the <video> lives inside it and is created once for the life of the page,
  // so swapping this chrome for the next document's never drops the buffer.
  // No renderer ever emits a <video> tag; web/render_test.mjs checks that.
  //
  // #player-title — the way back and what is playing — is drawn here and MOVED
  // into the device by player.js (attach), where it overlays the top of the
  // picture, goes fullscreen with it and fades with the transport.
  function session(doc, ctx) {
    if (!doc || doc.method === 'read') return '';
    const c = ctx || {};
    const back = link(doc, 'back');
    const drawn = ['keep_watching', 'keep_reading', 'next', 'mark_in', 'mark_out', 'link', 'stop'];
    const marks = doc.marks || {};
    return '<div id="player-title">' +
        '<button class="back" id="detail-back">← ' + esc(back && back.title ? back.title : 'Back') + '</button>' +
        '<div id="now-playing">' + esc(nowPlaying(doc, c)) + '</div>' +
      '</div>' +
      '<div id="device-slot"></div>' +
      skipButtons(doc) +
      '<div id="doc-controls">' +
        prevNext(doc) +
        markChip('in', doc, marks.in, c) +
        markChip('out', doc, marks.out, c) +
        control('link', action(doc, 'link'), { id: 'btn-mark-link', title: why(doc, 'link') }) +
        control('stop', action(doc, 'stop'), { id: 'btn-stop-session' }) +
        restControls(doc, drawn) +
      '</div>' +
      '<div id="passage-link" hidden><span id="passage-link-note">Passage link</span>' +
        '<code id="passage-link-url"></code></div>' +
      '<div id="status"></div>' +
      '<div id="chapters"></div>' +
      '<div id="trace"></div>' +
      upNext(doc) +
      passageEndPanel(doc);
  }

  // The stretches the SESSION says are worth jumping over — the opening
  // titles, the closing credits — each as the button the server labelled it
  // with. Where it seeks to is where the stretch ends, carried the way a
  // chapter carries its start (data-seek), so pressing one goes through the
  // same seek every chapter button does.
  //
  // They are drawn hidden: player.js moves this box into the device, where it
  // overlays the picture and goes fullscreen with it, and shows the one the
  // position is actually inside.
  function skipButtons(doc) {
    const skips = (doc && doc.skips) || [];
    if (!skips.length) return '';
    return '<div id="skips">' + skips.map(s =>
      '<button class="skip" data-seek="' + esc(s.end) + '"' +
        ' data-skip-start="' + esc(s.start) + '" data-skip-end="' + esc(s.end) + '"' +
        ' hidden>' + esc(s.label || '') + '</button>').join('') + '</div>';
  }

  // What is playing, said the way the medium says it: a record names the work
  // and the part, everything else is the item's own title.
  function nowPlaying(doc, ctx) {
    const wk = (ctx && ctx.work) || null;
    if (wk && wk.work_kind !== 'show' && (wk.members || []).length > 1) {
      const list = walked(wk);
      const i = list.findIndex(m => m.id === doc.item_id);
      const m = i >= 0 ? list[i] : null;
      return wk.title + (m ? ' · ' + m.label + ' of ' + list.length : '');
    }
    return doc.title || '';
  }

  // Prev and next are the session's own relations — the work's order, which
  // is the server's. `next` is both a link (where it goes) and an action
  // (starting it on this same device without a new decision).
  function prevNext(doc) {
    const p = link(doc, 'prev'), n = link(doc, 'next');
    return (p ? '<button id="btn-prev-ep" data-goto="' + esc(idIn(p.href)) +
        '" title="' + esc(p.title || '') + '">⏮ Prev</button>' : '') +
      (n ? '<button id="btn-next-ep" data-goto="' + esc(idIn(n.href)) +
        '" title="' + esc(n.title || '') + '">Next ⏭</button>' : '');
  }

  // A mark chip: the action's label until the mark is set, then the time it
  // was set at, with a × that clears it. Both ends of the passage being made
  // are the SESSION's — this only shows what came back.
  function markChip(kind, doc, mark, ctx) {
    const act = action(doc, 'mark_' + kind);
    if (!act) return '';
    const set = !!mark;
    const here = set && mark.item_id === doc.item_id;
    const label = set
      ? (kind === 'in' ? 'In ' : 'Out ') + (here ? '' : markElsewhere(mark, ctx)) + fmtTime(mark.seconds)
      : act.label;
    return '<span class="markchip' + (set ? ' set' : '') + '" id="chip-mark-' + kind + '">' +
      control('mark_' + kind, act, { id: 'btn-mark-' + kind, label }) +
      '<button class="mark-clear" data-act="mark_' + kind + '" data-clear="1"' +
        ' data-href="' + esc(act.href) + '" data-method="' + esc(act.method || 'POST') + '"' +
        ' title="Clear the ' + kind + ' point"' + (set ? '' : ' hidden') + '>×</button>' +
      '</span>';
  }
  // A mark set in another member of the run says which one, from the work
  // document the kernel already has.
  function markElsewhere(mark, ctx) {
    const wk = (ctx && ctx.work) || null;
    if (!wk) return '';
    const m = walked(wk).find(x => x.id === mark.item_id);
    return m && m.label ? m.label + ' ' : '';
  }

  // Up next: the run's next member, named by the session. Hidden until the
  // item ends; the player shows it.
  function upNext(doc) {
    const n = link(doc, 'next');
    const act = action(doc, 'next');
    if (!n && !act) return '';
    return '<div id="upnext" hidden>' +
      '<div id="upnext-title">Up next: ' + esc((n && n.title) || (act && act.label) || '') + '</div>' +
      '<div id="upnext-count"></div>' +
      control('next', act, { id: 'upnext-play', label: (act && act.label) || 'Play next', hidden: true }) +
      '<button id="upnext-cancel">Cancel</button>' +
      '</div>';
  }

  // The end-of-passage panel: the two ways out, both the document's — the
  // keep_watching action labelled as the server labels it (a film is watched
  // on, an album listened on), and the back link, which names what going back
  // goes back to.
  function passageEndPanel(doc) {
    const keep = action(doc, 'keep_watching') || action(doc, 'keep_reading');
    const name = action(doc, 'keep_watching') ? 'keep_watching' : 'keep_reading';
    const back = link(doc, 'back');
    if (!keep) return '';
    return '<div id="passage-end" hidden>' +
      '<div id="passage-end-title">End of the passage</div>' +
      '<div id="passage-end-sub"></div>' +
      control(name, keep, { id: 'passage-keep' }) +
      // The third way out is not to leave at all: A-B repeat, which is the
      // device's own doing — the passage is played again from its start
      // instead of stopping at its end — so it carries no action.
      '<button id="passage-loop" aria-pressed="false" title="Play this passage again and again">' +
        '↻ Loop</button>' +
      '<button id="passage-back"' + (back ? ' title="Back to ' + esc(back.title || '') + '"' : '') +
        '>Back</button>' +
      '</div>';
  }

  // --- the mini player ---------------------------------------------------------

  // The other shape the session chrome comes in. Music does not stop because
  // somebody went looking for the next record: leaving an audio item COLLAPSES
  // its sitting into this bar (kernel.js holds that rule) instead of ending it.
  //
  // It is the same session document, said in one line — the picture, what is
  // playing, the work it belongs to — with the device's own play/pause and
  // ±30s beside it and a thin line of progress under it. #device-slot is here
  // too, inside a hidden box: the ONE media element moves into it, which is
  // what keeps the buffer and the sound across the document swap.
  //
  // The bar itself is a tap back to the item's own page, where the full chrome
  // is drawn again — a hash, which is the client's grammar, never an address.
  function miniPlayer(doc, ctx) {
    if (!doc || doc.method === 'read') return '';
    const c = ctx || {};
    const art = artwork(doc);
    const wk = (link(doc, 'work') || {}).title || (c.work && c.work.title) || '';
    return '<div id="mini-bar" data-nav="' + esc(itemHash(doc.item_id)) + '" data-expand="1"' +
        ' tabindex="0" role="button" title="Back to the player">' +
      (art ? '<img id="mini-art" src="' + esc(art) + '" alt="">' : '') +
      '<div id="mini-what">' +
        '<div id="mini-title">' + esc(doc.title || '') + '</div>' +
        (wk ? '<div id="mini-work">' + esc(wk) + '</div>' : '') +
      '</div>' +
      '<div id="mini-controls">' +
        '<button id="mini-back" title="Back 30s" aria-label="Back 30 seconds">⏪</button>' +
        '<button id="mini-play" title="Play/Pause" aria-label="Play/Pause">⏵</button>' +
        '<button id="mini-fwd" title="Forward 30s" aria-label="Forward 30 seconds">⏩</button>' +
        // The speed, as a tap that walks the list rather than a menu: the bar
        // has room for one word. player.js writes the word and hides it while
        // the television is playing, which has no speed to set.
        '<button id="mini-rate" title="Playback speed" aria-label="Playback speed">1×</button>' +
        // The moon opens the sleep timer. Its list is the device's own element,
        // moved in here by player.js — the bar keeps the device in a hidden
        // box, so a menu drawn inside it could never be seen.
        '<button id="mini-sleep" title="Sleep timer" aria-label="Sleep timer">🌙</button>' +
        // The five above are the DEVICE's and player.js answers them. This one
        // is the document's: the session's own stop, said in the server's words,
        // so a sitting can be ended from the bar without opening it back up.
        control('stop', action(doc, 'stop'), { id: 'mini-stop' }) +
      '</div>' +
      '<div id="mini-device" hidden><div id="device-slot"></div></div>' +
      '<div id="mini-progress"><div id="mini-fill"></div></div>' +
      '</div>';
  }

  // --- who is signed in --------------------------------------------------------
  //
  // The root says it: `viewer` is the household member the server read this
  // page as, and links.logout / links.login are the two doors. A server with
  // no auth configured carries neither, and then the panel says nothing about
  // accounts at all — there is no account.
  //
  // Both doors are plain <a>s rather than buttons: the server answers them
  // with a redirect to Keycloak, which is a navigation, not a fetch. The
  // sign-in door takes `returnTo` — where the browser was, hash and all —
  // because the server cannot see a hash and the hash is the place.
  function account(doc, returnTo) {
    const viewer = doc && doc.viewer;
    const out = link(doc, viewer ? 'logout' : 'login');
    if (!out) return viewer ? '<span class="acct-who">Signed in as ' + esc(viewer.name || '') + '</span>' : '';
    const href = viewer ? out.href : out.href + '?return_to=' + encodeURIComponent(returnTo || '');
    return (viewer ? '<span class="acct-who">Signed in as ' + esc(viewer.name || '') + '</span>' : '') +
      '<a class="acct-link" href="' + esc(href) + '">' +
      esc(out.title || (viewer ? 'Sign out' : 'Sign in')) + '</a>';
  }

  // --- the page as an app ------------------------------------------------------
  //
  // A phone's idea of an app is an icon on the home screen, and the browser
  // offers to put one there — as an event, fired once, and only where it would
  // actually work. The kernel keeps that event; this draws the button while it
  // is held. iOS Safari fires no such event and hides Add to Home Screen in the
  // share menu instead, so there the row is the one sentence that says where to
  // look. Already on the home screen, or a desktop the browser offered nothing:
  // there is nothing to say, and the row is not drawn.
  function install(state) {
    const s = state || {};
    if (s.standalone) return '';
    if (s.prompt) return '<button id="settings-install">Install flickr</button>';
    if (s.ios) return '<span class="install-hint">Add to Home Screen from the share menu</span>';
    return '';
  }

  // --- the trace ---------------------------------------------------------------
  // Not a document kind of its own: the decision the session document carries,
  // drawn for the system panel under the player. The badge line stays out in
  // the open — one quiet line saying what is happening — and the step by step
  // "why" hides behind a disclosure, because nobody sitting down to watch
  // something asked for it.
  function trace(decision, encoder, dest) {
    if (!decision) return '';
    const m = decision.method || '';
    const cls = m === 'direct_play' ? 'direct' : m === 'transcode' ? 'transcode' : 'deny';
    const steps = decision.trace || [];
    if (!steps.length) {
      return { status: statusLine(cls, m, steps, encoder, dest), trace: '' };
    }
    return {
      status: statusLine(cls, m, steps, encoder, dest),
      trace: '<details><summary>' + esc(traceQuestion(m)) + '</summary>' + steps.map(s =>
        '<div class="step ' + (s.passed ? 'pass' : 'fail') + '">' +
        '<span class="mark">' + (s.passed ? '✓' : '✗') + '</span>' +
        '<span class="check">' + esc(s.check) + '</span>' +
        '<span class="detail">' + esc(s.detail) + '</span></div>').join('') +
        '</details>',
    };
  }

  function statusLine(cls, m, steps, encoder, dest) {
    return '<span class="badge ' + cls + '">' + esc(m.replace('_', ' ')) + '</span>' +
      esc((steps.length && steps[steps.length - 1].detail) || '') +
      (encoder ? ' <span style="color:#8db4f0">via ' + esc(encoder) + '</span>' : '') +
      (dest ? ' <span style="color:#8db4f0">→ ' + esc(dest) + '</span>' : '');
  }

  // The disclosure asks the question the viewer would ask, in the words of
  // what actually happened.
  function traceQuestion(method) {
    if (method === 'direct_play') return 'Why does this play directly?';
    if (method === 'transcode') return 'Why is this transcoding?';
    return 'Why can’t this play?';
  }

  const api = {
    library, libraryGrid, hero, work, artist, item, session, miniPlayer, continueShelf, search,
    lines,
    account, install,
    card, memberRow, control, watchedControl, bookmarkControl, restControls, trace, identityForm,
    esc, fmtTime, fmtRuntime, hashFor, itemHash, showHash, artistHash, searchHash,
    idIn,
    // The sprite-sheet arithmetic, shared with the device: the scrub bar's
    // hover preview cuts its frame out of the same sheets this list does.
    tileAt,
    linkHref: href, actionOf: action, unavailableReason: why, artworkOf: artwork,
    SILENT_ACTIONS: SILENT,
  };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Render = api;
})(typeof window !== 'undefined' ? window : this);
