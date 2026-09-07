#!/usr/bin/env node
// flickr — the browser smoke: the page opened in a real Chromium and walked
// screen by screen against the fixture library, failing on any JavaScript
// error. The renderers are tested over the goldens and the kernel by grep;
// this is the one check that the whole shell boots, routes, plays, reads and
// searches as a person would see it.
//
//   node scripts/browser-smoke.mjs            # headless, prints a report
//   SMOKE_OUT=/tmp/smoke node scripts/...     # keep screenshots there
//
// Needs: go, node, and Playwright with its Chromium (the web environment has
// them; locally `npm i -g playwright && npx playwright install chromium`).
// The server half is cmd/server/smoke_test.go, built with `-tags smoke` and
// run as a plain binary so it can be stopped with SIGTERM. The media it
// serves is made here: a six-second WebM recorded off a page by Playwright
// itself, and a six-second WAV written by hand — no ffmpeg anywhere.

import { spawn, spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { mkdtempSync, writeFileSync, renameSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
function loadPlaywright() {
  for (const p of ['playwright', '/opt/node22/lib/node_modules/playwright', '/usr/lib/node_modules/playwright']) {
    try { return require(p); } catch (e) { /* next */ }
  }
  throw new Error('playwright is not installed: npm i -g playwright');
}
const { chromium } = loadPlaywright();

const repo = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const out = process.env.SMOKE_OUT || mkdtempSync(join(tmpdir(), 'flickr-smoke-'));
mkdirSync(out, { recursive: true });
const media = join(out, 'media');
mkdirSync(media, { recursive: true });
const port = 8000 + Math.floor(Math.random() * 1000);
const addr = `127.0.0.1:${port}`;
const base = `http://${addr}`;

// --- the media -----------------------------------------------------------------

function writeWav(path, seconds) {
  const rate = 8000, n = rate * seconds;
  const buf = Buffer.alloc(44 + n * 2);
  buf.write('RIFF', 0); buf.writeUInt32LE(36 + n * 2, 4); buf.write('WAVE', 8);
  buf.write('fmt ', 12); buf.writeUInt32LE(16, 16); buf.writeUInt16LE(1, 20); buf.writeUInt16LE(1, 22);
  buf.writeUInt32LE(rate, 24); buf.writeUInt32LE(rate * 2, 28); buf.writeUInt16LE(2, 32); buf.writeUInt16LE(16, 34);
  buf.write('data', 36); buf.writeUInt32LE(n * 2, 40);
  for (let i = 0; i < n; i++) buf.writeInt16LE(Math.round(6000 * Math.sin(2 * Math.PI * 330 * i / rate)), 44 + i * 2);
  writeFileSync(path, buf);
}

async function recordWebm(path, seconds) {
  const browser = await chromium.launch();
  const ctx = await browser.newContext({ recordVideo: { dir: media, size: { width: 320, height: 180 } }, viewport: { width: 320, height: 180 } });
  const page = await ctx.newPage();
  await page.setContent(`<body style="margin:0;font:48px sans-serif;color:#fff"><div id=c></div>
    <script>let i=0;setInterval(()=>{i++;document.body.style.background='hsl('+(i*23%360)+',70%,40%)';document.getElementById('c').textContent=i},200)</script>`);
  await page.waitForTimeout(seconds * 1000);
  const video = page.video();
  await ctx.close();
  await browser.close();
  renameSync(await video.path(), path);
}

// --- the server ----------------------------------------------------------------

function buildServer() {
  const bin = join(out, 'smoke.test');
  const r = spawnSync('go', ['test', '-c', '-tags', 'smoke', '-o', bin, './cmd/server'], { cwd: repo, stdio: 'inherit' });
  if (r.status !== 0) throw new Error('go test -c failed');
  return bin;
}

async function startServer(bin) {
  const child = spawn(bin, ['-test.run', 'TestSmokeServe', '-test.v'], {
    cwd: join(repo, 'cmd', 'server'),
    env: { ...process.env, SMOKE_ADDR: addr, SMOKE_MEDIA: media },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  let log = '';
  child.stdout.on('data', d => { log += d; });
  child.stderr.on('data', d => { log += d; });
  for (let i = 0; i < 100; i++) {
    try {
      const r = await fetch(base + '/api/');
      if (r.ok) return { child, log: () => log };
    } catch (e) { /* not up yet */ }
    if (child.exitCode != null) throw new Error('server exited early:\n' + log);
    await new Promise(r => setTimeout(r, 100));
  }
  throw new Error('server did not come up:\n' + log);
}

// --- the walk ------------------------------------------------------------------

const failures = [];
const jsErrors = [];
let shot = 0;

async function step(page, name, fn) {
  try {
    await fn();
    console.log('ok   ' + name);
  } catch (e) {
    const file = join(out, `${String(++shot).padStart(2, '0')}-${name.replace(/[^a-z0-9]+/gi, '-')}.png`);
    try { await page.screenshot({ path: file, fullPage: true }); } catch (e2) { /* page gone */ }
    failures.push({ name, error: String(e && e.message || e), shot: file });
    console.log('FAIL ' + name + ': ' + String(e && e.message || e).split('\n')[0]);
  }
}

async function expectVisible(page, sel, what) {
  await page.locator(sel).first().waitFor({ state: 'visible', timeout: 8000 })
    .catch(() => { throw new Error(`${what || sel} is not visible`); });
}

async function playing(page, min = 1) {
  await page.waitForFunction(m => { const v = document.getElementById('video'); return v && !v.paused && v.currentTime > m; }, min, { timeout: 15000 })
    .catch(() => { throw new Error('the media element is not playing'); });
}

// A screenshot for the record, taken once the view transition has settled.
async function snap(page, name) {
  await page.waitForTimeout(450);
  await page.screenshot({ path: join(out, name) });
}

async function walk(page) {
  page.on('pageerror', e => jsErrors.push('pageerror: ' + e.message));
  page.on('console', m => {
    if (m.type() !== 'error') return;
    const t = m.text();
    if (/Failed to load resource/.test(t)) return; // artwork the fixture never wrote
    jsErrors.push('console: ' + t);
  });
  page.on('response', r => {
    if (r.status() >= 500) jsErrors.push(`HTTP ${r.status()} ${r.url()}`);
  });

  await step(page, 'boot: the gate asks who is watching', async () => {
    await page.goto(base + '/');
    await expectVisible(page, '#gate', 'the profile gate');
    await page.fill('#gate-name', 'smoke');
    await page.click('#gate-create');
    await page.waitForFunction(() => document.getElementById('gate').hidden);
    const chip = await page.textContent('#profile-chip');
    if (!/smoke/.test(chip)) throw new Error('chip reads ' + chip);
  });

  await step(page, 'home: a hero and headed band rows', async () => {
    await expectVisible(page, '#hero', 'the hero');
    await page.waitForFunction(() => document.querySelectorAll('section.band').length >= 3);
    const heads = await page.$$eval('section.band > h3', hs => hs.map(h => h.textContent.trim()));
    if (!heads.some(h => /TV|Shows/i.test(h)) || !heads.some(h => /Movies|Films/i.test(h))) throw new Error('bands: ' + heads.join(', '));
    if (!(await page.$('.card'))) throw new Error('no tiles');
    const tech = await page.$$eval('.card .c-tech', els => els.length);
    if (tech) throw new Error('tiles still carry a tech line');
    await snap(page, 'home.png');
  });

  await step(page, 'settings: gear opens the panel, ten-foot toggles', async () => {
    await page.click('#gear');
    await expectVisible(page, '#settings', 'the settings panel');
    await page.click('#settings-tenfoot');
    await page.waitForFunction(() => document.body.hasAttribute('data-ten-foot'));
    await page.click('#settings-tenfoot');
    await page.waitForFunction(() => !document.body.hasAttribute('data-ten-foot'));
    // The household dashboard fills itself from the root's activity link the
    // moment the panel is up; nothing is playing, so it says so.
    await page.waitForFunction(
      () => /\S/.test(document.getElementById('settings-activity').textContent));
    // The subtitle look: picking a size writes the ::cue rule the page carries
    // and remembers it, which is the whole of that setting's server side.
    await page.selectOption('#cue-size', 'large');
    await page.waitForFunction(
      () => /font-size: 140%/.test(document.getElementById('cue-style').textContent));
    await page.selectOption('#cue-size', 'medium');
    await page.click('#settings-close');
    await page.waitForFunction(() => document.getElementById('settings').hidden);
  });

  await step(page, 'show: seasons, next up, an episode plays with keys and a clip', async () => {
    await page.locator('.card', { hasText: 'The Office' }).first().click();
    await expectVisible(page, '#next-up', 'the Next up card');
    await expectVisible(page, 'details.season', 'a season');
    await snap(page, 'show.png');
    await page.locator('details.season .item').first().click();
    await expectVisible(page, '#detail-play', 'the play control');
    await page.click('#detail-play');
    await expectVisible(page, '#player-chrome', 'the player chrome');
    await playing(page);
    if (!(await page.evaluate(() => document.body.classList.contains('theatre')))) throw new Error('no theatre mode');
    await snap(page, 'player.png');
    await page.keyboard.press('m');
    await page.waitForFunction(() => document.getElementById('video').muted);
    await page.keyboard.press('Space');
    await page.waitForFunction(() => document.getElementById('video').paused);
    await page.keyboard.press('Space');
    await page.waitForFunction(() => !document.getElementById('video').paused);
    await page.click('#btn-clip');
    await expectVisible(page, '#clipbar', 'the clip bar');
    await page.waitForFunction(() => (document.getElementById('clip-sentence').textContent || '').trim().length > 0, null, { timeout: 8000 })
      .catch(() => { throw new Error('the clip never got its sentence'); });
    await page.click('#btn-clip-done');
    await page.waitForFunction(() => { const v = document.getElementById('video'); return v.ended || document.querySelector('#upnext:not([hidden])'); }, null, { timeout: 15000 });
    await expectVisible(page, '#upnext', 'up next');
    await page.click('#upnext-cancel');
    await page.click('#detail-back');
    await page.waitForFunction(() => document.getElementById('stage').hidden);
  });

  // The one context action a library is asked for most. The row's tick is a
  // control inside a control, so what this proves is that pressing it MARKS
  // the row rather than opening it — and that the pane comes back saying so.
  await step(page, 'watched: the tick marks an episode off, and back on', async () => {
    await page.goto(base + '/#/');
    await page.locator('.card', { hasText: 'The Office' }).first().click();
    await expectVisible(page, 'details.season .item', 'an episode row');
    const ticked = () => page.$$eval('details.season .item',
      rows => rows.filter(r => r.classList.contains('watched')).length);
    const before = await ticked();
    await page.locator('details.season .item [data-act="watched"]').first().click();
    await page.waitForFunction(n => document.querySelectorAll('details.season .item.watched').length !== n,
      before, { timeout: 8000 }).catch(() => { throw new Error('the tick did not move'); });
    const after = await ticked();
    await page.locator('details.season .item [data-act="watched"]').first().click();
    await page.waitForFunction(n => document.querySelectorAll('details.season .item.watched').length !== n,
      after, { timeout: 8000 }).catch(() => { throw new Error('the tick did not come back off'); });
    if (await ticked() !== before) throw new Error('the mark did not undo cleanly');
  });

  await step(page, 'film: backdrop, facts, cast, and Escape leaves the player', async () => {
    await page.goto(base + '/#/');
    await page.locator('.card', { hasText: 'Frozen' }).first().click();
    await expectVisible(page, '#detail-play', 'the play control');
    const meta = await page.textContent('#detail-meta');
    if (!/PG/.test(meta) || !/1h 42m|102/.test(meta)) throw new Error('meta reads ' + meta);
    const cast = await page.textContent('#detail-cast');
    if (!/Kristen Bell/.test(cast)) throw new Error('cast reads ' + cast);
    if (!(await page.$('#detail-details'))) throw new Error('no Details disclosure');
    await page.click('#detail-play');
    await playing(page);
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => document.getElementById('stage').hidden, null, { timeout: 8000 })
      .catch(() => { throw new Error('Escape did not leave the player'); });
  });

  await step(page, 'music: a track keeps playing under the mini-player while browsing', async () => {
    await page.goto(base + '/#/');
    await page.locator('.card', { hasText: 'Radiohead' }).first().click();
    await expectVisible(page, '#artist-albums .card', 'the artist shelf');
    await page.locator('#artist-albums .card').first().click();
    await expectVisible(page, '#detail-play', 'the record play control');
    await page.click('#detail-play');
    await playing(page, 0.5);
    if (!(await page.evaluate(() => document.body.classList.contains('audio-mode')))) throw new Error('no audio mode');
    await page.click('#detail-back');
    await page.waitForFunction(() => !document.getElementById('mini-player').hidden, null, { timeout: 8000 })
      .catch(() => { throw new Error('no mini-player after leaving'); });
    const t1 = await page.evaluate(() => document.getElementById('video').currentTime);
    await page.waitForTimeout(700);
    const t2 = await page.evaluate(() => document.getElementById('video').currentTime);
    if (!(t2 > t1)) throw new Error(`playback stopped on leaving (${t1} -> ${t2})`);
    await page.click('#mini-bar');
    await expectVisible(page, '#player-chrome', 'the full chrome back');
    await page.click('#btn-stop-session');
    await page.waitForFunction(() => document.getElementById('stage').hidden);
  });

  await step(page, 'book: an EPUB opens in the reader and turns a page', async () => {
    await page.goto(base + '/#/');
    await page.locator('.card', { hasText: 'Hill House' }).first().click();
    await expectVisible(page, '#detail-play', 'the read control');
    await page.click('#detail-play');
    await expectVisible(page, '#reader-pane', 'the reader pane');
    await page.locator('.rd-view iframe').first().waitFor({ timeout: 15000 })
      .catch(() => { throw new Error('epub.js never rendered a page'); });
    await page.click('.rd-next');
    await page.waitForTimeout(500);
    await page.click('.rd-close');
    await page.waitForFunction(() => document.getElementById('stage').hidden);
  });

  await step(page, 'pdf: a PDF opens on a canvas and turns a page', async () => {
    await page.goto(base + '/#/');
    await page.locator('.card', { hasText: 'Flatland' }).first().click();
    await expectVisible(page, '#detail-play', 'the read control');
    await page.click('#detail-play');
    await page.locator('.rd-canvas').first().waitFor({ timeout: 15000 })
      .catch(() => { throw new Error('pdf.js never drew a page'); });
    await page.click('.rd-next');
    await page.waitForTimeout(500);
    await page.click('.rd-close');
    await page.waitForFunction(() => document.getElementById('stage').hidden);
  });

  await step(page, 'search: Enter opens a results route that finds an episode', async () => {
    await page.goto(base + '/#/');
    await page.fill('#search', 'beach');
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => location.hash.startsWith('#/search/'));
    await expectVisible(page, '#search-title', 'the results heading');
    await page.waitForFunction(() => /Beach Games/.test(document.getElementById('view').textContent));
  });

  await step(page, 'phone: nothing overflows sideways at 390px', async () => {
    await page.setViewportSize({ width: 390, height: 844 });
    for (const hash of ['#/', '#/show/The%20Office', '#/item/4']) {
      await page.goto(base + '/' + hash);
      await page.waitForFunction(() => document.querySelector('.card, #detail-play'));
      await page.waitForTimeout(300);
      const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      if (over > 1) throw new Error(`${hash} is ${over}px wider than the phone`);
    }
    await page.goto(base + '/#/');
    await page.waitForFunction(() => document.querySelector('.card'));
    await page.waitForTimeout(450);
    await page.screenshot({ path: join(out, 'phone.png'), fullPage: true });
    await page.setViewportSize({ width: 1280, height: 800 });
  });

  await step(page, 'kids: a kid profile sees the PG film and not the TV-14 show', async () => {
    await page.goto(base + '/#/');
    await page.fill('#search', ''); // the last step's query still filters the shelf
    await page.click('#profile-chip');
    await expectVisible(page, '#gate', 'the gate');
    await page.fill('#gate-name', 'junior');
    await page.check('#gate-kid-check');
    // The shelf is re-read for the new profile: wait for that answer, not
    // for whatever cards the last view left on the page.
    const shelf = page.waitForResponse(r => r.url().includes('/api/library'));
    await page.click('#gate-create');
    await page.waitForFunction(() => document.getElementById('gate').hidden);
    await shelf;
    // ...and for the swap that draws it: a view transition lands a frame later.
    await page.waitForTimeout(400);
    await page.waitForFunction(() => document.querySelectorAll('section.band .card').length > 0);
    const titles = await page.$$eval('.card .c-title', els => els.map(e => e.textContent.trim()));
    if (!titles.includes('Frozen')) throw new Error('Frozen missing for a kid: ' + titles.join(', '));
    if (titles.includes('The Office')) throw new Error('The Office shown to a kid');
  });
}

// --- main ----------------------------------------------------------------------

let server = null;
try {
  console.log('smoke: media into ' + media);
  writeWav(join(media, 'track.wav'), 6);
  await recordWebm(join(media, 'film.webm'), 6);
  console.log('smoke: building the server');
  const bin = buildServer();
  server = await startServer(bin);
  console.log('smoke: server on ' + base);
  const browser = await chromium.launch({ args: ['--autoplay-policy=no-user-gesture-required'] });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  await walk(page);
  await browser.close();
} catch (e) {
  failures.push({ name: 'harness', error: String(e && e.stack || e) });
} finally {
  if (server) server.child.kill('SIGTERM');
}

console.log('');
if (jsErrors.length) {
  console.log('JavaScript errors (' + jsErrors.length + '):');
  for (const e of jsErrors) console.log('  ' + e);
}
if (failures.length) {
  console.log('Failures (' + failures.length + '):');
  for (const f of failures) console.log('  ' + f.name + '\n    ' + f.error.split('\n').join('\n    ') + (f.shot ? '\n    screenshot: ' + f.shot : ''));
  process.exit(1);
}
if (jsErrors.length) process.exit(1);
console.log('browser smoke: all steps passed, no JavaScript errors');
