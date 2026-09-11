// flickr — what a finger meant (docs/hypermedia.md §The client).
//
// A tap is not a click. A click means one thing the moment it lands; a tap
// means nothing until the double-tap window has passed without a second one,
// and WHERE it landed decides what it meant — the left third of a picture is
// "back", the right third "forward", the middle "play or pause". That rule is
// the part with the bugs in it (an off-by-one third, a first tap that pauses
// and then unpauses, a double counted across two different sides), not the
// listener around it, so it lives here as pure functions over numbers and
// web/touch_test.mjs drives it with no DOM at all.
(function (root) {
  'use strict';

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

  const api = { tapZone, tapPhase, tapIntent, DOUBLE_TAP_MS };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Touch = api;
})(typeof window !== 'undefined' ? window : this);
