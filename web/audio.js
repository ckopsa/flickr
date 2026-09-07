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
//   isAudioItem(it)            medium test (the up-next label, and the panes)
//   renderAudioDetail(item)    the detail pane's author line and parts list
//   audioMode(item | null)     entering / leaving audio playback
//   partLabel(item)            "Part 3" / "Track 7 · Karma Police" / the file name
//   renderArtist(name)         the #/artist/<name> view; false when unknown
//   audioBackHash(item)        where Back goes from a track: its artist's shelf
//
// NOTHING here groups any more. The work an item belongs to — its parts or
// tracks, in order — is the server's answer, held by index.html as
// `currentWork` (GET /api/works/{key}); the shelf is a document of its own
// (GET /api/artists/{name}), and the grid's artist tile comes down inside
// GET /api/library. What was `audioWorks()` and `artists()` here was a copy
// of internal/works.Build and works.Artists, and it is gone.
//
// Loaded as a plain script before index.html's own, so nothing here runs at
// parse time beyond defining functions — the page's state (currentWork,
// currentItem, passage) is read only when a hook is called.
(function (root) {
  'use strict';

  const AUDIO_KINDS = new Set(['audiobook_part', 'track']);

  // The medium is the file's, not the path's: an unidentified .mp3 is audio
  // too. Identity kinds cover rows probed before the medium field existed.
  function isAudioItem(it) {
    return !!it && (it.media_info?.medium === 'audio' || AUDIO_KINDS.has(it.identity?.kind));
  }

  // Zero (and the omitted zero the server sends as undefined) = unnumbered.
  function partNo(it) { return it.identity?.part || 0; }

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

  function artistHash(name) { return '#/artist/' + encodeURIComponent(name); }

  // The artist's shelf (#/artist/<name>): GET /api/artists/{name} — the
  // picture beside the name, then the albums as tiles in the order the
  // document lists them (year order, the year-less last), each opening its
  // album's pane. Builds its own DOM inside index.html's empty #view-artist,
  // so the page carries one line for it. Returns false for a name the server
  // knows no shelf for — the route then falls back to the library.
  async function renderArtist(name) {
    let doc;
    try {
      doc = await api('/api/artists/' + encodeURIComponent(name));
    } catch (e) {
      return false; // a problem+json refusal: no shelf under that name
    }
    const view = $('view-artist');
    view.innerHTML = '';
    const back = document.createElement('button');
    back.className = 'back';
    back.textContent = '← Library';
    back.onclick = () => navigate('#/');
    view.appendChild(back);
    const head = document.createElement('div');
    head.id = 'artist-head';
    const art = doc.links?.artwork?.href;
    if (art) {
      const cover = document.createElement('img');
      cover.id = 'artist-cover';
      cover.alt = '';
      cover.src = art;
      cover.onerror = () => { cover.hidden = true; };
      head.appendChild(cover);
    }
    const n = doc.album_count || 0, tracks = doc.track_count || 0;
    head.insertAdjacentHTML('beforeend',
      `<div><h2 id="artist-name">${esc(doc.name)}</h2>` +
      `<div id="artist-sub">${n} album${n === 1 ? '' : 's'} · ${tracks} track${tracks === 1 ? '' : 's'}</div></div>`);
    view.appendChild(head);
    const shelf = document.createElement('div');
    shelf.id = 'artist-albums';
    for (const al of doc.albums || []) {
      shelf.appendChild(makeCard({
        artwork: al.links?.artwork?.href, title: al.title, sub: al.subtitle, tech: al.tech,
        onClick: () => navigate(itemHash(al.item_id, null)),
      }));
    }
    view.appendChild(shelf);
    showView('view-artist');
    return true;
  }

  // Back from a track's pane goes to its artist's shelf, the way an episode's
  // goes to its show; anything else answers null and Back goes to the grid.
  // Every album with an artist is on that artist's shelf, so the work saying
  // it is an album by somebody is the whole test.
  function audioBackHash(item) {
    const w = currentWork;
    if (!isAudioItem(item) || w?.kind !== 'album' || !w.author) return null;
    return artistHash(w.author);
  }

  // The pane's content: cover, title, author, where this item sits in the
  // work, and the work's members with the current one marked. Clicking a
  // member while playing moves the player there (route replaced, like an
  // episode); from the detail pane it opens that member's own page.
  function fillAudioPane(w, item) {
    const cover = $('audio-cover');
    cover.onerror = () => { cover.hidden = true; };
    cover.hidden = !w.artwork; // the work's own cover, as the document gave it
    if (w.artwork) cover.src = w.artwork;
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
    const w = currentWork;
    if (!isAudioItem(item) || !w) { pane.hidden = true; return; }
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
    const w = currentWork;
    if (!w) return;
    fillAudioPane(w, item);
    $('audio-pane').hidden = false;
    const own = trackTitle(item);
    $('now-playing').textContent = w.items.length > 1
      ? `${w.title} · ${partNumLabel(item)} of ${w.items.length}${own ? ' · ' + own : ''}` : w.title;
  }

  Object.assign(root, { isAudioItem, partLabel, renderAudioDetail, audioMode,
                        renderArtist, audioBackHash });
})(typeof window !== 'undefined' ? window : this);
