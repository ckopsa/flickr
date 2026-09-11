// Run with: node --test web/touch_test.mjs
//
// What a finger meant, driven with numbers. No DOM is needed and none is used:
// touch.js takes an x, a width and a clock and answers what the tap was for,
// which is exactly the part of a thumb's player worth pinning — the thirds,
// the window, and the two bugs a naive version always has (the first tap of a
// double pausing and unpausing, and a double counted across two sides).
import test from 'node:test';
import assert from 'node:assert/strict';
// touch.js is a plain browser script with a CommonJS export bolted on; Node
// cannot see named exports through its IIFE, so take the default.
import touch from './touch.js';
const { tapZone, tapPhase, tapIntent, DOUBLE_TAP_MS } = touch;

// A phone's picture, sideways: the width the thirds are measured against.
const W = 844;

test('the thirds of the picture', () => {
  assert.equal(tapZone(0, W), 'left');
  assert.equal(tapZone(W * 0.32, W), 'left');
  assert.equal(tapZone(W * 0.34, W), 'middle');
  assert.equal(tapZone(W / 2, W), 'middle');
  assert.equal(tapZone(W * 0.66, W), 'middle');
  assert.equal(tapZone(W * 0.68, W), 'right');
  assert.equal(tapZone(W, W), 'right');
});

test('a picture of no width is all middle: a seek needs a side to be on', () => {
  assert.equal(tapZone(0, 0), 'middle');
  assert.equal(tapZone(40, -1), 'middle');
  assert.equal(tapZone(40, undefined), 'middle');
});

test('one tap is a single, and it remembers where and when it landed', () => {
  const t = tapPhase(null, 'right', 1000);
  assert.equal(t.phase, 'single');
  assert.deepEqual(t.pending, { zone: 'right', at: 1000 });
});

test('a second tap on the same third inside the window is the double', () => {
  const first = tapPhase(null, 'right', 1000);
  const second = tapPhase(first.pending, 'right', 1000 + DOUBLE_TAP_MS - 1);
  assert.equal(second.phase, 'double');
  // Nothing is left pending: a third tap starts a fresh pair rather than
  // reading as a second double off the same first tap.
  assert.equal(second.pending, null);
  const third = tapPhase(second.pending, 'right', 1000 + DOUBLE_TAP_MS);
  assert.equal(third.phase, 'single');
});

test('a tap past the window is a new single, not a double', () => {
  const first = tapPhase(null, 'left', 1000);
  const late = tapPhase(first.pending, 'left', 1000 + DOUBLE_TAP_MS);
  assert.equal(late.phase, 'single');
  assert.deepEqual(late.pending, { zone: 'left', at: 1000 + DOUBLE_TAP_MS });
});

test('a thumb that crosses the picture meant two things', () => {
  const first = tapPhase(null, 'left', 1000);
  const across = tapPhase(first.pending, 'right', 1010);
  assert.equal(across.phase, 'single', 'two thirds, two taps');
  const middle = tapPhase(first.pending, 'middle', 1010);
  assert.equal(middle.phase, 'single');
});

test('the window can be said in so many words', () => {
  const first = tapPhase(null, 'right', 0, 50);
  assert.equal(tapPhase(first.pending, 'right', 40, 50).phase, 'double');
  assert.equal(tapPhase(first.pending, 'right', 60, 50).phase, 'single');
  // Nonsense falls back on the default rather than making every tap a double.
  assert.equal(tapPhase(first.pending, 'right', 200, 0).phase, 'double');
  assert.equal(tapPhase(first.pending, 'right', 400, -1).phase, 'single');
});

test('a double tap on a side seeks, in the middle it pauses', () => {
  assert.equal(tapIntent('double', 'left', false), 'back');
  assert.equal(tapIntent('double', 'right', false), 'forward');
  assert.equal(tapIntent('double', 'middle', false), 'toggle');
  // A seek is a seek whether or not the controls were up.
  assert.equal(tapIntent('double', 'left', true), 'back');
  assert.equal(tapIntent('double', 'right', true), 'forward');
});

test('a tap on a dark picture asks for the controls, not for a pause', () => {
  assert.equal(tapIntent('single', 'middle', true), 'reveal');
  assert.equal(tapIntent('single', 'left', true), 'reveal');
  assert.equal(tapIntent('single', 'right', true), 'reveal');
  // ...and the next one, with them up, pauses as any tap does.
  assert.equal(tapIntent('single', 'middle', false), 'toggle');
  assert.equal(tapIntent('single', 'left', false), 'toggle');
});
