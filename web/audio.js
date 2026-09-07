// flickr — the audio pane: audiobooks and albums, and the artist's shelf.
//
// An audio item (media_info.medium "audio": an audiobook part, a track) plays
// through the SAME player as a film — the media element, hls.js or a direct
// src, the scrub bar and chapter ticks, saveProgress() and its passage gate,
// the passage predicates from passage.js, the up-next countdown and casting
// — because a stream with no picture is still a stream, and a second player
// would have been a second copy of every rule. What differs is what the page
// shows around it: the cover where the picture would be, the work's parts or
// tracks (played in order, tracks by their titles; up-next walks them the way
// it walks episodes), and the chapter list. Music adds one view of its own:
// the artist's shelf (#/artist/<name>), the albums filed under a name in
// year order, each opening its own pane; the grid shows one tile per artist
// rather than one per album. index.html calls in at a handful of hooks:
//
//   isAudioItem(it)            medium test (buildLibrary, episodeSiblings, …)
//   artworkUrl(id)             /cover for audio and text items, /poster otherwise
//   renderAudioCards(grid, items, q)   one tile per audiobook / artist
//   renderAudioDetail(item)    the detail pane's author line and parts list
//   audioMode(item | null)     entering / leaving audio playback
//   audioSiblings(item)        prev / next part, for nav and up-next
//   partLabel(item)            "Part 3" / "Track 7 · Karma Police" / the file name
//   renderArtist(name)         the #/artist/<name> view; false when unknown
//   audioBackHash(item)        where Back goes from a track: its artist's shelf
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
    const own = isAudioItem(it) || (it?.media_info?.medium && it.media_info.medium !== 'video');
    return `/api/items/${id}/${own ? 'cover' : 'poster'}`;
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

  // A track's own name (identity.track_title); empty for parts, and for a
  // track identified before the path grammar kept it.
  function trackTitle(it) { return it.identity?.track_title || ''; }

  // The member's place: "Part 3" / "Track 7" when the path gave a number,
  // else the file name — "Part 0" would be a number nobody wrote.
  function partNumLabel(it) {
    const n = partNo(it);
    if (!n) return baseName(it.object_key);
    return `${it.identity?.kind === 'track' ? 'Track' : 'Part'} ${n}`;
  }

  // Mirrors internal/works.itemLabel: the place, and for a track its title
  // after it ("Track 7 · Karma Police"); an unnumbered track is its title.
  function partLabel(it) {
    const title = trackTitle(it);
    if (!title) return partNumLabel(it);
    return partNo(it) ? `${partNumLabel(it)} · ${title}` : title;
  }

  // Mirrors internal/works.Artists: the album works shelved by artist,
  // case-insensitively, in name order; each shelf's albums in year order (the
  // year-less last, then title), the name spelled as the first album spells
  // it. An album with no artist has no shelf and stays a tile of its own.
  function artists(works) {
    const albums = works.filter(w => w.kind === 'album' && w.author)
      .sort((a, b) => a.author.toLowerCase().localeCompare(b.author.toLowerCase())
        || ((a.year && b.year) ? a.year - b.year : (a.year ? -1 : b.year ? 1 : 0))
        || a.title.toLowerCase().localeCompare(b.title.toLowerCase()) || a.key.localeCompare(b.key));
    const out = [];
    for (const w of albums) {
      const last = out[out.length - 1];
      if (!last || last.name.toLowerCase() !== w.author.toLowerCase()) out.push({ name: w.author, albums: [] });
      out[out.length - 1].albums.push(w);
    }
    return out;
  }

  function artistHash(name) { return '#/artist/' + encodeURIComponent(name); }
  // The route spells the name as a tile or a link did; match it the way the
  // shelf groups, case-insensitively.
  function findArtist(name) {
    const lower = String(name || '').toLowerCase();
    return artists(audioWorks(allItems.filter(isAudioItem))).find(a => a.name.toLowerCase() === lower) || null;
  }

  function albumSub(w) {
    const n = w.items.length;
    return [w.year ? String(w.year) : '', n > 1 ? `${n} ${partNoun(w)}s` : ''].filter(Boolean).join(' · ');
  }
  function albumTech(w) {
    const mi = w.items[0].media_info;
    return [w.kind === 'file' ? 'audio' : w.kind, mi?.audio_codec, w.duration ? fmtTime(w.duration) : null]
      .filter(Boolean).join(' · ');
  }
  // A work's tile: its first member's cover; tapping opens that member's pane,
  // which lists the whole work.
  function workCard(w, title, sub) {
    return makeCard({
      posterId: w.items[0].id, title, sub, tech: albumTech(w),
      onClick: () => navigate(itemHash(w.items[0].id)),
    });
  }

  // The library grid: one tile per audiobook, one per ARTIST (the shelf of
  // albums behind it), and one per album nobody filed under an artist. Audio
  // carries no TMDB genres, so an active genre chip shows none of it. The
  // search matches an artist by name or by any album on the shelf. Returns
  // how many tiles were added.
  function renderAudioCards(grid, items, q) {
    if (activeGenre || !items.length) return 0;
    let count = 0;
    const works = audioWorks(items);
    const match = (s) => !q || s.toLowerCase().includes(q);
    for (const w of works) {
      if (w.kind === 'album' && w.author) continue; // on its artist's shelf below
      if (!match(w.title) && !match(w.author)) continue;
      grid.appendChild(workCard(w, w.title, [w.author, albumSub(w)].filter(Boolean).join(' · ')));
      count++;
    }
    for (const a of artists(works)) {
      if (!match(a.name) && !a.albums.some(w => match(w.title))) continue;
      const years = a.albums.map(w => w.year).filter(Boolean);
      const span = years.length ? (years[0] === years[years.length - 1] ? String(years[0]) : `${years[0]}–${years[years.length - 1]}`) : '';
      const n = a.albums.length;
      grid.appendChild(makeCard({
        posterId: a.albums[0].items[0].id,
        title: a.name,
        sub: `${n} album${n === 1 ? '' : 's'}`,
        tech: ['artist', span].filter(Boolean).join(' · '),
        onClick: () => navigate(artistHash(a.name)),
      }));
      count++;
    }
    return count;
  }

  // The artist's shelf (#/artist/<name>): the first album's cover beside the
  // name, then the albums as tiles in year order; a tap opens the album's
  // pane. Builds its own DOM inside index.html's empty #view-artist, so the
  // page carries one line for it. Returns false for a name no album is filed
  // under — the route then falls back to the library.
  function renderArtist(name) {
    const a = findArtist(name);
    if (!a) return false;
    const view = $('view-artist');
    view.innerHTML = '';
    const back = document.createElement('button');
    back.className = 'back';
    back.textContent = '← Library';
    back.onclick = () => navigate('#/');
    view.appendChild(back);
    const head = document.createElement('div');
    head.id = 'artist-head';
    const cover = document.createElement('img');
    cover.id = 'artist-cover';
    cover.alt = '';
    cover.src = artworkUrl(a.albums[0].items[0].id);
    cover.onerror = () => { cover.hidden = true; };
    head.appendChild(cover);
    const n = a.albums.length, tracks = a.albums.reduce((s, w) => s + w.items.length, 0);
    head.insertAdjacentHTML('beforeend',
      `<div><h2 id="artist-name">${esc(a.name)}</h2>` +
      `<div id="artist-sub">${n} album${n === 1 ? '' : 's'} · ${tracks} track${tracks === 1 ? '' : 's'}</div></div>`);
    view.appendChild(head);
    const shelf = document.createElement('div');
    shelf.id = 'artist-albums';
    for (const w of a.albums) shelf.appendChild(workCard(w, w.title, albumSub(w)));
    view.appendChild(shelf);
    showView('view-artist');
    return true;
  }

  // Back from a track's pane goes to its artist's shelf, the way an episode's
  // goes to its show; anything else answers null and Back goes to the grid.
  function audioBackHash(item) {
    if (item?.identity?.kind !== 'track' || !item.identity.author) return null;
    return findArtist(item.identity.author) ? artistHash(item.identity.author) : null;
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
    const own = trackTitle(item);
    $('audio-part').textContent = many
      ? `${partNumLabel(item)} of ${w.items.length}${own ? ' · ' + own : ''}` : '';
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
      // A titled track: the title is the row, its number the meta. Without a
      // title (a part, or a track from before titles) the number is the row
      // and the file name says what it is.
      const own = trackTitle(m);
      const meta = [own ? (partNo(m) ? partNumLabel(m) : null) : (partNo(m) ? baseName(m.object_key) : null),
                    dur ? fmtTime(dur) : null, chapters ? `${chapters} ch` : null].filter(Boolean).join(' · ');
      row.innerHTML = `<div class="title">${esc(own || partNumLabel(m))}</div><div class="meta">${esc(meta)}</div>`;
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
    const own = trackTitle(item);
    $('now-playing').textContent = w.items.length > 1
      ? `${w.title} · ${partNumLabel(item)} of ${w.items.length}${own ? ' · ' + own : ''}` : w.title;
  }

  Object.assign(root, { isAudioItem, artworkUrl, audioWorks, audioSiblings, partLabel, artists,
                        renderAudioCards, renderAudioDetail, audioMode, renderArtist, audioBackHash });
})(typeof window !== 'undefined' ? window : this);
