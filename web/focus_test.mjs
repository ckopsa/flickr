// Run with: node --test web/focus_test.mjs
//
// The D-pad's arithmetic, driven with fake rectangles. No DOM is needed and
// none is used: focus.js takes rects and answers an index, which is exactly
// the part of ten-foot navigation worth pinning — a grid where Down lands in
// the next row of the SAME column, a row where Right walks along it, and an
// edge where the arrow points at nothing and says so.
import test from 'node:test';
import assert from 'node:assert/strict';
// focus.js is a plain browser script with a CommonJS export bolted on; Node
// cannot see named exports through its IIFE, so take the default.
import focus from './focus.js';
const { pick } = focus;

// A rect as the DOM writes one, from a top-left corner and a size.
const r = (x, y, w, h) => ({ left: x, top: y, right: x + w, bottom: y + h });

// Six posters to a row, two rows — the ten-foot library grid.
const TILE = 180, POSTER = 270, GAP = 28;
const grid = [];
for (let row = 0; row < 2; row++) {
  for (let col = 0; col < 6; col++) {
    grid.push(r(40 + col * (TILE + GAP), 120 + row * (POSTER + GAP), TILE, POSTER));
  }
}
const at = (row, col) => row * 6 + col;

test('nothing focused: the first press lands on the first candidate', () => {
  assert.equal(pick(null, grid, 'down'), 0);
  assert.equal(pick(null, grid, 'right'), 0);
  assert.equal(pick(null, [], 'down'), -1, 'nothing to land on is not a landing');
});

test('right and left walk along the row', () => {
  assert.equal(pick(grid[at(0, 2)], grid, 'right'), at(0, 3));
  assert.equal(pick(grid[at(0, 2)], grid, 'left'), at(0, 1));
  assert.equal(pick(grid[at(1, 0)], grid, 'right'), at(1, 1));
});

test('down and up stay in the column', () => {
  assert.equal(pick(grid[at(0, 3)], grid, 'down'), at(1, 3));
  assert.equal(pick(grid[at(1, 3)], grid, 'up'), at(0, 3));
  assert.equal(pick(grid[at(0, 0)], grid, 'down'), at(1, 0));
  assert.equal(pick(grid[at(0, 5)], grid, 'down'), at(1, 5));
});

test('the edge of the grid points at nothing', () => {
  assert.equal(pick(grid[at(0, 5)], grid, 'right'), -1);
  assert.equal(pick(grid[at(0, 0)], grid, 'left'), -1);
  assert.equal(pick(grid[at(0, 2)], grid, 'up'), -1);
  assert.equal(pick(grid[at(1, 2)], grid, 'down'), -1);
});

test('an element never picks itself, however it is asked', () => {
  const one = [r(0, 0, 100, 100)];
  for (const d of ['left', 'right', 'up', 'down']) assert.equal(pick(one[0], one, d), -1);
});

test('a direction nobody sends is not a direction', () => {
  assert.equal(pick(grid[0], grid, 'sideways'), -1);
});

// The page is not a grid: a header over a row of tiles, a row of tiles over a
// band heading. Down out of the header must reach the tiles under it rather
// than the tile it happens to share an edge with.
test('down out of the header lands under it, not along it', () => {
  const header = [r(40, 20, 120, 40), r(680, 20, 150, 40), r(1000, 20, 60, 40)];
  const all = header.concat(grid);
  assert.equal(pick(header[1], all, 'down'), header.length + at(0, 3),
    'the tile the search box sits over, not the first tile on the row');
  assert.equal(pick(grid[at(0, 0)], all, 'up'), 0, 'and back up to the one above it');
});

// A band that runs off the side is a ROW, and the next band is under it. The
// last tile of a row has nothing to its right, but Down still crosses bands.
test('down crosses from one band into the next', () => {
  const bandA = [r(40, 100, 180, 270), r(248, 100, 180, 270)];
  const bandB = [r(40, 430, 180, 270), r(248, 430, 180, 270)];
  const all = bandA.concat(bandB);
  assert.equal(pick(bandA[1], all, 'down'), 3);
  assert.equal(pick(bandB[0], all, 'up'), 0);
});

// Nothing sits directly below, so the penalty has to give way: a lone control
// off to the side is still where Down goes.
test('with nothing straight down, the nearest thing down wins', () => {
  const from = r(40, 100, 180, 270);
  const cands = [from, r(900, 420, 120, 40)];
  assert.equal(pick(from, cands, 'down'), 1);
});

// Rows of different heights (a tall poster beside a short button) overlap on
// the cross axis, and overlapping costs nothing: the step along decides.
test('overlap across the arrow costs nothing; the step along decides', () => {
  const from = r(0, 100, 100, 300);
  const near = r(140, 220, 100, 60);   // fully inside the tall rect's span
  const far = r(400, 220, 100, 60);
  assert.equal(pick(from, [from, near, far], 'right'), 1);
});
