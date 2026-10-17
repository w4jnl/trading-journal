// Screenshots of the seeded demo instance for the README (assets/readme/*.png).
//
//   scripts/demo/seed.sh up && (cd scripts/demo && npm install && node shots.mjs)
//
// Pages are read with the proxy-auth header the demo config trusts; the theme comes from the
// `?theme=` parameter the app's pre-paint script honours. Chrome (the installed browser, via
// Playwright's `channel: 'chrome'`) at 1440x900 CSS px and two device pixels per CSS px.
import { chromium } from 'playwright';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { statSync } from 'node:fs';

const here = dirname(fileURLToPath(import.meta.url));
const out = join(here, '..', '..', 'assets', 'readme');
const base = process.env.TJ_DEMO_URL || 'http://127.0.0.1:18777';
const scale = Number(process.env.TJ_SHOT_SCALE || 2);

const browser = await chromium.launch({ channel: 'chrome' });

async function shoot(name, path, theme, prepare) {
  const ctx = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: scale,
    colorScheme: theme,
    extraHTTPHeaders: { 'Remote-User': 'demo', 'Remote-Groups': 'admin' },
  });
  const page = await ctx.newPage();
  const sep = path.includes('?') ? '&' : '?';
  await page.goto(`${base}${path}${sep}theme=${theme}`, { waitUntil: 'networkidle' });
  await page.evaluate(() => document.fonts.ready);
  if (prepare) await prepare(page);
  await page.waitForTimeout(400);
  const file = join(out, `${name}.png`);
  await page.screenshot({ path: file });
  const kb = Math.round(statSync(file).size / 1024);
  console.log(`${name}.png ${kb} KB${kb > 700 ? '  (over 700 KB: re-run with TJ_SHOT_SCALE=1.5)' : ''}`);
  await ctx.close();
}

// the sheet opens from a row: a closed trade with a good note. The replay chart is left
// unloaded: the invented fills would float beside the real bars
async function openNotedTrade(page) {
  const rows = page.locator('tr[data-trade-id]:not([data-trade-id^="pos:"])');
  const noted = rows.filter({ has: page.locator('.icon-journal.icon-good') });
  await ((await noted.count()) ? noted.first() : rows.first()).click();
  await page.locator('#trade-sheet, [data-sheet], .sheet').first().waitFor({ state: 'visible', timeout: 10000 }).catch(() => {});
  await page.waitForLoadState('networkidle');
  await page.waitForTimeout(1500);
}

await shoot('dashboard-dark', '/dashboard', 'dark');
await shoot('trades-sheet-dark', '/trades', 'dark', openNotedTrade);
await shoot('journal-light', '/journal', 'light');
await shoot('calendar-light', '/calendar', 'light');
await shoot('analytics-dark', '/analytics', 'dark');
await shoot('day-dark', `/journal/${process.env.TJ_DEMO_DAY || '2026-10-15'}`, 'dark');

await browser.close();
