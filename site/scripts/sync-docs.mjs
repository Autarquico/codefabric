// Copies docs/guide/*.md into the Starlight content folder, adapting them for the website.
// docs/guide is the only source: nothing else under docs/ is ever published.
import { existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { siteVersion } from './version.mjs';

export const SITE = 'https://autarquico.github.io';
export const BASE = '/codefabric/';
export const REPO_URL = 'https://github.com/Autarquico/codefabric';
export const GUIDE_DIR = 'docs/guide';
export const GUIDE_PAGES = ['getting-started.md', 'concepts.md', 'cli.md', 'tui.md'];
const NOT_PUBLIC = [/^docs\/codefabric-spec\.md$/, /^docs\/specs(\/|$)/, /^docs\/adr(\/|$)/, /^docs\/runbooks(\/|$)/];

// Astro bundles TuiFrame (and this module) into dist/.prerender, so the module's own location is not the
// site directory; every npm script runs from site/, which is.
export const siteRoot = process.cwd();
// The content (guide and golden frames) can come from another checkout, e.g. a release tag.
export const contentRoot = path.resolve(process.env.CF_CONTENT_ROOT || path.join(siteRoot, '..'));
const outputDir = path.join(siteRoot, 'src', 'content', 'docs');

// Links to files that are not site pages point at the tag the site is built from, or main for dev builds.
export function repoRef(env = process.env) {
  return siteVersion(env) ?? 'main';
}

export const slugOf = (file) => file.replace(/\.md$/, '');
export const routeFor = (slug, base = BASE) => `${base}${slug}/`;

function fenceOpening(line) {
  const match = line.match(/^ {0,3}(`{3,}|~{3,})/);
  return match ? match[1] : null;
}

// visit(line, inCode) returns the new line, or null to drop it.
function mapLines(markdown, visit) {
  let fence = null;
  return markdown
    .split('\n')
    .map((line) => {
      const marker = fenceOpening(line);
      if (fence) {
        if (marker && marker[0] === fence[0] && marker.length >= fence.length) fence = null;
        return visit(line, true);
      }
      if (marker) {
        fence = marker;
        return visit(line, true);
      }
      return visit(line, false);
    })
    .filter((line) => line !== null)
    .join('\n');
}

function outsideInlineCode(line, transform) {
  return line
    .split(/(`+[^`]*`+)/)
    .map((part, index) => (index % 2 === 1 ? part : transform(part)))
    .join('');
}

// Guide pages carry Starlight front matter; title and description are required and passed through verbatim.
export function splitFrontMatter(markdown, file) {
  const match = markdown.match(/^---\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/);
  if (!match) throw new Error(`${file}: front matter with "title" and "description" is required`);
  for (const key of ['title', 'description']) {
    const present = new RegExp(`^${key}:[ \\t]*\\S`, 'm').test(match[1]);
    if (!present) throw new Error(`${file}: front matter is missing a non-empty "${key}"`);
  }
  return { frontMatter: match[0].replace(/\r?\n$/, ''), body: markdown.slice(match[0].length) };
}

// The page title is the front matter's, so any "# " heading in the body would be a second h1.
function demoteH1(markdown) {
  return mapLines(markdown, (line, inCode) => (!inCode && /^# +\S/.test(line) ? `#${line}` : line));
}

function rewriteDestination(destination, { file, base, ref, kindOf }) {
  if (destination.startsWith('#') || /^[a-z][a-z0-9+.-]*:/i.test(destination)) return destination;
  // "../concepts/" is the site-relative spelling the guide uses between its pages (each page is a directory there).
  const sibling = destination.match(/^\.\.\/([a-z0-9-]+)\/(#.*)?$/);
  if (sibling && GUIDE_PAGES.includes(`${sibling[1]}.md`)) return `${routeFor(sibling[1], base)}${sibling[2] ?? ''}`;
  const hashAt = destination.indexOf('#');
  const target = hashAt === -1 ? destination : destination.slice(0, hashAt);
  const hash = hashAt === -1 ? '' : destination.slice(hashAt);
  const repoPath = path.posix.normalize(path.posix.join(GUIDE_DIR, target));
  if (repoPath.startsWith('..')) throw new Error(`${GUIDE_DIR}/${file}: link "${destination}" leaves the repository`);
  if (NOT_PUBLIC.some((pattern) => pattern.test(repoPath))) {
    throw new Error(`${GUIDE_DIR}/${file}: link "${destination}" points to ${repoPath}, which is not published`);
  }
  const kind = kindOf(repoPath);
  if (kind === null) throw new Error(`${GUIDE_DIR}/${file}: link "${destination}" points to ${repoPath}, which does not exist`);
  const guideFile = repoPath.startsWith(`${GUIDE_DIR}/`) ? repoPath.slice(GUIDE_DIR.length + 1) : null;
  if (guideFile !== null && GUIDE_PAGES.includes(guideFile)) return `${routeFor(slugOf(guideFile), base)}${hash}`;
  return `${REPO_URL}/${kind === 'dir' ? 'tree' : 'blob'}/${ref}/${repoPath}${hash}`;
}

function rewriteLinks(markdown, context) {
  return mapLines(markdown, (line, inCode) => {
    if (inCode) return line;
    const definition = line.match(/^( {0,3}\[[^\]]+\]:\s*)(\S+)(.*)$/);
    if (definition) return `${definition[1]}${rewriteDestination(definition[2], context)}${definition[3]}`;
    return outsideInlineCode(line, (text) =>
      text.replace(/\]\(([^)\s<>]+)/g, (_, destination) => `](${rewriteDestination(destination, context)}`),
    );
  });
}

// A focusable, labelled scroll region keeps wide tables reachable for keyboard users.
function wrapTables(markdown) {
  const out = [];
  let inTable = false;
  mapLines(markdown, (line, inCode) => {
    const isRow = !inCode && /^ {0,3}\|/.test(line);
    if (isRow && !inTable) out.push('<div class="table-scroll" tabindex="0" role="region" aria-label="Table">', '');
    if (!isRow && inTable) out.push('', '</div>');
    inTable = isRow;
    out.push(line);
    return line;
  });
  if (inTable) out.push('', '</div>');
  return out.join('\n');
}

export function transformPage({ file, source, base = BASE, ref, kindOf }) {
  const { frontMatter, body } = splitFrontMatter(source, `${GUIDE_DIR}/${file}`);
  const rewritten = wrapTables(rewriteLinks(demoteH1(body), { file, base, ref, kindOf }));
  return `${frontMatter}\n${rewritten.replace(/^\n+/, '\n')}`;
}

function kindOnDisk(root) {
  return (repoPath) => {
    const absolute = path.join(root, repoPath);
    if (!existsSync(absolute)) return null;
    return statSync(absolute).isDirectory() ? 'dir' : 'file';
  };
}

export function syncDocs({ root = contentRoot, out = outputDir, base = BASE, ref = repoRef() } = {}) {
  const kindOf = kindOnDisk(root);
  const pages = GUIDE_PAGES.map((file) => {
    const sourcePath = path.join(root, GUIDE_DIR, file);
    if (!existsSync(sourcePath)) throw new Error(`${GUIDE_DIR}/${file} is missing under ${root}`);
    return { file, content: transformPage({ file, source: readFileSync(sourcePath, 'utf8'), base, ref, kindOf }) };
  });
  rmSync(out, { recursive: true, force: true });
  mkdirSync(out, { recursive: true });
  for (const { file, content } of pages) writeFileSync(path.join(out, file), content);
  return GUIDE_PAGES;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const files = syncDocs();
  console.log(`sync-docs: ${files.length} guide pages copied to site/src/content/docs`);
}
