// Fails the build when the landing page's install commands do not carry the version it was built for.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { UNRELEASED_NOTE, installCommands, siteVersion } from './version.mjs';

const decode = (html) =>
  html
    .replace(/<[^>]+>/g, '')
    .replace(/&amp;/g, '&')
    .replace(/&#38;/g, '&')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'");

export function installProblems(html, version) {
  const text = decode(html);
  const { latest, pinned } = installCommands(version);
  const problems = [];
  if (!text.includes(latest)) problems.push(`the one-liner is missing: ${latest}`);
  if (!text.includes(pinned)) problems.push(`the pinned command is missing: ${pinned}`);
  if (version === null && !text.includes(UNRELEASED_NOTE)) problems.push('a build without SITE_VERSION must say it is unreleased');
  if (version !== null) {
    const others = [...text.matchAll(/CF_VERSION=(\S+)/g)].map((m) => m[1]).filter((v) => v !== version);
    if (others.length > 0) problems.push(`CF_VERSION shows ${others.join(', ')} instead of ${version}`);
  }
  return problems;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const version = siteVersion();
  const problems = installProblems(readFileSync(process.argv[2] ?? 'dist/index.html', 'utf8'), version);
  if (problems.length > 0) {
    console.error(problems.map((p) => `check-install: ${p}`).join('\n'));
    process.exit(1);
  }
  console.log(`check-install: install commands carry ${version ?? 'the unreleased note'}`);
}
