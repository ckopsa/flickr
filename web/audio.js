// flickr — the audio pane: audiobooks and albums.
//
// An audio item (media_info.medium "audio": an audiobook part, a track) plays
// through the SAME player as a film — the media element, hls.js or a direct
// src, the scrub bar and chapter ticks, saveProgress() and its passage gate,
// the passage predicates from passage.js, the up-next countdown and casting
// — because a stream with no picture is still a stream, and a second player
// would have been a second copy of every rule. What differs is what the page
// shows around it: the cover where the picture would be, the work's parts or
// tracks (played in order; up-next walks them the way it walks episodes), and
// the chapter list. index.html calls in at a handful of hooks:
//
//   isAudioItem(it)            medium test (buildLibrary, episodeSiblings, …)
//   artworkUrl(id)             /cover for audio items, /poster otherwise
//   renderAudioCards(grid, items, q)   one tile per audiobook / album
//   renderAudioDetail(item)    the detail pane's author line and parts list
//   audioMode(item | null)     entering / leaving audio playback
//   audioSiblings(item)        prev / next part, for nav and up-next
//   partLabel(item)            "Part 3" / "Track 7" / the file name
//
// Loaded as a plain script before index.html's own, so nothing here runs at
// parse time beyond defining functions — the page's state (allItems,
// currentItem, passage) is read only when a hook is called.
(function (root) {
  'use strict';

  const AUDIO_KINDS = new Set(['audiobook_part', 'track']);

  // The medium is the file's, not the path's: an unidentified .mp3 is audio
  // too. Identity kinds cover rows probed before the medium field existed.
  function isAudioItem(it) {
    return !!it && (it.media_info?.medium === 'audio' || AUDIO_KINDS.has(it.identity?.kind));
  }

  // Tiles ask for artwork by item id. The server's /cover route falls back to
  // the poster itself, but it only runs an extraction for audio items, so
  // video tiles keep asking for the poster directly.
  function artworkUrl(id) {
    const it = allItems.find(i => i.id === id);
    return `/api/items/${id}/${isAudioItem(it) ? 'cover' : 'poster'}`;
  }

  // Zero (and the omitted zero the server sends as undefined) = unnumbered.
  function partNo(it) { return it.identity?.part || 0; }

  // Mirrors internal/works.finish for audio kinds: by part number, the
  // unnumbered after the numbered and in key order, then by id.
  function byPartOrder(a, b) {
    const pa = partNo(a), pb = partNo(b);
    if (pa !== pb) {
      if (!pa || !pb) return pa ? -1 : 1;
      return pa - pb;
    }
    return a.object_key.localeCompare(b.object_key) || a.id - b.id;
  }

  // Mirrors internal/works.Build for the audio kinds: one work per (kind,
  // author, title), case-insensitively; a file the path could not place is a
  // work of its own. Title order, key as the tie-break.
  function audioWorks(items) {
    const byKey = new Map();
    for (const it of items) {
      const kind = it.identity?.kind;
      const workKind = kind === 'audiobook_part' ? 'audiobook' : kind === 'track' ? 'album' : 'file';
      const key = workKind === 'file' ? `file:${it.id}`
        : `${workKind}\0${(it.identity.author || '').toLowerCase()}\0${(it.identity.title || '').toLowerCase()}`;
      let w = byKey.get(key);
      if (!w) { w = { key, kind: workKind, title: '', author: '', year: 0, duration: 0, items: [] }; byKey.set(key, w); }
      w.items.push(it);
    }
    const works = [...byKey.values()];
    for (const w of works) {
      w.items.sort(byPartOrder);
      w.title = w.items.find(i => i.identity?.title)?.identity.title || baseName(w.items[0].object_key);
      w.author = w.items.find(i => i.identity?.author)?.identity.author || '';
      w.year = w.items.find(i => i.identity?.year)?.identity.year || 0;
      w.duration = w.items.reduce((s, i) => s + (i.media_info?.duration_seconds || 0), 0);
    }
    return works.sort((a, b) => a.title.localeCompare(b.title) || a.key.localeCompare(b.key));
  }

  function audioWorkOf(item) {
    return audioWorks(allItems.filter(isAudioItem)).find(w => w.items.some(i => i.id === item.id))
      || { key: `file:${item.id}`, kind: 'file', title: baseName(item.object_key), author: '', year: 0,
           duration: item.media_info?.duration_seconds || 0, items: [item] };
  }

  // The neighbours in play order — what prev/next and up-next move to.
  // Mirrors internal/works.nextMember (audio works carry no bonus material).
  function audioSiblings(item) {
    const w = audioWorkOf(item);
    const i = w.items.findIndex(m => m.id === item.id);
    if (i < 0) return null;
    return { prev: w.items[i - 1] || null, next: w.items[i + 1] || null };
  }

  function partNoun(w) { return w.kind === 'album' ? 'track' : 'part'; }

  // Mirrors internal/works.itemLabel: a number when the path gave one, else
  // the file name — "Part 0" would be a number nobody wrote.
  function partLabel(it) {
    const n = partNo(it);
    if (!n) return baseName(it.object_key);
    return `${it.identity?.kind === 'track' ? 'Track' : 'Part'} ${n}`;
  }

  // One tile per work on the library grid. Audio carries no TMDB genres, so
  // an active genre chip shows none of it. Returns how many tiles were added.
  function renderAudioCards(grid, items, q) {
    if (activeGenre || !items.length) return 0;
    let count = 0;
    for (const w of audioWorks(items)) {
      if (q && !w.title.toLowerCase().includes(q) && !w.author.toLowerCase().includes(q)) continue;
      const n = w.items.length;
      const mi = w.items[0].media_info;
      grid.appendChild(makeCard({
        posterId: w.items[0].id,
        title: w.title,
        sub: [w.author, w.year ? String(w.year) : '', n > 1 ? `${n} ${partNoun(w)}s` : ''].filter(Boolean).join(' · '),
        tech: [w.kind === 'file' ? 'audio' : w.kind, mi?.audio_codec, w.duration ? fmtTime(w.duration) : null]
          .filter(Boolean).join(' · '),
        onClick: () => navigate(itemHash(w.items[0].id)),
      }));
      count++;
    }
    return count;
  }

  // The pane's content: cover, title, author, where this item sits in the
  // work, and the work's members with the current one marked. Clicking a
  // member while playing moves the player there (route replaced, like an
  // episode); from the detail pane it opens that member's own page.
  function fillAudioPane(w, item) {
    const cover = $('audio-cover');
    cover.hidden = false;
    cover.onerror = () => { cover.hidden = true; };
    cover.src = artworkUrl(item.id);
    $('audio-title').textContent = w.title + (w.year ? ` (${w.year})` : '');
    $('audio-author').textContent = w.author;
    const many = w.items.length > 1;
    $('audio-part').textContent = many ? `${partLabel(item)} of ${w.items.length}` : '';
    const head = $('audio-list-head');
    head.hidden = !many;
    head.textContent = many
      ? `${w.items.length} ${partNoun(w)}s${w.duration ? ' · ' + fmtTime(w.duration) : ''}` : '';
    const list = $('audio-list');
    list.innerHTML = '';
    list.hidden = !many;
    for (const m of w.items) {
      const row = document.createElement('div');
      row.className = 'item' + (m.id === item.id ? ' current' : '');
      const dur = m.media_info?.duration_seconds;
      const chapters = m.media_info?.chapters?.length;
      const meta = [partNo(m) ? baseName(m.object_key) : null, dur ? fmtTime(dur) : null,
                    chapters ? `${chapters} ch` : null].filter(Boolean).join(' · ');
      row.innerHTML = `<div class="title">${esc(partLabel(m))}</div><div class="meta">${esc(meta)}</div>`;
      row.onclick = () => {
        if (currentItem && currentItem.id === m.id) return;
        if (currentItem) goToEpisode(m, 'nav'); else navigate(itemHash(m.id));
      };
      list.appendChild(row);
    }
  }

  // Detail-pane hook: the author beside the year, the parts list below; the
  // prev/next links give way to that list. Hides the pane for anything that
  // is not audio.
  function renderAudioDetail(item) {
    const pane = $('audio-pane');
    if (!isAudioItem(item)) { pane.hidden = true; return; }
    const w = audioWorkOf(item);
    const year = item.identity?.year;
    $('detail-year').textContent = [year ? `(${year})` : '', w.author].filter(Boolean).join(' · ');
    $('detail-epnav').hidden = true;
    fillAudioPane(w, item);
    pane.hidden = false;
  }

  // Playback hook: entering audio mode hides the picture-less media element
  // (CSS, body.audio-mode) and puts the cover, the work and the current part
  // where it was; null leaves the mode (closePlayer).
  function audioMode(item) {
    const on = isAudioItem(item);
    document.body.classList.toggle('audio-mode', on);
    if (!on) return;
    const w = audioWorkOf(item);
    fillAudioPane(w, item);
    $('audio-pane').hidden = false;
    $('now-playing').textContent = w.items.length > 1
      ? `${w.title} · ${partLabel(item)} of ${w.items.length}` : w.title;
  }

  Object.assign(root, { isAudioItem, artworkUrl, audioWorks, audioSiblings, partLabel,
                        renderAudioCards, renderAudioDetail, audioMode });
})(typeof window !== 'undefined' ? window : this);
