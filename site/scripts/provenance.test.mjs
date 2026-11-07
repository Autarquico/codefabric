import assert from 'node:assert/strict';
import { test } from 'node:test';
import { verifyProvenance } from './provenance.mjs';

const A = 'a'.repeat(40);
const B = 'b'.repeat(40);

function github({ draft = false, release = { draft }, tags = { 'v1.2.3': A }, runs = [] } = {}) {
  return {
    release: () => release,
    tagSha: (tag) => tags[tag] ?? null,
    successfulReleaseRuns: (sha) => runs.filter((run) => run.head_sha === sha),
  };
}

test('workflow_run: the tag at the run commit is accepted', () => {
  assert.deepEqual(verifyProvenance({ eventName: 'workflow_run', tag: 'v1.2.3', runSha: A }, github()), { tag: 'v1.2.3', sha: A });
});

test('workflow_run: a tag moved after the release ran is rejected, even if its new commit has another successful run', () => {
  const runs = [{ head_branch: 'v9.9.9', head_sha: B }];
  assert.throws(
    () => verifyProvenance({ eventName: 'workflow_run', tag: 'v1.2.3', runSha: A }, github({ tags: { 'v1.2.3': B }, runs })),
    /tag moved/,
  );
});

test('workflow_dispatch: a successful run of another tag on the same commit is rejected', () => {
  const runs = [{ head_branch: 'v9.9.9', head_sha: A }];
  assert.throws(() => verifyProvenance({ eventName: 'workflow_dispatch', tag: 'v1.2.3' }, github({ runs })), /no successful release\.yml run/);
});

test('workflow_dispatch: a successful run of this tag at the tag commit is accepted', () => {
  const runs = [{ head_branch: 'v1.2.3', head_sha: A }];
  assert.deepEqual(verifyProvenance({ eventName: 'workflow_dispatch', tag: 'v1.2.3' }, github({ runs })), { tag: 'v1.2.3', sha: A });
});

test('workflow_dispatch: a moved tag has no run of its own at the new commit', () => {
  const runs = [{ head_branch: 'v1.2.3', head_sha: A }];
  assert.throws(() => verifyProvenance({ eventName: 'workflow_dispatch', tag: 'v1.2.3' }, github({ tags: { 'v1.2.3': B }, runs })), /no successful/);
});

test('a draft release is rejected', () => {
  assert.throws(() => verifyProvenance({ eventName: 'workflow_run', tag: 'v1.2.3', runSha: A }, github({ draft: true })), /draft=true/);
});

test('a tag without a release is rejected', () => {
  assert.throws(() => verifyProvenance({ eventName: 'workflow_run', tag: 'v1.2.3', runSha: A }, github({ release: null })), /no release/);
});

test('a non-release tag is rejected before any lookup', () => {
  const never = { release: () => assert.fail('looked up'), tagSha: () => assert.fail('looked up'), successfulReleaseRuns: () => [] };
  for (const tag of ['main', 'release-1', '1.2.3', 'v1.2', 'v1.2.3+build', '', undefined]) {
    assert.throws(() => verifyProvenance({ eventName: 'workflow_run', tag, runSha: A }, never), /not a release tag/);
  }
});
