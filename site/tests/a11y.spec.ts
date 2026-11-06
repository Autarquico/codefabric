import AxeBuilder from '@axe-core/playwright';
import { expect, test, type Page } from '@playwright/test';

const PAGES = [
  { name: 'landing', path: '/codefabric/' },
  { name: 'getting-started', path: '/codefabric/getting-started/' },
];
const THEMES = ['light', 'dark'] as const;
const VIEWPORTS = [
  { name: '360px', width: 360, height: 780 },
  { name: 'desktop', width: 1280, height: 800 },
];
// Starlight's own search button reads "Search" to assistive tech and "Search Ctrl K" on screen (its kbd hint).
// That is upstream markup we do not override; every other node of that rule still fails the test.
const STARLIGHT_SEARCH_BUTTON = 'button[data-open-modal=""]';
const WCAG_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa'];

async function open(page: Page, path: string, theme: (typeof THEMES)[number], width: number, height: number) {
  await page.setViewportSize({ width, height });
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((value) => localStorage.setItem('starlight-theme', value), theme);
  await page.goto(path, { waitUntil: 'networkidle' });
}

for (const { name, path } of PAGES) {
  for (const theme of THEMES) {
    for (const viewport of VIEWPORTS) {
      test.describe(`${name} · ${theme} · ${viewport.name}`, () => {
        test.beforeEach(({ page }) => open(page, path, theme, viewport.width, viewport.height));

        test('there is no horizontal scroll', async ({ page }) => {
          const { scrollWidth, clientWidth } = await page.evaluate(() => ({
            scrollWidth: document.documentElement.scrollWidth,
            clientWidth: document.documentElement.clientWidth,
          }));
          expect(scrollWidth).toBeLessThanOrEqual(clientWidth);
        });

        test('no serious or critical axe violation', async ({ page }) => {
          const { violations } = await new AxeBuilder({ page }).withTags(WCAG_TAGS).analyze();
          const blocking = violations
            .filter(({ impact }) => impact === 'serious' || impact === 'critical')
            .map((violation) => ({
              ...violation,
              nodes: violation.nodes.filter(
                (node) => !(violation.id === 'label-content-name-mismatch' && node.target.join(' ') === STARLIGHT_SEARCH_BUTTON),
              ),
            }))
            .filter(({ nodes }) => nodes.length > 0);
          expect(
            blocking.map(({ id, impact, nodes }) => `${impact} ${id}: ${nodes.map((n) => n.target.join(' ')).join(' | ')}`),
          ).toEqual([]);
        });
      });
    }
  }
}

test('no page loads a resource from another origin', async ({ page, baseURL }) => {
  const foreign: string[] = [];
  page.on('request', (request) => {
    const url = request.url();
    if (!url.startsWith('data:') && !url.startsWith('blob:') && !url.startsWith(baseURL!)) foreign.push(url);
  });
  for (const { path } of PAGES) await page.goto(path, { waitUntil: 'networkidle' });
  expect(foreign).toEqual([]);
});
