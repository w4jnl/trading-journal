// The README recording (assets/readme/demo.gif): dashboard, the trade log, a trade's sheet with
// its note being edited, the calendar. Twenty-odd seconds in the dark theme.
//
//   scripts/demo/seed.sh up && (cd scripts/demo && npm install && node demo.mjs)
//
// Playwright records a WebM; ffmpeg (installed separately) turns it into a 12 fps GIF with a
// 128-colour palette. Nothing is saved to the journal: the note is typed, not submitted.
import { chromium } from 'playwright';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { mkdtempSync, readdirSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { execFileSync } from 'node:child_process';

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, '..', '..', 'assets', 'readme', 'demo.gif');
const base = process.env.TJ_DEMO_URL || 'http://127.0.0.1:18777';
const size = { width: 1152, height: 720 };
const videoDir = mkdtempSync(join(tmpdir(), 'tj-demo-video-'));

const browser = await chromium.launch({ channel: 'chrome' });
const ctx = await browser.newContext({
  viewport: size,
  deviceScaleFactor: 1,
  colorScheme: 'dark',
  recordVideo: { dir: videoDir, size },
  extraHTTPHeaders: { 'Remote-User': 'demo', 'Remote-Groups': 'admin' },
});
const page = await ctx.newPage();
const pause = (ms) => page.waitForTimeout(ms);

await page.goto(`${base}/dashboard?theme=dark`, { waitUntil: 'networkidle' });
await page.evaluate(() => document.fonts.ready);
await pause(3200);
await page.mouse.wheel(0, 420);
await pause(2200);

await page.goto(`${base}/trades`, { waitUntil: 'networkidle' });
await pause(2200);
const rows = page.locator('tr[data-trade-id]:not([data-trade-id^="pos:"])');
const noted = rows.filter({ has: page.locator('.icon-journal.icon-good') });
const row = (await noted.count()) ? noted.first() : rows.first();
await row.hover();
await pause(700);
await row.click();
await page.waitForLoadState('networkidle');
await pause(2600);

// the note's editor, a line typed and left unsaved
const edit = page.locator('#trade-sheet-content [data-journal-edit]').first();
if (await edit.count()) {
  await edit.click();
  const ta = page.locator('#trade-sheet-content textarea[data-je-input]').first();
  await ta.waitFor({ state: 'visible', timeout: 5000 }).catch(() => {});
  if (await ta.count()) {
    await ta.click();
    // keep the note's heading in view while typing, not just the top of a tall textarea
    const showHeading = (by) => page.evaluate((by) => {
      for (const sel of ['#trade-sheet-content', '#trade-sheet-backdrop', '.trade-sheet', '.trade-sheet__body']) {
        const el = document.querySelector(sel);
        if (el && el.scrollHeight > el.clientHeight + 1) el.scrollBy(0, -by);
      }
      window.scrollBy(0, -by);
    }, by);
    await showHeading(130);
    await pause(500);
    await page.keyboard.press('End');
    await page.keyboard.press('Control+End');
    await page.keyboard.type('\n\nNext time: hold the runner to the open, the first target is for the first half.', { delay: 28 });
    await showHeading(300); // back up to the tag, strategy and label chips above the text
    await pause(2600);
  }
}

await page.goto(`${base}/calendar`, { waitUntil: 'networkidle' });
await pause(3400);

await ctx.close();
await browser.close();

const webm = readdirSync(videoDir).find((f) => f.endsWith('.webm'));
const filters = 'fps=10,scale=1152:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle';
execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-ss', '0.6', '-i', join(videoDir, webm), '-vf', filters, out]);
rmSync(videoDir, { recursive: true, force: true });
console.log(`demo.gif ${Math.round(statSync(out).size / 1024)} KB`);
