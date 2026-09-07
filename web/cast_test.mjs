// Run with: node --test web/cast_test.mjs
import test from 'node:test';
import assert from 'node:assert/strict';
// cast.js is a plain browser script with a CommonJS export bolted on (the
// receiver loads it as a <script> too); Node cannot see named exports
// through its IIFE, so take the default.
import cast from './cast.js';
const { castAbsolute: absolute, sessionEndBound, castSeekOffset, castReceiverBound,
        castStartTime, castMediaSpec } = cast;

// A direct-play session document, shaped as cmd/server/session.go writes it.
const direct = {
  self: '/api/sessions/abc123',
  kind: 'session',
  title: 'Beach Games',
  id: 'abc123',
  item_id: 8,
  method: 'direct_play',
  url: 'https://storage.test/Shows/The%20Office/S03E22.mkv?signed',
  content_type: 'video/x-matroska',
  decision: { method: 'direct_play', trace: [] },
  passage: null,
  marks: {},
  links: {
    item: { href: '/api/items/8', title: 'Beach Games' },
    back: { href: '/api/items/8', title: 'Beach Games' },
    work: { href: '/api/works/show%3Athe-office', title: 'The Office' },
    next: { href: '/api/items/9', title: 'The Job' },
    artwork: { href: '/api/items/8/still' },
  },
  actions: {},
};

// A transcode, seeked: the stream is cut at 142s server-side.
const transcode = {
  ...direct,
  method: 'transcode',
  url: '/streams/deadbeef/master.m3u8',
  content_type: 'application/x-mpegurl',
  seek_seconds: 142,
  decision: { method: 'transcode', target: { segment_format: 'fmp4' } },
  passage: { t: 142, end: 854, ends_at: 854 },
};

const BASE = 'http://10.0.0.9:8099';

test('absolute resolves a document address, leaves a presigned URL alone', () => {
  assert.equal(absolute(BASE, '/api/sessions/abc123'), BASE + '/api/sessions/abc123');
  assert.equal(absolute(BASE, 'https://storage.test/x?signed'), 'https://storage.test/x?signed');
  assert.equal(absolute(BASE, ''), '');
  assert.equal(absolute('', '/api/x'), '/api/x');
});

test('sessionEndBound is the document\'s, or nothing', () => {
  assert.equal(sessionEndBound(transcode), 854);
  assert.equal(sessionEndBound(direct), null);
  assert.equal(sessionEndBound(null), null);
  // The first item of a RUN: `end` belongs to the last member, so this
  // session has no bound of its own and the server sends no ends_at.
  assert.equal(sessionEndBound({ passage: { t: 142, end: 300, until: 9 } }), null);
});

test('the receiver\'s clock runs from the seek the server baked in', () => {
  assert.equal(castSeekOffset(transcode), 142);
  assert.equal(castSeekOffset(direct), 0);
  // A direct play seeked to the passage start is still the whole file: the
  // two clocks agree, and the bound needs no adjusting.
  assert.equal(castSeekOffset({ method: 'direct_play', seek_seconds: 142 }), 0);
  assert.equal(castReceiverBound(transcode), 854 - 142);
  assert.equal(castReceiverBound(direct), null);
});

test('an HLS session starts at 0; a direct play starts at the passage', () => {
  assert.equal(castStartTime(transcode), 0); // already positioned server-side
  assert.equal(castStartTime(direct), 0);
  assert.equal(castStartTime({ method: 'direct_play', passage: { t: 142, ends_at: 854 } }), 142);
  assert.equal(castStartTime({ method: 'direct_play', seek_seconds: 745 }), 745);
  // A run's later member: `t` was the first item's, so it starts at the top.
  assert.equal(castStartTime({ method: 'direct_play', passage: { until: 9 } }), 0);
});

test('castMediaSpec reads a direct play off the document', () => {
  const spec = castMediaSpec(direct, null, { baseUrl: BASE });
  assert.equal(spec.url, 'https://storage.test/Shows/The%20Office/S03E22.mkv?signed');
  assert.equal(spec.contentType, 'video/x-matroska'); // not the old "video/mp4" guess
  assert.equal(spec.isHls, false);
  assert.equal(spec.segmentFormat, null);
  assert.equal(spec.title, 'Beach Games');
  assert.equal(spec.subtitle, 'The Office');
  assert.deepEqual(spec.images, [BASE + '/api/items/8/still']);
  assert.equal(spec.startTime, 0);
  assert.equal(spec.endsAt, null);
  assert.deepEqual(spec.customData, { session: BASE + '/api/sessions/abc123' });
});

test('castMediaSpec reads a transcoded passage off the document', () => {
  const spec = castMediaSpec(transcode, null, { baseUrl: BASE });
  assert.equal(spec.url, BASE + '/streams/deadbeef/master.m3u8');
  assert.equal(spec.contentType, 'application/x-mpegurl');
  assert.equal(spec.isHls, true);
  assert.equal(spec.segmentFormat, 'fmp4');
  assert.equal(spec.startTime, 0);
  assert.equal(spec.endsAt, 854);
});

test('segment format falls back to TS, the floor every receiver takes', () => {
  const spec = castMediaSpec({ ...transcode, decision: { method: 'transcode', target: {} } },
                             null, { baseUrl: BASE });
  assert.equal(spec.segmentFormat, 'ts');
});

test('a document from an older server still loads', () => {
  // No content_type, no artwork, no title: the type follows the method, the
  // item stands in for the name, and there is simply no picture.
  const old = { self: '/api/sessions/x', method: 'transcode', url: '/streams/x/master.m3u8',
                decision: {}, links: {} };
  const spec = castMediaSpec(old, { title: 'Beach Games' }, { baseUrl: BASE });
  assert.equal(spec.contentType, 'application/x-mpegurl');
  assert.equal(spec.title, 'Beach Games');
  assert.deepEqual(spec.images, []);
  assert.equal(spec.subtitle, '');

  const oldDirect = castMediaSpec({ method: 'direct_play', url: 'https://s/x.mp4' }, null, {});
  assert.equal(oldDirect.contentType, 'video/mp4');
  assert.equal(oldDirect.customData, null);
});
