import assert from 'node:assert/strict';
import { test } from 'node:test';
import { installProblems } from './check-install.mjs';
import { INSTALL_URL, UNRELEASED_NOTE, installCommands, siteVersion } from './version.mjs';

test('siteVersion reads a release tag, treats empty as unreleased and rejects anything else', () => {
  assert.equal(siteVersion({ SITE_VERSION: 'v0.2.0' }), 'v0.2.0');
  assert.equal(siteVersion({ SITE_VERSION: ' v0.2.0-rc.1 ' }), 'v0.2.0-rc.1');
  assert.equal(siteVersion({ SITE_VERSION: '' }), null);
  assert.equal(siteVersion({}), null);
  for (const bad of ['0.2.0', 'main', 'v1.2', 'v1.2.3; rm -rf /', 'v1.2.3 sh']) {
    assert.throws(() => siteVersion({ SITE_VERSION: bad }), /not a release tag/, bad);
  }
});

test('the one-liner is the contract with the release workflow', () => {
  assert.equal(installCommands('v0.2.0').latest, 'curl -fsSL https://github.com/Autarquico/codefabric/releases/latest/download/install.sh | sh');
  assert.equal(INSTALL_URL.endsWith('/releases/latest/download/install.sh'), true);
});

test('the pinned command carries the version, or a placeholder for unreleased builds', () => {
  assert.match(installCommands('v0.2.0').pinned, /\| CF_VERSION=v0\.2\.0 sh$/);
  assert.match(installCommands(null).pinned, /\| CF_VERSION=vX\.Y\.Z sh$/);
});

const page = (version, extra = '') => {
  const { latest, pinned } = installCommands(version);
  return `<pre><code>${latest.replace(/&/g, '&amp;')}</code></pre><pre><code>${pinned}</code></pre>${extra}`;
};

test('check-install accepts a page built for the version', () => {
  assert.deepEqual(installProblems(page('v0.2.0'), 'v0.2.0'), []);
  assert.deepEqual(installProblems(page(null, `<p>${UNRELEASED_NOTE}</p>`), null), []);
});

test('check-install rejects a page that carries another version or none', () => {
  assert.match(installProblems(page('v0.1.0'), 'v0.2.0').join('\n'), /pinned command is missing/);
  assert.match(installProblems(page('v0.2.0') + 'CF_VERSION=v0.1.0 sh', 'v0.2.0').join('\n'), /instead of v0\.2\.0/);
  assert.match(installProblems('<p>nothing</p>', 'v0.2.0').join('\n'), /one-liner is missing/);
  assert.match(installProblems(page(null), null).join('\n'), /unreleased/);
});
