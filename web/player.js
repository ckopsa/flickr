// flickr — the device (docs/hypermedia.md §The client).
//
// Everything the BROWSER owns, and nothing else: the media element, hls.js,
// the cast session, the clock, the scrub bar and its chapter ticks and
// passage flags, the volume, the subtitle and audio-track selects, the
// trickplay previews, the tap the browser wants when it refuses autoplay,
// the keys that drive all of it, and the heartbeat that writes where the
// person is.
//
// It owns no rules. Which item plays, what may be done to it, where the
// bytes are, what to show while they play, where the passage ends, what the
// next member is — every one of those is read off the SESSION DOCUMENT the
// play action answered. The chrome around the device is drawn by
// Render.session from that same document, and re-drawn whenever it changes.
//
// THE PERSISTENT ELEMENT. `element` below — the <video> and the transport
// around it — is built once, on the first init, and lives for the life of the
// page. attach() moves it into whatever chrome the kernel has just rendered;
// the move is synchronous and within one document, which by the HTML spec's
// media element insertion steps neither reloads the resource nor pauses it.
// That is what makes a document swap safe mid-stream: the renderers never
// emit a <video> tag, so there is only ever one, and it keeps its buffer.
(function (root) {
  'use strict';

  const R = root.Render;
  const $ = id => document.getElementById(id);
  const fmtTime = R.fmtTime;

  let K = null;                  // the kernel, handed in at init
  let element = null;            // the persistent device element
  let video = null, scrub = null;

  let session = null;            // the session document of the current sitting
  let item = null;               // the item document it plays
  let hls = null, method = null, seekOffset = 0;
  let passage = null;            // the ROUTE's passage (the hash's), or null
  let baseUrl = location.origin;
  let remotePlayer = null, remoteController = null;
  let castLastState = null, castEndSuppressed = false;
  let endFired = false, passageEndFired = false, passageNaturalEnd = false;
  let selectedSubOrdinal = null, burnSubOrdinal = null, selectedAudioOrdinal = null;
  let trickplay = null, trickplayItemId = null;
  let upNextTimer = null, upNextTarget = null;
  let autoplayNext = localStorage.autoplayNext !== 'off';
  let scrubbing = false;         // a finger (or a mouse) is dragging the bar
  let tapTimer = null, lastTapAt = 0, rippleTimer = null;
  let idleTimer = null;          // the countdown to a dark room
  const DOUBLE_TAP_MS = 300;     // how long a single tap waits for its twin
  const IDLE_MS = 3000;          // how long a still pointer waits before the chrome goes

  // --- capability profiles -----------------------------------------------------
  // What the PLAYING device can take. The dropdown simulates other devices;
  // casting always sends the Chromecast's manifest whatever it says.
  function detectBrowserCaps() {
    const v = document.createElement('video');
    const codecs = ['h264'];
    if (v.canPlayType('video/mp4; codecs="hev1.1.6.L93.B0"')) codecs.push('hevc');
    if (v.canPlayType('video/mp4; codecs="av01.0.05M.08"')) codecs.push('av1');
    return {
      schema_version: 1,
      containers: ['mp4', 'webm', 'm4v', 'm4a', 'm4b', 'mp3', 'flac', 'ogg', 'opus', 'wav'],
      video_codecs: codecs,
      audio_codecs: ['aac', 'mp3', 'opus', 'flac', 'vorbis'],
      max_width: 3840, max_height: 2160, max_audio_channels: 2,
      hls_segment_formats: ['ts', 'fmp4'], hls_audio_codecs: ['aac', 'mp3'],
    };
  }
  const profiles = {
    browser: null, // filled at init, once there is a document to probe with
    chromecast1: {
      schema_version: 1, containers: ['mp4'], video_codecs: ['h264'],
      audio_codecs: ['aac', 'mp3'], max_width: 1920, max_height: 1080, max_audio_channels: 2,
      hls_segment_formats: ['ts'], hls_audio_codecs: ['aac'],
    },
    chromecastUltra: {
      schema_version: 1, containers: ['mp4', 'webm'],
      video_codecs: ['h264', 'hevc', 'vp9'],
      audio_codecs: ['aac', 'mp3', 'opus', 'flac', 'vorbis', 'ac3', 'eac3'],
      max_width: 3840, max_height: 2160,
      supports_hdr: ['hdr10', 'dolbyvision'], max_audio_channels: 6,
      // Measured, not from the spec sheet: the Ultra direct-plays an HEVC
      // file fine, and its HLS player never starts on HEVC in fMP4 segments.
      hls_segment_formats: ['ts', 'fmp4'], hls_audio_codecs: ['aac'],
      hls_video_codecs: ['h264'],
    },
    tv4k: {
      schema_version: 1, containers: ['mp4', 'mkv'], video_codecs: ['h264', 'hevc', 'av1'],
      audio_codecs: ['aac', 'ac3', 'eac3'], max_width: 3840, max_height: 2160,
      supports_hdr: ['hdr10', 'hlg'], max_audio_channels: 8,
      hls_segment_formats: ['ts', 'fmp4'], hls_audio_codecs: ['aac', 'ac3', 'eac3'],
    },
    cellular: {
      schema_version: 1, containers: ['mp4'], video_codecs: ['h264'],
      audio_codecs: ['aac'], max_width: 1280, max_height: 720,
      max_bitrate_bps: 3000000, max_audio_channels: 2,
      hls_segment_formats: ['ts'], hls_audio_codecs: ['aac'],
    },
  };
  function caps() {
    return casting() ? profiles.chromecastUltra : (profiles[$('profile').value] || profiles.browser);
  }

  // --- the persistent element --------------------------------------------------

  // The keyboard, said in words. Every one of these does exactly what one of
  // the buttons below does — the keys reach nothing the transport does not —
  // and the '?' card is this list, drawn once.
  const SHORTCUTS = [
    ['Space / K', 'Play or pause'],
    ['← / →', 'Back 10s / forward 10s'],
    ['J / L', 'Back 30s / forward 30s'],
    ['↑ / ↓', 'Volume'],
    ['M', 'Mute'],
    ['F', 'Fullscreen'],
    ['C', 'Next subtitle track'],
    ['N', 'Next episode'],
    ['Esc', 'Leave fullscreen, or back out'],
  ];

  function build() {
    element = document.createElement('div');
    element.id = 'device';
    element.innerHTML =
      // No `controls`: the native bar is a SECOND transport, and its
      // fullscreen shows the bare picture — no chapter ticks, no passage
      // flags, no trickplay. The row below is the only transport there is,
      // and it goes fullscreen with the wrapper rather than being left
      // behind. `playsinline` is what keeps a phone from overriding all of
      // that with its own full-screen player the moment play starts.
      '<video id="video" playsinline></video>' +
      // Autoplay the browser refuses for want of a gesture is not an error,
      // it is a question — answered by the one tap this button is.
      '<button id="tap-to-play" hidden>▶ Tap to play</button>' +
      // What a double tap answers with, so the finger knows it landed.
      '<div id="seek-ripple" hidden></div>' +
      // While casting, the page is the REMOTE: no dimmed picture of what the
      // television is showing, but the artwork, the names and the device,
      // over the same transport below. What it says is the session
      // document's, filled in by paintCastRemote; CSS shows it (body.casting
      // in index.html) and hides the video.
      '<div id="cast-remote">' +
        '<img id="cast-art" alt="" hidden>' +
        '<div id="cast-what">' +
          '<div id="cast-title"></div>' +
          '<div id="cast-work"></div>' +
          '<div id="cast-to">▶ Casting to <span id="cast-device"></span></div>' +
          '<button id="btn-cast-stop">Stop casting</button>' +
        '</div>' +
      '</div>' +
      // What the keys do, listed where they are used, behind the '?' below.
      '<div id="keys-help" hidden><dl>' +
        SHORTCUTS.map(([k, what]) => '<dt>' + k + '</dt><dd>' + what + '</dd>').join('') +
      '</dl></div>' +
      '<div id="transport">' +
        '<div id="scrub"><div class="rail"></div><div class="avail"></div><div class="fill"></div>' +
          '<div id="thumb" hidden><div id="thumb-img"></div><div id="thumb-time"></div></div></div>' +
        '<div id="timebar"><span id="t-now">0:00</span><span id="t-total">0:00</span></div>' +
        '<div id="controls">' +
          '<button id="btn-play" title="Play/Pause">⏵</button>' +
          '<button id="btn-back" title="Back 10s">⏪</button>' +
          '<button id="btn-fwd" title="Forward 30s">⏩</button>' +
          '<button id="btn-mute" title="Mute" aria-label="Mute">🔊</button>' +
          '<input type="range" id="vol" min="0" max="1" step="0.05" value="1" aria-label="Volume">' +
          '<select id="subs" title="Subtitles" style="display:none"></select>' +
          '<select id="audio" title="Audio track" style="display:none"></select>' +
          '<button id="btn-autoplay"></button>' +
          '<button id="btn-pip" title="Picture in picture" aria-label="Picture in picture" hidden>⧉</button>' +
          '<button id="btn-keys" title="Keyboard shortcuts" aria-label="Keyboard shortcuts">?</button>' +
          '<button id="btn-fs" title="Fullscreen" aria-label="Fullscreen">⛶</button>' +
          '<google-cast-launcher id="cast-btn"></google-cast-launcher>' +
        '</div>' +
      '</div>';
    video = element.querySelector('#video');
    scrub = element.querySelector('#scrub');
    bindDevice();
    bindNowPlaying();
  }

  // attach moves the ONE device element into the chrome the kernel just
  // rendered. See the note at the top: this move keeps the buffer.
  function attach(chrome) {
    const slot = chrome.querySelector('#device-slot');
    if (!slot) return;
    slot.replaceWith(element);
    // The way back and what is playing ride INSIDE the device, as a title bar
    // over the top of the picture: there they go fullscreen with it and fade
    // with the transport. The chrome is re-rendered from the session document,
    // so the bar this render drew replaces the one the last render did.
    const title = chrome.querySelector('#player-title');
    if (title) {
      const old = element.querySelector('#player-title');
      if (old) old.remove();
      element.prepend(title);
    }
    // The immersive layout belongs to the PICTURE: a record has none to fill.
    document.body.classList.toggle('theatre', !!item && item.medium !== 'audio');
    renderChapters();
    renderMarksOnScrub();
    if (session) paintTrace();
    $('t-total').textContent = fmtTime(duration());
    syncPlayButton();
    syncMuteButton();
    syncFullscreenButton();
    setAutoplay(autoplayNext);
    renderSubSelector();
    renderAudioSelector();
    wake();
  }

  // --- clocks ------------------------------------------------------------------

  function casting() { return !!(remotePlayer && remotePlayer.isConnected); }
  function duration() {
    return (item && item.duration_seconds) ||
      (item && item.media_info && item.media_info.duration_seconds) || (video && video.duration) || 0;
  }
  function position() {
    const t = casting() ? (remotePlayer.currentTime || 0) : (video ? video.currentTime || 0 : 0);
    return seekOffset + t;
  }
  function availableEnd() {
    if (method !== 'transcode') return duration();
    const end = casting()
      ? (remotePlayer.duration > 0 ? remotePlayer.duration : 0)
      : (video.seekable.length ? video.seekable.end(video.seekable.length - 1) : 0);
    return seekOffset + end;
  }
  function isPlaying() {
    return casting() ? remotePlayer.playerState === 'PLAYING' : !!(video && !video.paused && !video.ended);
  }

  // --- subtitles and audio tracks ----------------------------------------------
  // Both are the ITEM document's: `subtitles` carries the WebVTT address per
  // track, and a bitmap track carries none because it cannot be converted —
  // so a select built from what has an href is right by construction.
  function subs() { return (item && item.subtitles) || []; }
  function supportedSubs() { return subs().filter(s => s.supported && s.href); }
  function burnableSubs() { return subs().filter(s => !s.supported && !s.external); }
  function subLabel(s) {
    return s.title && s.language ? `${s.title} (${s.language})` : (s.title || s.language || 'Track ' + s.ordinal);
  }
  function burnLabel(s) {
    const codec = { hdmv_pgs_subtitle: 'PGS', dvd_subtitle: 'DVD', dvb_subtitle: 'DVB' }[s.codec] ||
      (s.codec || '?').toUpperCase();
    return `${s.language || s.title || 'Track ' + s.ordinal} · ${codec} (burned-in)`;
  }
  function renderSubSelector() {
    const sel = $('subs');
    if (!sel) return;
    const ok = supportedSubs(), burns = burnableSubs();
    if (!ok.length && !burns.length) {
      sel.style.display = 'none'; selectedSubOrdinal = null; burnSubOrdinal = null; return;
    }
    sel.style.display = '';
    sel.innerHTML = '<option value="">Subtitles: Off</option>' +
      ok.map(s => `<option value="${s.ordinal}">${R.esc(subLabel(s))}${s.external ? ' [ext]' : ''}</option>`).join('') +
      burns.map(s => `<option value="burn:${s.ordinal}">${R.esc(burnLabel(s))}</option>`).join('');
    if (selectedSubOrdinal != null && !ok.some(s => s.ordinal === selectedSubOrdinal)) selectedSubOrdinal = null;
    if (burnSubOrdinal != null && !burns.some(s => s.ordinal === burnSubOrdinal)) burnSubOrdinal = null;
    sel.value = burnSubOrdinal != null ? 'burn:' + burnSubOrdinal
      : selectedSubOrdinal == null ? '' : String(selectedSubOrdinal);
  }
  function attachLocalTracks() {
    video.querySelectorAll('track').forEach(t => t.remove());
    for (const s of supportedSubs()) {
      const tr = document.createElement('track');
      tr.kind = 'subtitles';
      tr.src = s.href;              // the document's address, not one built here
      tr.srclang = s.language || '';
      tr.label = subLabel(s);
      tr.dataset.ordinal = s.ordinal;
      video.appendChild(tr);
    }
    applyLocalSubSelection();
  }
  function applyLocalSubSelection() {
    for (const tr of video.querySelectorAll('track')) {
      tr.track.mode = (selectedSubOrdinal != null && Number(tr.dataset.ordinal) === selectedSubOrdinal)
        ? 'showing' : 'disabled';
    }
  }
  function audioTracks() {
    return ((item && item.media_info && item.media_info.audio_tracks) || []).filter(t => t && t.ordinal != null);
  }
  function audioLabel(t) {
    const ch = t.channels === 6 ? '5.1' : t.channels === 8 ? '7.1' : t.channels ? t.channels + 'ch' : '';
    return [t.language || t.title || 'Track ' + t.ordinal,
            t.title && t.language ? t.title : null, t.codec, ch].filter(Boolean).join(' · ');
  }
  function renderAudioSelector() {
    const sel = $('audio');
    if (!sel) return;
    const tracks = audioTracks();
    if (tracks.length < 2) { sel.style.display = 'none'; selectedAudioOrdinal = null; return; }
    sel.style.display = '';
    sel.innerHTML = tracks.map(t => `<option value="${t.ordinal}">${R.esc(audioLabel(t))}</option>`).join('');
    if (selectedAudioOrdinal == null || !tracks.some(t => t.ordinal === selectedAudioOrdinal)) {
      selectedAudioOrdinal = (tracks.find(t => t.default) || tracks[0]).ordinal;
    }
    sel.value = String(selectedAudioOrdinal);
  }

  // --- the chrome's own paint --------------------------------------------------

  function paintTrace() {
    const st = $('status'), tr = $('trace');
    if (!st || !session || !session.decision) return;
    const t = R.trace(session.decision, session.video_encoder, casting() ? 'cast' : '');
    st.innerHTML = t.status;
    if (tr) tr.innerHTML = t.trace;
  }

  // The remote's face, and every word of it the SESSION DOCUMENT's: the
  // artwork it carries, its own name for what plays and the work it belongs
  // to. Painted on adopt, so a document swap redraws it.
  function paintCastRemote() {
    const art = $('cast-art');
    if (!art) return;
    const href = R.artworkOf(session);
    art.hidden = !href;
    if (href) art.src = href;
    const links = (session && session.links) || {};
    $('cast-title').textContent = (session && session.title) || '';
    $('cast-work').textContent = (links.work && links.work.title) || '';
  }

  function renderChapters() {
    const pane = $('chapters');
    if (!pane || !scrub) return;
    const chs = (item && item.media_info && item.media_info.chapters) || [];
    pane.innerHTML = chs.map(ch =>
      `<button data-seek="${ch.start_seconds}">${R.esc((ch.title || '') + ' · ' + fmtTime(ch.start_seconds))}</button>`).join('');
    scrub.querySelectorAll('.tick').forEach(t => t.remove());
    const dur = duration();
    if (!(dur > 0)) return;
    for (const ch of chs) {
      const tick = document.createElement('div');
      tick.className = 'tick';
      tick.style.left = (ch.start_seconds / dur * 100) + '%';
      scrub.appendChild(tick);
    }
  }

  // Two bound marks on the scrubber: the start and, on the item the `end`
  // applies to, the end. A passage being MADE (the session's marks) takes the
  // same flags, each shown only on the item it was set in.
  function renderMarksOnScrub() {
    if (!scrub) return;
    scrub.querySelectorAll('.bound').forEach(b => b.remove());
    const dur = duration();
    if (!(dur > 0)) return;
    const id = item && item.id;
    const m = session && session.marks;
    const b = (m && (m.in || m.out))
      ? { t: m.in && m.in.item_id === id ? m.in.seconds : null,
          end: m.out && m.out.item_id === id ? m.out.seconds : null }
      : passage ? { t: passage.t, end: passageEndNow() } : null;
    if (!b) return;
    for (const [cls, t, label] of [['start', b.t, 'Passage starts at '], ['end', b.end, 'Passage ends at ']]) {
      if (t == null) continue;
      const el = document.createElement('div');
      el.className = 'bound ' + cls;
      el.style.left = Math.min(100, t / dur * 100) + '%';
      el.title = label + fmtTime(t);
      scrub.appendChild(el);
    }
  }

  // --- starting a play ---------------------------------------------------------

  // play is the whole of it: the item document's `play` action, the answer
  // adopted as the session, and the bytes handed to the media element or the
  // cast device. `seek` undefined means "where this profile left off", which
  // the ITEM DOCUMENT says (`resume`) — the browser no longer asks a progress
  // route for it.
  async function play(itemDoc, opts) {
    const o = opts || {};
    if (!itemDoc) return;
    if (itemDoc.medium === 'text') return; // a book is read, not played
    const act = R.actionOf(itemDoc, 'play');
    if (!act) {
      K.note(R.unavailableReason(itemDoc, 'play') || 'This cannot be played.');
      return;
    }
    hideUpNext();
    item = itemDoc;
    document.body.classList.toggle('audio-mode', itemDoc.medium === 'audio');
    passage = o.passage || null;
    seekOffset = 0;
    resetPassageEnd();
    let seek = o.seek;
    if (seek === undefined) seek = passage ? 0 : resumeSeconds(itemDoc);
    await startPlayback(act, seek || 0);
  }

  // Where this profile left off, straight off the item document.
  function resumeSeconds(itemDoc) {
    const r = itemDoc && itemDoc.resume;
    return r && typeof r.position_seconds === 'number' ? r.position_seconds : 0;
  }

  // A chapter button, from the detail pane or from under the player.
  function playFrom(itemDoc, seconds) {
    if (item && itemDoc && item.id === itemDoc.id && method !== null) { seekTo(seconds); return; }
    play(itemDoc, { seek: seconds });
  }

  async function startPlayback(act, seekSeconds) {
    await stopSession();
    endFired = false;
    let resp;
    try {
      resp = await K.api(act.href, {
        method: act.method || 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          capabilities: caps(), client_id: K.profile(), seek_seconds: seekSeconds,
          ...(passage ? { passage: { t: passage.t, end: passage.end, until: passage.until,
                                     from: passage.from, to: passage.to } } : {}),
          ...(selectedAudioOrdinal != null ? { audio_track: selectedAudioOrdinal } : {}),
          ...(burnSubOrdinal != null ? { subtitle_burn: burnSubOrdinal } : {}),
        }),
      });
    } catch (e) {
      // A refusal is an answer: the sentence, and the remedy that would make
      // the action available, both come off the problem document.
      K.note([e.detail, e.remedyText].filter(Boolean).join(' — '));
      return;
    }
    adopt(resp);
    method = resp.decision ? resp.decision.method : null;
    if (method === 'deny') return;
    const isHls = method === 'transcode';
    seekOffset = isHls ? seekSeconds : 0;
    loadTrickplay();

    if (casting()) {
      try { await castLoad(); }
      catch (e) { $('status').textContent = 'Cast error: ' + (e.description || e.message || e); }
      return;
    }
    if (!isHls) {
      video.src = resp.url;
      if (seekSeconds) video.currentTime = seekSeconds;
      tryPlay();
    } else if (root.Hls && root.Hls.isSupported()) {
      hls = new root.Hls();
      hls.loadSource(resp.url);
      hls.attachMedia(video);
      tryPlay();
    } else {
      video.src = resp.url;
      tryPlay();
    }
    attachLocalTracks();
  }

  // adopt is the one place the session document lands: the chrome is drawn
  // from it, the device element moved into that chrome, and every reading
  // below — the bound, the marks, the next member — comes off it.
  function adopt(s) {
    session = s;
    K.renderSession(s);
    paintCastRemote();
    updateNowPlaying();
  }

  // holdSession lets the reader's sitting share this one's exit: a book's
  // session is stopped by the same DELETE a stream's is.
  function holdSession(s) { session = s; }

  function tryPlay() {
    const tap = $('tap-to-play');
    if (tap) tap.hidden = true;
    video.play().then(() => { if ($('tap-to-play')) $('tap-to-play').hidden = true; })
      .catch(e => { if (e && e.name === 'NotAllowedError' && $('tap-to-play')) $('tap-to-play').hidden = false; });
  }

  async function stopSession() {
    const act = session && R.actionOf(session, 'stop');
    if (act) fetch(act.href, { method: act.method || 'DELETE' }).catch(() => {});
    session = null;
    if (hls) { hls.destroy(); hls = null; }
  }

  async function close() {
    hideUpNext();
    setPassage(null);
    if (casting()) { castEndSuppressed = true; if (remoteController) remoteController.stop(); }
    else if (video) { video.pause(); video.removeAttribute('src'); video.load(); }
    await stopSession();
    clearNowPlaying();
    item = null;
    method = null;
    document.body.classList.remove('audio-mode');
    document.body.classList.remove('theatre');
    wake(); // nothing plays: the room comes back up
    const chrome = $('player-chrome');
    if (chrome) chrome.hidden = true;
  }

  // --- the invoked controls ----------------------------------------------------
  //
  // Every button the renderers draw comes back here with the action's own
  // href and method. What differs between them is only what is done with the
  // answer, which is why this is a switch and not six code paths.
  async function invoke(req, ctx) {
    const name = req.name;
    const it = (ctx && ctx.item) || item;
    switch (name) {
      case 'play': case 'resume':
        return play(it, {});
      case 'read':
        return; // the kernel owns the reader's sitting
      case 'stop':
        await close();
        K.closeStage();
        return;
      case 'next':
        return advance();
      case 'mark_in': case 'mark_out':
        return mark(req);
      case 'link':
        return K.mint(req); // the minted link is shown the same way a work's is
      case 'keep_watching': case 'keep_reading':
        return keepWatching(req);
      default: {
        // Anything else the document offers that needs no body — probe again,
        // look it up on TMDB — is the same three lines, and the view is
        // re-read afterwards because the answer changed what it says. An
        // action WITH a body is a form, and the kernel submits those.
        try { await K.api(req.href, { method: req.method }); }
        catch (e) { K.note(e.detail || String(e.message || e)); }
        K.forget();
        K.applyRoute();
      }
    }
  }

  // --- passages ----------------------------------------------------------------

  function setPassage(p) {
    passage = p || null;
    resetPassageEnd();
    renderMarksOnScrub();
  }
  function resetPassageEnd() {
    passageEndFired = false;
    passageNaturalEnd = false;
    const panel = $('passage-end');
    if (panel) panel.hidden = true;
  }
  // The bound this item is to stop at. The SERVER worked it out — a run's
  // `end` belongs to its last member — and sessionEndBound (cast.js) is the
  // one reading of it, shared with the receiver. The clock-side predicate
  // stands in for a passage held before a session answered.
  function passageEndNow() {
    if (session && session.passage) return root.sessionEndBound(session);
    return root.passageEndAt(passage, item && item.id);
  }
  function passageReadout(pos) {
    const endAt = passageEndNow();
    if (endAt == null) return `${fmtTime(pos)} · in passage`;
    const start = passage && passage.t != null ? passage.t : 0;
    return `${fmtTime(pos - start)} / ${fmtTime(endAt - start)} in passage`;
  }
  function checkPassageEnd() {
    if (!passage || !item || passageEndFired) return;
    if (!isPlaying() && !(casting() && remotePlayer.playerState === 'PAUSED')) return;
    const endAt = passageEndNow();
    if (!root.passageEnded(position(), endAt)) return;
    if (casting()) {
      // Sender-side stop, from the document's own bound: the DEFAULT receiver
      // knows nothing about passages, and a custom one pauses at the same
      // bound for itself — whichever gets there first, the other finds it
      // already paused.
      if (remotePlayer.playerState === 'PLAYING') remoteController.playOrPause();
    } else {
      video.pause();
    }
    onPassageEnd(`Paused at ${fmtTime(endAt)}`);
  }
  function onPassageEnd(sub) {
    passageEndFired = true;
    hideUpNext();
    const panel = $('passage-end');
    if (!panel) return;
    $('passage-end-sub').textContent = sub;
    panel.hidden = false;
  }
  // "Keep watching": the passage is the SESSION's, so leaving it is a write.
  // The server clears it and answers the session as it now is — progress
  // accepted from here on — and the route drops the passage too.
  async function keepWatching(req) {
    const wasNaturalEnd = passageNaturalEnd;
    const id = item && item.id;
    if (req && req.href) {
      try { adopt(await K.api(req.href, { method: req.method })); } catch (e) { /* already over */ }
    }
    setPassage(null);
    const route = K.currentRoute();
    if (id != null && route && route.passage) K.replaceHash(K.itemHash(id, null));
    if (wasNaturalEnd) { endFired = false; onPlaybackEnded(); return; }
    if (!isPlaying()) { casting() ? remoteController.playOrPause() : video.play().catch(() => {}); }
  }

  // --- marks and the minted link -----------------------------------------------

  async function mark(req) {
    try {
      adopt(await K.api(req.href, {
        method: req.method, headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(req.body || { seconds: position() }),
      }));
    } catch (e) { /* the session may already be over */ }
  }

  // --- the run: up next and the advance ----------------------------------------

  function nextHref() { return R.linkHref(session, 'next'); }
  function nextId() { const h = nextHref(); return h ? Number(R.idIn(h)) : null; }

  function hideUpNext() {
    if (upNextTimer) { clearInterval(upNextTimer); upNextTimer = null; }
    upNextTarget = null;
    const el = $('upnext');
    if (el) el.hidden = true;
  }
  function showUpNext() {
    hideUpNext();
    const el = $('upnext');
    const id = nextId();
    if (!el || id == null) return;
    upNextTarget = id;
    el.hidden = false;
    const playBtn = $('upnext-play');
    if (playBtn) playBtn.hidden = autoplayNext;
    if (!autoplayNext) { $('upnext-count').textContent = ''; return; }
    let left = 10;
    $('upnext-count').textContent = `Playing in ${left}s`;
    upNextTimer = setInterval(() => {
      left--;
      if (left <= 0) { advance(); return; }
      $('upnext-count').textContent = `Playing in ${left}s`;
    }, 1000);
  }
  // The run's next member. The route is REPLACED, not pushed: binging a
  // season must not build a mile of history. `until` carries on; t/end were
  // the first item's.
  function advance() {
    const id = upNextTarget != null ? upNextTarget : nextId();
    hideUpNext();
    if (id == null) return;
    goTo(id, 'zero');
  }
  function goTo(id, mode) {
    // A different file: the subtitle, burn and audio selections were this
    // one's ordinals and mean nothing there.
    selectedSubOrdinal = burnSubOrdinal = selectedAudioOrdinal = null;
    K.setPendingPlay({ id, mode });
    K.replaceHash(K.itemHash(id, root.passageForNext(passage)));
  }

  // --- natural ends and the heartbeat ------------------------------------------

  function onPlaybackEnded() {
    if (!item || endFired) return;
    endFired = true;
    if (passage) {
      // Inside a passage the item's own end is not a finish to record. In a
      // run it is the cue for the next member — until the `until` item has
      // played; then the passage is over.
      if (root.runContinues(passage, item.id) && nextId() != null) { showUpNext(); return; }
      passageNaturalEnd = true;
      onPassageEnd(`${item.title || ''} has ended`);
      return;
    }
    if (nextId() == null) return;
    const dur = duration();
    if (dur > 0) saveProgress(dur);
    showUpNext();
  }

  // The player's road to a progress write, and it goes through the SESSION:
  // actions.progress is the address, and the answer to a write during a
  // passage is 409 — a scene replayed for a talk is not where the person is
  // in the film. That refusal is an answer: no retry, nothing said on screen.
  function saveProgress(positionSeconds) {
    const act = session && session.item_id === (item && item.id) ? R.actionOf(session, 'progress') : null;
    if (!act) return;
    fetch(act.href, {
      method: act.method || 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ position_seconds: positionSeconds }),
    }).catch(() => {});
  }

  // --- seeking and the transport ------------------------------------------------

  function seekTo(t) {
    if (!item || method === null) return;
    t = Math.max(0, Math.min(t, duration()));
    const rel = t - seekOffset;
    const canLocal = method === 'direct_play' || (rel >= 0 && rel <= (availableEnd() - seekOffset) - 0.5);
    if (canLocal) {
      if (casting()) { remotePlayer.currentTime = rel; remoteController.seek(); }
      else video.currentTime = rel;
      return;
    }
    $('status').textContent = `Seeking to ${fmtTime(t)} — restarting transcode at that point…`;
    const act = R.actionOf(item, 'play');
    if (act) startPlayback(act, t);
  }
  // Burning a subtitle or switching the audio track changes the stream
  // itself, so the seek-restart machinery is reused.
  function restartFromCurrent() {
    if (!item || method === null) return;
    const t = position();
    $('status').textContent = `Restarting stream at ${fmtTime(t)}…`;
    const act = R.actionOf(item, 'play');
    if (act) startPlayback(act, t);
  }

  function syncPlayButton() {
    const b = $('btn-play');
    if (b) b.textContent = isPlaying() ? '⏸' : '⏵';
  }

  // The one play/pause there is: the button, the picture and the keyboard all
  // come here, and casting is the same choice made on the other device.
  function togglePlay() {
    if (casting()) { if (remoteController) remoteController.playOrPause(); return; }
    if (!video) return;
    video.paused ? video.play().catch(() => {}) : video.pause();
  }

  // Mute sits beside the volume and follows it: the cast device's when one is
  // connected, the media element's otherwise.
  function isMuted() {
    return casting() ? !!(remotePlayer && remotePlayer.isMuted) : !!(video && video.muted);
  }
  function toggleMute() {
    if (casting()) { if (remoteController) remoteController.muteOrUnmute(); }
    else if (video) video.muted = !video.muted;
    syncMuteButton();
  }
  function syncMuteButton() {
    const b = $('btn-mute');
    if (!b) return;
    const off = isMuted();
    b.textContent = off ? '🔇' : '🔊';
    b.title = off ? 'Unmute' : 'Mute';
    b.setAttribute('aria-label', b.title);
  }

  // Fullscreen is the DEVICE WRAPPER's, never the <video>'s. Taking the
  // wrapper full keeps the scrub bar on screen — its chapter ticks, its
  // passage flags, its trickplay previews — which the native fullscreen
  // threw away. An iPhone allows no element but the video to go full, and
  // lends its own player instead; that is the fallback, not the default.
  function toggleFullscreen() {
    if (document.fullscreenElement) { document.exitFullscreen(); return; }
    if (element && element.requestFullscreen) element.requestFullscreen().catch(() => {});
    else if (video && video.webkitEnterFullscreen) video.webkitEnterFullscreen();
  }
  function syncFullscreenButton() {
    const b = $('btn-fs');
    if (!b) return;
    b.title = document.fullscreenElement === element ? 'Leave fullscreen' : 'Fullscreen';
    b.setAttribute('aria-label', b.title);
  }

  // --- the keyboard ------------------------------------------------------------
  //
  // The keys are a transport, not a second set of rules: each one calls the
  // function its button calls. A key typed into a field is the field's — that
  // is the whole of the guard — and Space on a focused button is that button's,
  // which already answers it.
  function typingIn(t) {
    return !!(t && (t.isContentEditable || /^(INPUT|SELECT|TEXTAREA)$/.test(t.tagName)));
  }
  function onKey(e) {
    if (!item || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    const t = e.target;
    if (typingIn(t)) return;
    const space = e.key === ' ' || e.key === 'Spacebar';
    if (space && t && t.closest && t.closest('button')) return;
    const k = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    if (space || k === 'k') togglePlay();
    else if (k === 'ArrowLeft') { seekTo(position() - 10); seekRipple('left', '−10s'); }
    else if (k === 'ArrowRight') { seekTo(position() + 10); seekRipple('right', '+10s'); }
    else if (k === 'j') { seekTo(position() - 30); seekRipple('left', '−30s'); }
    else if (k === 'l') { seekTo(position() + 30); seekRipple('right', '+30s'); }
    else if (k === 'ArrowUp') nudgeVolume(0.1);
    else if (k === 'ArrowDown') nudgeVolume(-0.1);
    else if (k === 'm') toggleMute();
    else if (k === 'f') toggleFullscreen();
    else if (k === 'c') cycleSubs();
    else if (k === 'n') advance();
    else if (k === '?') toggleKeys();
    else if (k === 'Escape') escapeOut();
    else return;
    e.preventDefault(); // Space would scroll the page, the arrows too
    wake();
  }

  // Escape is the way out, in the order the room offers one: the help card,
  // then fullscreen, then the sitting itself — which is what Back does.
  function escapeOut() {
    if ($('keys-help') && !$('keys-help').hidden) { showKeys(false); return; }
    if (document.fullscreenElement) { document.exitFullscreen(); return; }
    K.closeStage();
    K.applyRoute();
  }

  // The volume the keys set is the one the slider sets, on whichever device is
  // playing; the slider follows so the two never disagree.
  function nudgeVolume(delta) {
    const now = casting() ? (remotePlayer.volumeLevel || 0) : (video ? video.volume : 0);
    const v = Math.max(0, Math.min(1, now + delta));
    if (casting()) { remotePlayer.volumeLevel = v; if (remoteController) remoteController.setVolumeLevel(); }
    else if (video) video.volume = v;
    if ($('vol')) $('vol').value = String(v);
  }

  // 'c' walks the subtitle select the way clicking through it would: Off, then
  // each track the ITEM document published, then round again.
  function cycleSubs() {
    const sel = $('subs');
    if (!sel || sel.style.display === 'none' || !sel.options.length) return;
    sel.selectedIndex = (sel.selectedIndex + 1) % sel.options.length;
    onSubChange(sel.value);
  }

  function showKeys(on) {
    const el = $('keys-help');
    if (el) el.hidden = !on;
  }
  function toggleKeys() { showKeys(!!($('keys-help') && $('keys-help').hidden)); }

  // --- the room darkens ---------------------------------------------------------
  //
  // While the picture runs and nothing moves, the title bar, the transport and
  // the cursor fade out; any movement, tap or key brings them back. Paused,
  // they stay: a stopped picture with no controls is a dead page.
  function wake() {
    if (!element) return;
    element.classList.remove('idle');
    clearTimeout(idleTimer);
    idleTimer = null;
    if (!isPlaying()) return;
    idleTimer = setTimeout(() => { if (isPlaying()) element.classList.add('idle'); }, IDLE_MS);
  }

  // What a double tap answers with, so the finger knows it landed: the amount
  // seeked, on the side it was tapped, gone again in half a second. Hiding it
  // first restarts the fade when a second tap comes straight after.
  function seekRipple(side, label) {
    const el = $('seek-ripple');
    if (!el) return;
    el.hidden = true;
    el.className = side;
    void el.offsetWidth;
    el.textContent = label;
    el.hidden = false;
    clearTimeout(rippleTimer);
    rippleTimer = setTimeout(() => { el.hidden = true; }, 500);
  }

  // --- the scrub bar under a pointer -------------------------------------------

  function scrubSeconds(clientX) {
    const rect = scrub.getBoundingClientRect();
    return Math.min(1, Math.max(0, (clientX - rect.left) / (rect.width || 1))) * duration();
  }
  // While a drag is on, the fill and the readout follow the finger and the
  // tick leaves them alone; the seek itself waits for the release, because a
  // seek past what has been transcoded restarts the stream.
  function paintScrubAt(t) {
    const dur = duration();
    if (!(dur > 0)) return;
    const fill = scrub.querySelector('.fill');
    if (fill) fill.style.width = Math.min(100, t / dur * 100) + '%';
    if ($('t-now')) $('t-now').textContent = fmtTime(t);
  }
  function hideThumb() { if ($('thumb')) $('thumb').hidden = true; }

  // Autoplay-next toggle: global, persisted, default ON.
  function setAutoplay(on) {
    autoplayNext = on;
    localStorage.autoplayNext = on ? 'on' : 'off';
    const b = $('btn-autoplay');
    if (!b) return;
    b.textContent = 'Autoplay: ' + (on ? 'on' : 'off');
    b.title = on ? 'Next episode starts automatically (10s countdown)'
                 : 'Playback stops at the end — you choose when to play the next episode';
    if (upNextTarget != null && $('upnext') && !$('upnext').hidden) showUpNext();
  }

  // --- trickplay ---------------------------------------------------------------
  // The index is a link on the item document, and the sheets are addresses IN
  // the index (sheet_hrefs): the browser builds neither.
  async function loadTrickplay() {
    const href = R.linkHref(item, 'trickplay');
    if (!href || trickplayItemId === item.id) return;
    trickplay = null;
    trickplayItemId = item.id;
    if ($('thumb')) $('thumb').hidden = true;
    try {
      const idx = await K.api(href);
      if (trickplayItemId === item.id) trickplay = idx;
    } catch (e) { /* no sheets — the feature stays hidden */ }
  }

  // --- the device's own listeners ----------------------------------------------

  function bindDevice() {
    video.addEventListener('ended', onPlaybackEnded);
    video.addEventListener('timeupdate', checkPassageEnd);
    // Pressing play — the button, the picture, a key — while paused at the
    // end bound is the same choice as "Keep watching".
    video.addEventListener('play', () => {
      if ($('tap-to-play')) $('tap-to-play').hidden = true;
      if (!passageEndFired || passageNaturalEnd) return;
      if (root.passageEnded(position(), passageEndNow())) {
        const keep = R.actionOf(session, 'keep_watching');
        keepWatching(keep ? { href: keep.href, method: keep.method } : null);
      }
    });

    element.addEventListener('click', e => {
      const t = e.target;
      if (t.id === 'tap-to-play') { t.hidden = true; video.play().catch(() => {}); return; }
      if (t.id === 'btn-play') { togglePlay(); return; }
      if (t.id === 'btn-back') { seekTo(position() - 10); return; }
      if (t.id === 'btn-fwd') { seekTo(position() + 30); return; }
      if (t.id === 'btn-mute') { toggleMute(); return; }
      if (t.id === 'btn-fs') { toggleFullscreen(); return; }
      if (t.id === 'btn-keys') { toggleKeys(); return; }
      if (t.id === 'btn-autoplay') { setAutoplay(!autoplayNext); return; }
      if (t.id === 'btn-cast-stop') { stopCasting(); return; }
    });

    // The chrome comes back for any sign of life over the picture, and the
    // keys are the device's wherever the focus is (the guard is in onKey).
    element.addEventListener('pointermove', wake);
    element.addEventListener('pointerdown', wake);
    document.addEventListener('keydown', onKey);
    video.addEventListener('play', wake);
    video.addEventListener('pause', wake);

    // The picture is a control too, now that no native bar is drawn over it.
    // A mouse has nothing to disambiguate, so its click acts at once; a
    // finger's single tap waits out the double-tap window, and a double tap
    // on the left or right third seeks instead of pausing.
    video.addEventListener('pointerup', e => {
      if (e.pointerType !== 'touch') { togglePlay(); return; }
      const now = Date.now();
      if (tapTimer && now - lastTapAt < DOUBLE_TAP_MS) {
        clearTimeout(tapTimer);
        tapTimer = null;
        const rect = video.getBoundingClientRect();
        const x = (e.clientX - rect.left) / (rect.width || 1);
        if (x < 1 / 3) { seekTo(position() - 10); seekRipple('left', '−10s'); }
        else if (x > 2 / 3) { seekTo(position() + 30); seekRipple('right', '+30s'); }
        else togglePlay();
        return;
      }
      lastTapAt = now;
      tapTimer = setTimeout(() => { tapTimer = null; togglePlay(); }, DOUBLE_TAP_MS);
    });
    video.addEventListener('volumechange', syncMuteButton);
    document.addEventListener('fullscreenchange', syncFullscreenButton);

    element.addEventListener('input', e => {
      if (e.target.id !== 'vol') return;
      const v = parseFloat(e.target.value);
      if (casting()) { remotePlayer.volumeLevel = v; remoteController.setVolumeLevel(); }
      else video.volume = v;
    });

    element.addEventListener('change', e => {
      if (e.target.id === 'subs') return onSubChange(e.target.value);
      if (e.target.id === 'audio') return onAudioChange(e.target.value);
    });

    // Pointers, not clicks: the same three listeners serve a mouse and a
    // finger, so the bar can be dragged on a phone. A hover still previews;
    // a finger has no hover, so its preview comes with the drag.
    scrub.addEventListener('pointerdown', e => {
      scrubbing = true;
      try { scrub.setPointerCapture(e.pointerId); } catch (err) { /* no capture, no matter */ }
      paintScrubAt(scrubSeconds(e.clientX));
      showThumb(e.clientX);
      e.preventDefault();
    });
    scrub.addEventListener('pointermove', e => {
      if (scrubbing) { paintScrubAt(scrubSeconds(e.clientX)); showThumb(e.clientX); return; }
      if (e.pointerType !== 'touch') showThumb(e.clientX);
    });
    scrub.addEventListener('pointerup', e => {
      if (!scrubbing) return;
      scrubbing = false;
      seekTo(scrubSeconds(e.clientX));
      if (e.pointerType === 'touch') hideThumb();
    });
    scrub.addEventListener('pointercancel', () => { scrubbing = false; hideThumb(); });
    scrub.addEventListener('pointerleave', () => { if (!scrubbing) hideThumb(); });

    setInterval(tick, 250);
    setInterval(() => { if (item && isPlaying()) saveProgress(position()); }, 5000);
  }

  function onSubChange(v) {
    if (v.startsWith('burn:')) {
      selectedSubOrdinal = null;
      applyLocalSubSelection();
      const ord = Number(v.slice(5));
      if (ord !== burnSubOrdinal) { burnSubOrdinal = ord; restartFromCurrent(); }
      return;
    }
    selectedSubOrdinal = v === '' ? null : Number(v);
    if (burnSubOrdinal != null) { burnSubOrdinal = null; restartFromCurrent(); return; }
    if (casting()) {
      const ms = root.cast.framework.CastContext.getInstance().getCurrentSession();
      const media = ms && ms.getMediaSession();
      if (media) {
        const ids = selectedSubOrdinal == null ? [] : [selectedSubOrdinal + 1];
        media.editTracksInfo(new root.chrome.cast.media.EditTracksInfoRequest(ids),
          () => {}, e => console.warn('editTracksInfo failed', e));
      }
    } else {
      applyLocalSubSelection();
    }
  }
  function onAudioChange(v) {
    const ord = v === '' ? null : Number(v);
    if (ord === selectedAudioOrdinal) return;
    selectedAudioOrdinal = ord;
    restartFromCurrent();
  }

  function showThumb(clientX) {
    const dur = duration();
    if (!trickplay || !item || item.id !== trickplayItemId || dur <= 0) return;
    const sheets = trickplay.sheet_hrefs || [];
    if (!sheets.length) return;
    const rect = scrub.getBoundingClientRect();
    const frac = Math.min(1, Math.max(0, (clientX - rect.left) / (rect.width || 1)));
    const t = frac * dur;
    const per = trickplay.cols * trickplay.rows;
    const frame = Math.min(Math.floor(t / trickplay.interval_seconds), sheets.length * per - 1);
    const sheet = Math.floor(frame / per), tile = frame % per;
    const col = tile % trickplay.cols, row = Math.floor(tile / trickplay.cols);
    const tw = trickplay.tile_width, th = trickplay.tile_height || 180;
    const img = $('thumb-img');
    img.style.width = tw + 'px';
    img.style.height = th + 'px';
    img.style.backgroundImage = `url(${sheets[sheet]})`;
    img.style.backgroundPosition = `-${col * tw}px -${row * th}px`;
    $('thumb-time').textContent = fmtTime(t);
    const el = $('thumb');
    el.style.left = Math.min(Math.max(clientX - rect.left, tw / 2), Math.max(rect.width - tw / 2, tw / 2)) + 'px';
    el.hidden = false;
  }

  // The 250ms tick: the fill, the readout, the passage bound (cast has no
  // timeupdate on this side, so the tick is its clock), and the two
  // end-of-stream conditions a media element does not always announce.
  function tick() {
    if (!element || !element.isConnected) return;
    const dur = duration();
    if (dur > 0) {
      const fill = scrub.querySelector('.fill'), avail = scrub.querySelector('.avail');
      if (avail) avail.style.width = Math.min(100, availableEnd() / dur * 100) + '%';
      if (!scrubbing) {
        if (fill) fill.style.width = Math.min(100, position() / dur * 100) + '%';
        if ($('t-now')) $('t-now').textContent = passage ? passageReadout(position()) : fmtTime(position());
      }
    }
    syncPlayButton();
    updatePositionState();
    // A stop the media element never announced — a cast device's pause, a
    // stream that ran out — lights the room back up all the same.
    if (element.classList.contains('idle') && !isPlaying()) wake();
    checkPassageEnd();
    if (passageEndFired && !passageNaturalEnd && isPlaying()) {
      const endAt = passageEndNow();
      if (endAt != null && position() < endAt - 1) resetPassageEnd();
    }
    // Transcode tail: the final HLS fragment can end a hair short of the
    // media element's duration, so 'ended' never fires — catch the stall at
    // the very end of the stream, but only within the ITEM's last seconds.
    if (item && !casting() && method === 'transcode' && !endFired &&
        !video.paused && !video.ended && isFinite(video.duration) && video.duration > 0 &&
        video.duration - video.currentTime < 0.5 && dur > 0 && position() >= dur - 10) {
      onPlaybackEnded();
    }
    if (endFired && isPlaying() && dur > 0 && position() < dur - 10) endFired = false;
  }

  // --- the lock screen and the small window ------------------------------------
  //
  // A lock screen, a headphone button, a car stereo and a hardware key all
  // speak one API, and none of them can see the transport above. What they
  // are told is read off the SAME documents the transport draws from — the
  // session's title, its work, its artwork link, the item's author — so the
  // phone says what the page says, and nothing here composes an address or a
  // rule of its own. Every call is guarded: a browser without the API is a
  // browser with no lock-screen controls, which is where it started.

  function mediaSession() {
    return (root.navigator && root.navigator.mediaSession) || null;
  }
  function linkTitle(doc, rel) {
    const l = doc && doc.links && doc.links[rel];
    return (l && l.title) || '';
  }
  // What is playing, in the three lines an OS shows it in. The author is the
  // audiobook's author and the record's artist; a film or an episode has
  // none, and then the work's own title is the name under the title.
  function nowPlaying() {
    const work = linkTitle(session, 'work') || linkTitle(item, 'work');
    const author = (item && item.identity && item.identity.author) || '';
    const art = R.linkHref(session, 'artwork');
    return {
      title: (session && session.title) || (item && item.title) || '',
      artist: author || work,
      album: work,
      // The same absolute the cast device is given: a lock screen fetches the
      // picture itself, and a relative href is not enough for one.
      artwork: art ? [{ src: root.castAbsolute(baseUrl, art) }] : [],
    };
  }
  function updateNowPlaying() {
    const ms = mediaSession();
    if (!ms) return;
    if (root.MediaMetadata) {
      try { ms.metadata = new root.MediaMetadata(nowPlaying()); } catch (e) { /* no metadata, then */ }
    }
    ms.playbackState = isPlaying() ? 'playing' : 'paused';
    syncPipButton();
  }
  function clearNowPlaying() {
    const ms = mediaSession();
    if (!ms) return;
    ms.metadata = null;
    ms.playbackState = 'none';
    if (document.pictureInPictureElement) document.exitPictureInPicture().catch(() => {});
  }
  // Where the person is, in the ITEM's clock — the one the transport shows —
  // so the scrubber on a lock screen agrees with the one on the page. A
  // position past the duration is refused by the API rather than clamped,
  // which is why it is clamped here.
  function updatePositionState() {
    const ms = mediaSession();
    if (!ms || !ms.setPositionState) return;
    const dur = duration();
    if (!(dur > 0)) return;
    try {
      ms.setPositionState({
        duration: dur,
        position: Math.min(Math.max(position(), 0), dur),
        playbackRate: (video && video.playbackRate) || 1,
      });
    } catch (e) { /* a clock the browser would not take */ }
    ms.playbackState = isPlaying() ? 'playing' : 'paused';
  }
  // The neighbour a track button asks for: the work's order, off the item
  // document's own relations, taken the way the transport's prev/next take
  // it. The session's `next` is the fallback, because a run's next member is
  // the session's and not the item's.
  function goToNeighbour(rel) {
    const href = R.linkHref(item, rel);
    if (href) { goTo(Number(R.idIn(href)), 'nav'); return; }
    if (rel === 'next' && nextId() != null) advance();
  }
  function bindMediaSession() {
    const ms = mediaSession();
    if (!ms || !ms.setActionHandler) return;
    const handlers = {
      play: () => { if (!isPlaying()) togglePlay(); },
      pause: () => { if (isPlaying()) togglePlay(); },
      seekbackward: d => seekTo(position() - ((d && d.seekOffset) || 10)),
      seekforward: d => seekTo(position() + ((d && d.seekOffset) || 30)),
      seekto: d => { if (d && typeof d.seekTime === 'number') seekTo(d.seekTime); },
      previoustrack: () => goToNeighbour('prev'),
      nexttrack: () => goToNeighbour('next'),
    };
    for (const name of Object.keys(handlers)) {
      // A browser that does not know an action THROWS on its name rather
      // than ignoring it, so each one is set on its own.
      try { ms.setActionHandler(name, handlers[name]); } catch (e) { /* not this browser's */ }
    }
  }

  // Picture-in-picture is the browser's own small window, and only the video
  // element may go into it. The button appears when the browser has the
  // feature; audio mode and casting hide it in CSS, having no picture here to
  // put in a window.
  function pipAvailable() {
    return !!(document.pictureInPictureEnabled && video && !video.disablePictureInPicture);
  }
  function syncPipButton() {
    const b = $('btn-pip');
    if (!b) return;
    b.hidden = !pipAvailable();
    const on = document.pictureInPictureElement === video;
    b.title = on ? 'Leave picture in picture' : 'Picture in picture';
    b.setAttribute('aria-label', b.title);
  }
  function togglePip() {
    if (!pipAvailable()) return;
    if (document.pictureInPictureElement) { document.exitPictureInPicture().catch(() => {}); return; }
    video.requestPictureInPicture().catch(() => {});
  }

  // The listeners this pair needs, bound beside the device's own rather than
  // inside them: the OS's handlers never change, and the button answers a
  // click of its own.
  function bindNowPlaying() {
    bindMediaSession();
    element.addEventListener('click', e => {
      if (e.target.id === 'btn-pip') togglePip();
    });
    video.addEventListener('enterpictureinpicture', syncPipButton);
    video.addEventListener('leavepictureinpicture', syncPipButton);
    video.addEventListener('play', updateNowPlaying);
    video.addEventListener('pause', updateNowPlaying);
    syncPipButton();
  }

  // --- casting -----------------------------------------------------------------

  function initCast() {
    const ctx = root.cast.framework.CastContext.getInstance();
    ctx.setOptions({
      receiverApplicationId: localStorage.castAppId ||
        root.chrome.cast.media.DEFAULT_MEDIA_RECEIVER_APP_ID,
      autoJoinPolicy: root.chrome.cast.AutoJoinPolicy.ORIGIN_SCOPED,
    });
    remotePlayer = new root.cast.framework.RemotePlayer();
    remoteController = new root.cast.framework.RemotePlayerController(remotePlayer);
    remoteController.addEventListener(
      root.cast.framework.RemotePlayerEventType.IS_CONNECTED_CHANGED, onCastConnectionChanged);
    remoteController.addEventListener(
      root.cast.framework.RemotePlayerEventType.PLAYER_STATE_CHANGED, onCastPlayerState);
  }

  function onCastPlayerState() {
    syncPlayButton();
    const st = remotePlayer.playerState;
    if (st === castLastState) return;
    const prev = castLastState;
    castLastState = st;
    if (st === 'PLAYING' && prev === 'PAUSED' && passageEndFired && !passageNaturalEnd) {
      const keep = R.actionOf(session, 'keep_watching');
      keepWatching(keep ? { href: keep.href, method: keep.method } : null);
    }
    if (st === 'PLAYING' || st === 'BUFFERING' || st === 'PAUSED') { castEndSuppressed = false; return; }
    if (st !== 'IDLE' || castEndSuppressed || !item) return;
    const s = root.cast.framework.CastContext.getInstance().getCurrentSession();
    const ms = s && s.getMediaSession();
    if (ms && ms.idleReason === 'FINISHED') onPlaybackEnded();
  }

  function onCastConnectionChanged() {
    document.body.classList.toggle('casting', casting());
    if (casting()) {
      const s = root.cast.framework.CastContext.getInstance().getCurrentSession();
      const dev = s && s.getCastDevice();
      if ($('cast-device')) $('cast-device').textContent = (dev && dev.friendlyName) || 'Chromecast';
      paintCastRemote();
      video.pause();
    }
    if (item) {
      const pos = position();
      const act = R.actionOf(item, 'play');
      if (act) startPlayback(act, pos > 5 ? pos : 0);
    }
  }

  // Stop casting hands the sitting back to this page: ending the session
  // fires IS_CONNECTED_CHANGED, and the handler above restarts the play here.
  function stopCasting() {
    const f = root.cast && root.cast.framework;
    if (f) f.CastContext.getInstance().endCurrentSession(true);
  }

  // The receiver is handed the SESSION DOCUMENT, not a pile of fields this
  // page composed for it: castMediaSpec (web/cast.js) reads the url, the media
  // type, the segment format the decision negotiated, where to start and what
  // to show off the one object the play answered. The subtitle tracks stay
  // here: they are the ITEM document's, by ordinal, with their own addresses.
  async function castLoad() {
    castEndSuppressed = true;
    const spec = root.castMediaSpec(session, item, { baseUrl });
    const media = new root.chrome.cast.media.MediaInfo(spec.url, spec.contentType);
    if (spec.isHls) {
      const fmp4 = spec.segmentFormat === 'fmp4';
      media.hlsSegmentFormat = fmp4 ? root.chrome.cast.media.HlsSegmentFormat.FMP4
                                    : root.chrome.cast.media.HlsSegmentFormat.TS;
      media.hlsVideoSegmentFormat = fmp4 ? root.chrome.cast.media.HlsVideoSegmentFormat.FMP4
                                         : root.chrome.cast.media.HlsVideoSegmentFormat.TS;
      media.streamType = root.chrome.cast.media.StreamType.BUFFERED;
    }
    media.metadata = new root.chrome.cast.media.GenericMediaMetadata();
    media.metadata.title = spec.title;
    if (spec.subtitle) media.metadata.subtitle = spec.subtitle;
    if (spec.images.length) media.metadata.images = spec.images.map(u => new root.chrome.cast.Image(u));
    if (spec.customData) media.customData = spec.customData;

    const ok = supportedSubs();
    if (ok.length) {
      media.tracks = ok.map(s => {
        const t = new root.chrome.cast.media.Track(s.ordinal + 1, root.chrome.cast.media.TrackType.TEXT);
        t.trackContentId = root.castAbsolute(baseUrl, s.href);
        t.trackContentType = 'text/vtt';
        t.subtype = root.chrome.cast.media.TextTrackType.SUBTITLES;
        t.name = subLabel(s);
        t.language = s.language || 'en';
        return t;
      });
    }
    const req = new root.chrome.cast.media.LoadRequest(media);
    if (ok.length) {
      req.activeTrackIds = (selectedSubOrdinal != null && ok.some(s => s.ordinal === selectedSubOrdinal))
        ? [selectedSubOrdinal + 1] : [];
    }
    req.currentTime = spec.startTime;
    req.autoplay = true;
    if (spec.customData) req.customData = spec.customData;
    await root.cast.framework.CastContext.getInstance().getCurrentSession().loadMedia(req);
  }

  // --- init --------------------------------------------------------------------

  function init(kernel) {
    K = kernel;
    profiles.browser = detectBrowserCaps();
    if (!element) build();
    root.__onGCastApiAvailable = ok => { if (ok) initCast(); };
  }

  root.Player = {
    init, attach, invoke, play, playFrom, close, holdSession,
    goTo, advance, cancelUpNext: hideUpNext,
    // The setting is the device's; the settings panel hosts a second switch
    // for it, so it is read and set through here rather than duplicated.
    setAutoplay, autoplay: () => autoplayNext,
    setBaseUrl: u => { baseUrl = u; },
    playingItem: () => (item ? item.id : null),
    passage: () => passage,
    session: () => session,
    get element() { return element; },
  };
})(typeof window !== 'undefined' ? window : this);
