// TUI frames come from the golden files the TUI test suite renders; the site never invents one.
import { readFileSync } from 'node:fs';
import path from 'node:path';

export const GOLDEN_DIR = 'internal/tui/testdata';

// Control characters would run in the reader's terminal if copied; show the escape byte instead.
export function sanitizeGolden(text) {
  return text
    .replace(/\r\n?/g, '\n')
    .replace(/\u001b/g, '␛')
    .replace(/[\u0000-\u0008\u000b-\u001f\u007f]/g, '')
    .replace(/\s+$/, '');
}

export function readGolden(root, file) {
  const full = path.join(root, GOLDEN_DIR, file);
  try {
    return sanitizeGolden(readFileSync(full, 'utf8'));
  } catch (error) {
    if (error.code === 'ENOENT') throw new Error(`golden frame ${GOLDEN_DIR}/${file} not found under ${root}`);
    throw error;
  }
}
