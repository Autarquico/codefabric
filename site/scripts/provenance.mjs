// Decides which release tag the site may be built for. The tag must be a release tag, have a published
// release, and resolve to the very commit the triggering release.yml run was built from.
import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

export const RELEASE_TAG = /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$/;

/**
 * @param {{ eventName: string, tag: string, runSha?: string }} request
 * @param {{ release(tag: string): {draft: boolean} | null, tagSha(tag: string): string | null,
 *           successfulReleaseRuns(sha: string): {head_branch: string, head_sha: string}[] }} github
 * @returns {{ tag: string, sha: string }} throws Error with the reason when the tag is not acceptable
 */
export function verifyProvenance({ eventName, tag, runSha }, github) {
  if (!RELEASE_TAG.test(tag ?? '')) throw new Error(`not a release tag: '${tag}'`);
  const release = github.release(tag);
  if (!release) throw new Error(`no release for ${tag}`);
  if (release.draft !== false) throw new Error(`no published release for ${tag} (draft=${release.draft})`);
  const sha = github.tagSha(tag);
  if (!sha) throw new Error(`tag ${tag} does not resolve to a commit`);
  if (eventName === 'workflow_run') {
    if (sha !== runSha) throw new Error(`tag moved: ${tag} is at ${sha}, the release run built ${runSha}`);
  } else {
    const built = github.successfulReleaseRuns(sha).some((run) => run.head_branch === tag && run.head_sha === sha);
    if (!built) throw new Error(`no successful release.yml run for ${tag} at ${sha}`);
  }
  return { tag, sha };
}

function ghApi(path, args = []) {
  return JSON.parse(execFileSync('gh', ['api', path, ...args], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }));
}

function notFound(read) {
  try {
    return read();
  } catch (error) {
    if (/HTTP 404/.test(String(error.stderr ?? error.message))) return null;
    throw error;
  }
}

function githubOf(repository) {
  return {
    release: (tag) => notFound(() => ghApi(`repos/${repository}/releases/tags/${tag}`)),
    tagSha: (tag) => notFound(() => ghApi(`repos/${repository}/commits/refs/tags/${tag}`))?.sha ?? null,
    successfulReleaseRuns: (sha) =>
      ghApi(`repos/${repository}/actions/workflows/release.yml/runs?event=push&status=success&head_sha=${sha}`).workflow_runs,
  };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { EVENT_NAME, TAG, RUN_SHA, REPOSITORY, GITHUB_OUTPUT } = process.env;
  try {
    const { tag, sha } = verifyProvenance({ eventName: EVENT_NAME, tag: TAG, runSha: RUN_SHA }, githubOf(REPOSITORY));
    appendFileSync(GITHUB_OUTPUT, `tag=${tag}\nsha=${sha}\n`);
  } catch (error) {
    console.error(error.message);
    process.exit(1);
  }
}
