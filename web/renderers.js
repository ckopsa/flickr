// flickr — the renderers: one per document kind, each a PURE FUNCTION of the
// envelope (docs/hypermedia.md §The client).
//
//   library   the grid, its genre chips and the count the header's search filtered
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
    return '<button class="act' + (o.cls ? ' ' + esc(o.cls) : '') + '"' +
      ' data-act="' + esc(name) + '"' +
      ' data-href="' + esc(act.href) + '"' +
      ' data-method="' + esc(act.method || 'POST') + '"' +
      (o.title ? ' title="' + esc(o.title) + '"' : '') +
      (o.hidden ? ' hidden' : '') +
      (o.id ? ' id="' + esc(o.id) + '"' : '') +
      '>' + esc(o.label || act.label || name) + '</button>';
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
    const wrap = art
      ? '<div class="poster-wrap"><img loading="lazy" alt="" src="' + esc(art) + '"></div>'
      : '<div class="poster-wrap text-tile"><div class="tile-title">' + esc(title) +
        '</div><div class="tile-sub">' + esc(t.subtitle || t.tech || '') + '</div></div>';
    // A tile is a div, so the keyboard would walk straight past it: tabindex
    // puts it in the tab order and the role says what activating it does.
    return '<div class="' + esc(cls || 'card') + '" tabindex="0" role="link"' +
      ' data-nav="' + esc(hashFor(t)) + '">' +
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

  function libraryGrid(doc, state) {
    const tiles = (doc && doc.items) || [];
    if (!tiles.length) return { html: '<div id="empty">No items yet — run a scan.</div>', count: 0 };
    const kept = tiles.filter(t => matches(t, state));
    if (!kept.length) return { html: '<div id="empty">Nothing matches your search.</div>', count: 0 };
    return { html: kept.map(t => card(t)).join(''), count: kept.length };
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

  function library(doc, state) {
    const s = state || {};
    const grid = libraryGrid(doc, s);
    return '<div id="lib-note"' + (s.note ? '' : ' hidden') + '>' + esc(s.note || '') + '</div>' +
      genreRow(doc, s) +
      '<div id="cw" hidden></div>' +
      // The search box is the header's — it outlives this render, so the count
      // is all the bar carries.
      '<div id="lib-bar">' +
        '<span id="lib-count">' + (grid.count ? esc(grid.count + ' title' + (grid.count === 1 ? '' : 's')) : '') + '</span>' +
      '</div>' +
      '<div id="grid">' + grid.html + '</div>';
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
      return '<div class="cw-card" tabindex="0" role="button"' +
        (act ? ' data-act="resume" data-href="' + esc(act.href) + '"' +
               ' data-method="' + esc(act.method || 'POST') + '"' : '') +
        ' data-nav="' + esc(itemHash(en.id)) + '" data-autoplay="1">' +
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
    const meta = '<div class="title">' + esc(m.label || m.title || '') + '</div>' +
      (m.duration_seconds ? '<div class="meta">' + esc(fmtTime(m.duration_seconds)) + '</div>' : '') +
      (m.overview ? '<div class="overview">' + esc(m.overview) + '</div>' : '');
    const cls = 'item' + (still ? ' enriched' : '') + (o.current ? ' current' : '');
    return '<div class="' + cls + '" tabindex="0" role="link"' +
      ' data-nav="' + esc(itemHash(m.id)) + '">' +
      (still ? '<img class="still" loading="lazy" alt="" src="' + esc(still) + '">' : '') +
      '<div>' + meta + '</div>' +
      '</div>';
  }

  function members(doc) { return (doc && doc.members) || []; }
  function walked(doc) { return members(doc).filter(m => !m.extra); }
  function extras(doc) { return members(doc).filter(m => m.extra); }

  // The show pane: the episodes, then the bonus material under its own
  // heading — both in the document's order, and both told apart by the
  // member's own `extra` flag rather than by the browser reading an identity.
  function showPane(doc) {
    const eps = walked(doc), bonus = extras(doc);
    const drawn = ['play', 'read', 'passage'];
    return backTo('#/', 'Library') +
      '<h2 id="show-title">' + esc(doc.title || '') + '</h2>' +
      (doc.progress && doc.progress.text
        ? '<div id="show-note">' + esc(doc.progress.text) + '</div>' : '') +
      '<div id="work-actions">' + primary(doc) + restControls(doc, drawn) +
        passagePicker(doc) + '</div>' +
      '<div id="episode-list"' + (eps.length ? '' : ' hidden') + '>' +
        eps.map(m => memberRow(m)).join('') + '</div>' +
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
    const drawn = ['play', 'read', 'passage'];
    return backTo('#/', 'Library') +
      '<div id="audio-pane"><div id="audio-head">' +
        (art ? '<img id="audio-cover" alt="" src="' + esc(art) + '">' : '') +
        '<div><div id="audio-title">' + esc(doc.title || '') +
          (doc.year ? ' (' + esc(doc.year) + ')' : '') + '</div>' +
          '<div id="audio-author">' + esc(doc.author || '') + '</div>' +
          '<div id="audio-part">' + esc((doc.progress && doc.progress.text) || '') + '</div>' +
          '<div id="work-actions">' + primary(doc) + restControls(doc, drawn) +
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
    const chapters = ((doc.media_info && doc.media_info.chapters) || [])
      .filter(() => doc.medium !== 'text');
    const drawn = ['play', 'read'];
    const prev = link(doc, 'prev'), next = link(doc, 'next');
    const bonus = wk && wk.work_kind !== 'show' ? extras(wk) : [];
    const siblings = wk && wk.work_kind !== 'show' ? walked(wk) : [];

    return backTo(backHash(doc, wk), backLabel(doc, wk)) +
      '<div id="detail-info">' +
        (art ? '<img id="detail-poster" alt="" src="' + esc(art) + '">' : '') +
        '<div id="detail-body">' +
          '<h2><span id="detail-title">' + esc(doc.title || '') + '</span> ' +
            '<span id="detail-year">' + esc(detailSub(doc, wk)) + '</span></h2>' +
          '<p id="detail-overview"' + (doc.overview ? '' : ' hidden') + '>' + esc(doc.overview || '') + '</p>' +
          primary(doc) + restControls(doc, drawn) +
          '<div id="detail-chapters">' + chapters.map(ch =>
            '<button data-seek="' + esc(ch.start_seconds) + '">' +
            esc((ch.title || '') + ' · ' + fmtTime(ch.start_seconds)) + '</button>').join('') + '</div>' +
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
      details(doc);
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
    if (!rows.length) return '';
    return '<details id="detail-details"><summary>Details</summary>' +
      '<dl>' + rows.map(r =>
        '<dt>' + esc(r[0]) + '</dt><dd>' + esc(r[1]) + '</dd>').join('') + '</dl>' +
      '</details>';
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
  function session(doc, ctx) {
    if (!doc || doc.method === 'read') return '';
    const c = ctx || {};
    const back = link(doc, 'back');
    const drawn = ['keep_watching', 'keep_reading', 'next', 'mark_in', 'mark_out', 'link', 'stop'];
    const marks = doc.marks || {};
    return '<button class="back" id="detail-back">← ' + esc(back && back.title ? back.title : 'Back') + '</button>' +
      '<div id="now-playing">' + esc(nowPlaying(doc, c)) + '</div>' +
      '<div id="device-slot"></div>' +
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
      '<button id="passage-back"' + (back ? ' title="Back to ' + esc(back.title || '') + '"' : '') +
        '>Back</button>' +
      '</div>';
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
    library, libraryGrid, work, artist, item, session, continueShelf,
    card, memberRow, control, restControls, trace,
    esc, fmtTime, hashFor, itemHash, showHash, artistHash,
    idIn,
    linkHref: href, actionOf: action, unavailableReason: why, artworkOf: artwork,
    SILENT_ACTIONS: SILENT,
  };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Render = api;
})(typeof window !== 'undefined' ? window : this);
