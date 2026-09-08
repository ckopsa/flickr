// flickr — the Cast bridge, as pure functions of the SESSION DOCUMENT.
//
// A play is a document (docs/hypermedia.md §Passages are session state), and
// the sender stops composing what the receiver plays out of loose fields it
// happened to be holding. Everything the bridge needs is on that one object:
//
//   url            where the bytes are
//   content_type   what they are, in the words a media element takes
//   method         "transcode" (an HLS playlist) or "direct_play" (a file)
//   seek_seconds   where the bytes BEGIN — a transcode is positioned
//                  server-side, so the receiver's own clock starts there
//   decision.target.segment_format   fmp4 or ts, as the device negotiated it
//   passage.ends_at                  the bound to stop at, absolute in the
//                                    item's timeline
//   title, links.work, links.artwork what the device shows while it plays
//   subtitles                        the tracks it loads, addressed for it
//   self                             the document's own address
//
// Both ends of the cast read it the same way: index.html builds the
// LoadRequest from castMediaSpec and puts `self` in customData, and
// receiver.html fetches the document at that address and asks
// castReceiverBound where to pause. One reading of the bound, two players.
//
// Deliberately conservative JavaScript (no optional chaining, no nullish
// coalescing): this file is parsed by a Cast device's browser as well as by
// index.html and by `node --test web/cast_test.mjs`.
(function (root) {
  'use strict';

  // absolute resolves a document's own address against the LAN-reachable
  // base the server advertises (GET /api/system). A cast device cannot
  // resolve the sender's localhost, and an already-absolute URL — a
  // presigned storage URL — is left exactly as it is.
  function absolute(base, href) {
    if (!href) return '';
    return href.charAt(0) === '/' ? (base || '') + href : href;
  }

  function linkHref(session, rel) {
    var links = (session && session.links) || {};
    var link = links[rel];
    return (link && link.href) || '';
  }

  function linkTitle(session, rel) {
    var links = (session && session.links) || {};
    var link = links[rel];
    return (link && link.title) || '';
  }

  // sessionEndBound is rule 2 in one line: the bound this session is to stop
  // at, in the ITEM's timeline, or null when there is none to stop at — an
  // ordinary play, or the first item of a run, whose `end` belongs to the
  // last member and not to this one. The server worked it out; nobody
  // recomputes it.
  function sessionEndBound(session) {
    var p = session && session.passage;
    if (!p || typeof p.ends_at !== 'number') return null;
    return p.ends_at;
  }

  // castSeekOffset is the difference between the item's timeline and the
  // playing device's. A transcode is cut at the seek, so the stream's 0 is
  // the item's `seek_seconds`; a direct play is the whole file and the two
  // clocks are the same.
  function castSeekOffset(session) {
    if (!session || session.method !== 'transcode') return 0;
    return typeof session.seek_seconds === 'number' ? session.seek_seconds : 0;
  }

  // castReceiverBound is the same bound in the RECEIVER's own clock — what a
  // custom receiver compares getCurrentTimeSec() against, having taken off
  // the offset the server baked into the stream.
  function castReceiverBound(session) {
    var end = sessionEndBound(session);
    if (end == null) return null;
    return end - castSeekOffset(session);
  }

  // castStartTime is where the receiver is told to begin. An HLS session is
  // already positioned server-side, so it is pinned to 0 — otherwise the
  // receiver jumps to the live edge of a playlist that is still growing. A
  // direct play is the whole file, so the passage's `t` (or the resume the
  // server was asked for, which is the same number) is a real seek.
  function castStartTime(session) {
    if (!session || session.method === 'transcode') return 0;
    var p = session.passage;
    if (p && typeof p.t === 'number' && p.t > 0) return p.t;
    var seek = session.seek_seconds;
    return typeof seek === 'number' && seek > 0 ? seek : 0;
  }

  // castMediaSpec is the load, described. It is a plain object rather than a
  // chrome.cast.media.MediaInfo so that it can be read in a test without the
  // Cast SDK; player.js turns it into the SDK's objects and adds the
  // subtitle tracks castSubtitles picked.
  //
  // `item` is only a fallback for the title: the session document carries
  // the server's own name for what is playing, and the item is what is left
  // when a caller has a document from an older server.
  function castMediaSpec(session, item, opts) {
    var s = session || {};
    var o = opts || {};
    var base = o.baseUrl || '';
    var isHls = s.method === 'transcode';
    var target = (s.decision && s.decision.target) || {};
    var artwork = linkHref(s, 'artwork');
    var spec = {
      url: absolute(base, s.url || ''),
      contentType: s.content_type || (isHls ? 'application/x-mpegurl' : 'video/mp4'),
      isHls: isHls,
      // TS is the floor and fMP4 is asked for by name: whichever the
      // decision negotiated for THIS device is what the receiver is told.
      segmentFormat: isHls ? (target.segment_format === 'fmp4' ? 'fmp4' : 'ts') : null,
      title: s.title || linkTitle(s, 'item') || (item && item.title) || '',
      // The work the item belongs to reads as the subtitle on a TV: the
      // episode's own name above, the show's below.
      subtitle: linkTitle(s, 'work'),
      images: artwork ? [absolute(base, artwork)] : [],
      startTime: castStartTime(s),
      endsAt: sessionEndBound(s),
      customData: null,
    };
    // The document's own address, so a custom receiver can read the passage
    // and the run for itself instead of being told about them piecemeal.
    if (s.self) spec.customData = { session: absolute(base, s.self) };
    return spec;
  }

  // castSubtitles is which list of subtitle tracks the RECEIVER is loaded
  // with. Behind the gate the item document's addresses are no use to a cast
  // device — it holds no cookie — so the session document carries the same
  // rows with each href written for a device, and those win when they are
  // there. The item's own are the fallback, for a document from an older
  // server. A track with no href is a bitmap track nothing can load.
  function castSubtitles(session, item) {
    var from = (session && session.subtitles) || (item && item.subtitles) || [];
    return from.filter(function (s) { return s && s.supported && s.href; });
  }

  var api = { castAbsolute: absolute, sessionEndBound: sessionEndBound,
              castSeekOffset: castSeekOffset, castReceiverBound: castReceiverBound,
              castStartTime: castStartTime, castMediaSpec: castMediaSpec,
              castSubtitles: castSubtitles };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else Object.assign(root, api);
})(typeof window !== 'undefined' ? window : this);
