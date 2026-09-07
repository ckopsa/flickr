// flickr — spatial focus: the arithmetic behind a D-pad (docs/hypermedia.md
// §The client).
//
// A television is a screen nobody touches. The remote sends four arrows and an
// OK button, and a web page moves focus for none of them: Tab order is
// document order, which on a grid of posters walks along a row and then keeps
// walking, so "down" is not a thing the browser has. This file is that thing,
// and it is one pure function over bounding rectangles — no DOM, no page, no
// state — because the picking is the part with the bugs in it, not the
// querySelectorAll around it. web/focus_test.mjs drives it with fake rects.
//
// THE RULE, in words: a candidate is in play if its CENTRE lies beyond ours in
// the arrow's direction. Among those the nearest wins, where "near" is the
// step along the arrow plus a heavy penalty for how far the two rectangles
// MISS each other across it. The penalty is the whole trick: it keeps Down
// inside its column and Right inside its row, while still letting either reach
// the next band once the column runs out.
(function (root) {
  'use strict';

  // What a miss across the arrow costs against a step along it. High enough
  // that the tile directly below beats the one below and over; not so high
  // that a column with nothing under it leads nowhere at all.
  const CROSS = 4;

  // Sub-pixel layout: a candidate has to be beyond us by more than a rounding
  // error, or a row of tiles picks itself.
  const EPS = 1;

  function centreX(r) { return (r.left + r.right) / 2; }
  function centreY(r) { return (r.top + r.bottom) / 2; }

  // The gap between two spans on one axis; 0 for as long as they overlap.
  function gap(aLo, aHi, bLo, bHi) {
    if (bHi < aLo) return aLo - bHi;
    if (bLo > aHi) return bLo - aHi;
    return 0;
  }

  // What one candidate costs from here, or -1 when it is not in that
  // direction at all.
  function cost(from, to, dir) {
    let along, across;
    if (dir === 'left' || dir === 'right') {
      along = dir === 'left' ? centreX(from) - centreX(to) : centreX(to) - centreX(from);
      across = gap(from.top, from.bottom, to.top, to.bottom);
    } else {
      along = dir === 'up' ? centreY(from) - centreY(to) : centreY(to) - centreY(from);
      across = gap(from.left, from.right, to.left, to.right);
    }
    if (!(along > EPS)) return -1;
    return along + CROSS * across;
  }

  // Which of `rects` the arrow points at from `from`, as an index into the
  // list, or -1 for nothing that way. `from` null is the first press with
  // nothing focused: the first candidate is where the ring starts.
  function pick(from, rects, dir) {
    if (!rects || !rects.length) return -1;
    if (!from) return 0;
    if (dir !== 'left' && dir !== 'right' && dir !== 'up' && dir !== 'down') return -1;
    let best = -1;
    let bestCost = Infinity;
    for (let i = 0; i < rects.length; i++) {
      const c = cost(from, rects[i], dir);
      if (c < 0 || c >= bestCost) continue;
      best = i;
      bestCost = c;
    }
    return best;
  }

  const api = { pick, CROSS };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Focus = api;
})(typeof window !== 'undefined' ? window : this);
