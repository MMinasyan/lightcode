import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
const files = [join(scriptDir, '..', 'wailsjs', 'go', 'models.ts')];

for (const file of files) {
  const original = readFileSync(file, 'utf8');
  const normalized = original.replace(/[ \t]+$/gm, '').replace(/\n*$/u, '\n');
  if (normalized !== original) {
    writeFileSync(file, normalized);
  }
}
