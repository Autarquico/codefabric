import assert from 'node:assert/strict';
import { mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { GOLDEN_DIR, readGolden, sanitizeGolden } from './goldens.mjs';

test('sanitizeGolden shows escape bytes instead of passing them on and drops other control characters', () => {
  assert.equal(sanitizeGolden('rm -rf build/\u001b[2J\u0007\n'), 'rm -rf build/␛[2J');
});

test('sanitizeGolden keeps layout: inner spaces, blank lines, box drawing; trims the end', () => {
  const frame = ' ─── Inbox · 4 ───\n\n  › permission   auth\n\n\n';
  assert.equal(sanitizeGolden(frame), ' ─── Inbox · 4 ───\n\n  › permission   auth');
  assert.equal(sanitizeGolden('a\r\nb\r\n'), 'a\nb');
});

test('readGolden reads from the content root and fails clearly when the frame is missing', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'cf-goldens-'));
  mkdirSync(path.join(root, GOLDEN_DIR), { recursive: true });
  writeFileSync(path.join(root, GOLDEN_DIR, 'home.golden'), ' CODEFABRIC\n');
  assert.equal(readGolden(root, 'home.golden'), ' CODEFABRIC');
  assert.throws(() => readGolden(root, 'nope.golden'), /nope\.golden not found/);
});

test('a golden that is a symbolic link is refused', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'cf-goldens-'));
  mkdirSync(path.join(root, GOLDEN_DIR), { recursive: true });
  writeFileSync(path.join(root, 'secret.txt'), 'secret\n');
  symlinkSync(path.join(root, 'secret.txt'), path.join(root, GOLDEN_DIR, 'home.golden'));
  assert.throws(() => readGolden(root, 'home.golden'), /not a regular file/);
});
