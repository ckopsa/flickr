// touch.js — the pure parts of touch gestures: no DOM, tested under node
//
// A finger is not a mouse: it has no hover, it arrives with a rectangle of
// skin rather than a point, and the gestures it makes — a tap in a zone, a
// swipe across the page, two fingers pinching — are DECISIONS over a log of
// pointer positions. The decisions are the part with the bugs in it, so they
// live here as functions over numbers, the way focus.js holds the D-pad's
// arithmetic, and the panes and the player keep only the listeners.
(function (root) {
  'use strict';

  // The shared surface. Each section below hangs its own functions on it, so
  // two of them can live in this file without one export line to fight over.
  const api = (typeof module === 'object' && module.exports)
    ? module.exports : (root.Touch = root.Touch || {});

  // --- player -------------------------------------------------------------------

  // How long a single tap waits for its twin.
  const DOUBLE_TAP_MS = 300;

  // Which third of the picture a tap landed in, from the x INSIDE the element
  // and that element's width. A width of nothing is not a picture: call it the
  // middle, which is the harmless answer — play or pause, never a seek.
  function tapZone(x, width) {
    if (!(width > 0)) return 'middle';
    const f = x / width;
    if (f < 1 / 3) return 'left';
    if (f > 2 / 3) return 'right';
    return 'middle';
  }

  // The double-tap state machine, one tap at a time. `pending` is what the
  // last tap left behind — { zone, at } — or null, and the answer says what
  // THIS tap is and what to remember for the next one.
  //
  //   single — nothing may happen yet: the caller defers the action by the
  //            window, so the first tap of a double never acts at all
  //   double — a second tap on the SAME third inside the window: the caller
  //            drops the deferred single and acts now
  //
  // A second tap on a DIFFERENT third is no double — a thumb that crossed the
  // picture meant two things — so it starts over as a single.
  function tapPhase(pending, zone, at, windowMs) {
    const w = windowMs > 0 ? windowMs : DOUBLE_TAP_MS;
    if (pending && pending.zone === zone && at - pending.at < w) {
      return { phase: 'double', zone, pending: null };
    }
    return { phase: 'single', zone, pending: { zone, at } };
  }

  // What the tap ASKED FOR, once its phase is known: the one decision the
  // device then carries out.
  //
  //   back / forward — a double tap on a third that is not the middle
  //   reveal         — the controls have faded out: a tap on a dark picture is
  //                    asking for them back, not for a pause. The tap after
  //                    it, with the chrome up, pauses as any tap does.
  //   toggle         — play or pause, the one thing a mouse click has always
  //                    meant here
  function tapIntent(phase, zone, chromeHidden) {
    if (phase === 'double' && zone === 'left') return 'back';
    if (phase === 'double' && zone === 'right') return 'forward';
    if (phase === 'single' && chromeHidden) return 'reveal';
    return 'toggle';
  }

  api.tapZone = tapZone;
  api.tapPhase = tapPhase;
  api.tapIntent = tapIntent;
  api.DOUBLE_TAP_MS = DOUBLE_TAP_MS;

// --- readers --------------------------------------------------------------------
// The reader's page is three invisible zones wide (left turns back, right
// turns on, the middle shows and hides the chrome), a swipe across it turns
// it too, and on a PDF two fingers scale the page.

  const SWIPE_MIN = 60;    // how far across the page counts as a turn
  const SWIPE_DRIFT = 40;  // ...and how far up or down still counts as across
  const SWIPE_MS = 400;    // a slower drag is reading, or a scroll, not a turn
  const ZOOM_MIN = 0.75, ZOOM_MAX = 3;

  // Which third of the page a tap landed in. x is measured from the page's
  // left edge; anything off either end belongs to the zone it went past.
  function readerZone(x, width) {
    if (!(width > 0)) return 'menu';
    const f = x / width;
    if (f < 1 / 3) return 'prev';
    if (f > 2 / 3) return 'next';
    return 'menu';
  }

  // The turn a pointer log asks for, or null for anything that is not a
  // swipe: `points` is [{x, y, t}, ...] from the touch going down to it
  // coming up, `now` is when it came up. A swipe is far enough across, not
  // far up or down at any point along the way, and quick. A LEFT swipe turns
  // the page on, the way a left swipe moves a photo out of the way.
  function swipeOf(points, now) {
    if (!points || points.length < 2) return null;
    const a = points[0], b = points[points.length - 1];
    if (!(now - a.t < SWIPE_MS)) return null;
    let drift = 0;
    for (const p of points) drift = Math.max(drift, Math.abs(p.y - a.y));
    if (drift >= SWIPE_DRIFT) return null;
    const dx = b.x - a.x;
    if (!(Math.abs(dx) > SWIPE_MIN)) return null;
    return dx < 0 ? 'next' : 'prev';
  }

  // The scale a pinch asks for: the scale the page was at when the two
  // fingers went down, times how much further apart they are now, held
  // between a page three quarters of the pane wide and three times it.
  // A degenerate first distance (the two fingers landed on one spot) leaves
  // the page where it was.
  function pinchScale(d0, d1, base) {
    const b = base > 0 ? base : 1;
    const s = d0 > 0 && d1 > 0 ? b * d1 / d0 : b;
    return s < ZOOM_MIN ? ZOOM_MIN : s > ZOOM_MAX ? ZOOM_MAX : s;
  }

  api.readerZone = readerZone;
  api.swipeOf = swipeOf;
  api.pinchScale = pinchScale;
})(typeof window !== 'undefined' ? window : this);
