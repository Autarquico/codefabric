import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { GUIDE_PAGES, splitFrontMatter, syncDocs, transformPage } from './sync-docs.mjs';

const known = new Set(['docs/guide/cli.md', 'docs/guide/tui.md', 'docs/guide/mcp.md', 'docs/guide/concepts.md', 'docs/guide/getting-started.md', 'docs/codefabric-spec.md', 'internal/tui', 'README.md']);
const kindOf = (p) => (!known.has(p) ? null : p === 'internal/tui' ? 'dir' : 'file');
const FM = '---\ntitle: T\ndescription: D\n---\n';
const page = (source, file = 'concepts.md') => transformPage({ file, source: source.startsWith('---') ? source : FM + source, base: '/codefabric/', ref: 'v0.2.0', kindOf });

test('front matter is required with title and description, and passed through verbatim', () => {
  const { frontMatter, body } = splitFrontMatter('---\ntitle: "A: b"\ndescription: D\n---\n\nText\n', 'x.md');
  assert.equal(frontMatter, '---\ntitle: "A: b"\ndescription: D\n---');
  assert.equal(body, '\nText\n');
  assert.throws(() => splitFrontMatter('# no front matter', 'x.md'), /front matter .* required/);
  assert.throws(() => splitFrontMatter('---\ntitle: A\n---\n', 'x.md'), /missing .*"description"/);
  assert.throws(() => splitFrontMatter('---\ndescription: D\n---\n', 'x.md'), /missing .*"title"/);
  assert.throws(() => splitFrontMatter('---\ntitle:\ndescription: D\n---\n', 'x.md'), /missing .*"title"/);
});

test('a body "# " heading is demoted (the title is the front matter\'s); code is left alone', () => {
  const out = page('# Again\n\n```sh\n# comment\n```\n');
  assert.match(out, /^## Again$/m);
  assert.match(out, /^# comment$/m);
});

test('links between guide pages become site routes, others point at the tag', () => {
  const out = page('# T\nSee [CLI](cli.md#flags), [TUI](./tui.md), [Concepts](../concepts/), [tui source](../../internal/tui) and [home](https://example.com).');
  assert.match(out, /\]\(\/codefabric\/cli\/#flags\)/);
  assert.match(out, /\]\(\/codefabric\/tui\/\)/);
  assert.match(out, /\]\(\/codefabric\/concepts\/\)/);
  assert.match(out, /\]\(https:\/\/github\.com\/Autarquico\/codefabric\/tree\/v0\.2\.0\/internal\/tui\)/);
  assert.match(out, /\]\(https:\/\/example\.com\)/);
});

test('a link to something that is never published, or does not exist, fails the sync', () => {
  assert.throws(() => page('# T\n[spec](../codefabric-spec.md)'), /not published/);
  assert.throws(() => page('# T\n[spec](../specs/x.md)'), /not published/);
  assert.throws(() => page('# T\n[gone](missing.md)'), /does not exist/);
  assert.throws(() => page('# T\n[up](../../../../etc/passwd)'), /leaves the repository/);
});

test('tables get a labelled scroll region', () => {
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
  for (const f of files) writeFileSync(path.join(root, 'docs', 'guide', f), `---\ntitle: ${f}\ndescription: d\n---\n\nbody\n`);
  return { root, out: path.join(root, 'out') };
}

test('syncDocs publishes exactly the guide pages and nothing else from docs/', () => {
  const { root, out } = fixture(GUIDE_PAGES);
  writeFileSync(path.join(root, 'docs', 'guide', 'draft.md'), FM + 'x\n');
  syncDocs({ root, out, ref: 'main' });
  assert.deepEqual(readdirSync(out).sort(), [...GUIDE_PAGES].sort());
  assert.match(readFileSync(path.join(out, 'cli.md'), 'utf8'), /^---\ntitle: cli\.md\ndescription: d\n---/);
  assert.equal(existsSync(path.join(out, 'codefabric-spec.md')), false);
});

test('syncDocs fails clearly when a guide page is missing', () => {
  const { root, out } = fixture(GUIDE_PAGES.slice(1));
  assert.throws(() => syncDocs({ root, out }), /getting-started\.md is missing/);
});
