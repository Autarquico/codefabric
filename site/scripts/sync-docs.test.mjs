import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { GUIDE_PAGES, extractTitle, syncDocs, transformPage } from './sync-docs.mjs';

const known = new Set(['docs/guide/cli.md', 'docs/guide/tui.md', 'docs/guide/concepts.md', 'docs/guide/getting-started.md', 'docs/codefabric-spec.md', 'internal/tui', 'README.md']);
const kindOf = (p) => (!known.has(p) ? null : p === 'internal/tui' ? 'dir' : 'file');
const page = (source, file = 'concepts.md') => transformPage({ file, source, base: '/codefabric/', ref: 'v0.2.0', kindOf });

test('the first "# " heading becomes the title and later ones are demoted; code is left alone', () => {
  const { title, body } = extractTitle('# The `cf` CLI\n\n```sh\n# comment\n```\n\n# Again\n', 'x.md');
  assert.equal(title, 'The cf CLI');
  assert.match(body, /^```sh\n# comment\n```/m);
  assert.match(body, /^## Again$/m);
  assert.throws(() => extractTitle('no heading', 'x.md'), /no top-level/);
});

test('links between guide pages become site routes, others point at the tag', () => {
  const out = page('# T\nSee [CLI](cli.md#flags), [TUI](./tui.md), [tui source](../../internal/tui) and [home](https://example.com).');
  assert.match(out, /\]\(\/codefabric\/cli\/#flags\)/);
  assert.match(out, /\]\(\/codefabric\/tui\/\)/);
  assert.match(out, /\]\(https:\/\/github\.com\/Autarquico\/codefabric\/tree\/v0\.2\.0\/internal\/tui\)/);
  assert.match(out, /\]\(https:\/\/example\.com\)/);
});

test('a link to something that is never published, or does not exist, fails the sync', () => {
  assert.throws(() => page('# T\n[spec](../codefabric-spec.md)'), /not published/);
  assert.throws(() => page('# T\n[spec](../specs/x.md)'), /not published/);
  assert.throws(() => page('# T\n[gone](missing.md)'), /does not exist/);
  assert.throws(() => page('# T\n[up](../../../../etc/passwd)'), /leaves the repository/);
});

test('front matter in a guide page is refused and tables get a labelled scroll region', () => {
  assert.throws(() => page('---\ntitle: x\n---\n# A'), /front matter/);
  const out = page('# A\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nafter');
  assert.match(out, /<div class="table-scroll" tabindex="0" role="region" aria-label="Table">/);
  assert.match(out, /<\/div>\n\nafter/);
});

function fixture(files) {
  const root = mkdtempSync(path.join(tmpdir(), 'cf-sync-'));
  mkdirSync(path.join(root, 'docs', 'guide'), { recursive: true });
  writeFileSync(path.join(root, 'docs', 'codefabric-spec.md'), '# Spec\n');
  mkdirSync(path.join(root, 'docs', 'specs'), { recursive: true });
  writeFileSync(path.join(root, 'docs', 'specs', 'secret.md'), '# Secret\n');
  for (const f of files) writeFileSync(path.join(root, 'docs', 'guide', f), `# ${f}\n\nbody\n`);
  return { root, out: path.join(root, 'out') };
}

test('syncDocs publishes exactly the guide pages and nothing else from docs/', () => {
  const { root, out } = fixture(GUIDE_PAGES);
  writeFileSync(path.join(root, 'docs', 'guide', 'draft.md'), '# Draft\n');
  syncDocs({ root, out, ref: 'main' });
  assert.deepEqual(readdirSync(out).sort(), [...GUIDE_PAGES].sort());
  assert.match(readFileSync(path.join(out, 'cli.md'), 'utf8'), /^---\ntitle: "cli\.md"\n---/);
  assert.equal(existsSync(path.join(out, 'codefabric-spec.md')), false);
});

test('syncDocs fails clearly when a guide page is missing', () => {
  const { root, out } = fixture(GUIDE_PAGES.slice(1));
  assert.throws(() => syncDocs({ root, out }), /getting-started\.md is missing/);
});
