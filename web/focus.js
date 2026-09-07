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
//
// AND ONE EXCEPTION: when nothing lies that way at all, Right and Left WRAP —
// off the end of a row to the start of the next, off the start of one to the
// end of the row above — because on a remote the end of a row is a line
// break, not a dead end.
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

  // Two rectangles are on the same ROW when their vertical spans touch: a
  // row is a band of the screen, not a container, so a tall poster and the
  // short button beside it are on one.
  function sameRow(a, b) { return gap(a.top, a.bottom, b.top, b.bottom) === 0; }

  // Right off the end of a row, or Left off the start of one: reading order
  // says where that goes — the first thing on the next row down, the last
  // thing on the row above. Without it the last tile of a row is a dead end,
  // and a remote has no other way past it.
  function wrap(from, rects, dir) {
    if (dir !== 'left' && dir !== 'right') return -1;
    const down = dir === 'right';
    // The nearest row on that side of ours, named by any one of its members.
    let row = -1;
    for (let i = 0; i < rects.length; i++) {
      const r = rects[i];
      if (sameRow(from, r)) continue;
      if (down ? centreY(r) <= centreY(from) : centreY(r) >= centreY(from)) continue;
      if (row < 0 || (down ? centreY(r) < centreY(rects[row]) : centreY(r) > centreY(rects[row]))) row = i;
    }
    if (row < 0) return -1;
    // …and the end of it the arrow arrives at.
    let best = row;
    for (let i = 0; i < rects.length; i++) {
      if (!sameRow(rects[row], rects[i])) continue;
      if (down ? centreX(rects[i]) < centreX(rects[best]) : centreX(rects[i]) > centreX(rects[best])) best = i;
    }
    return best;
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
    if (best < 0) return wrap(from, rects, dir);
    return best;
  }

  const api = { pick, CROSS };
  if (typeof module === 'object' && module.exports) module.exports = api;
  else root.Focus = api;
})(typeof window !== 'undefined' ? window : this);
