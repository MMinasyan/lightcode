import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = dirname(fileURLToPath(import.meta.url));
// An optional first argument names the models file to normalize, resolved
// from the caller's working directory (the target build hook runs from its
// project's build/bin). Without one, the shipped root models file is used.
const files = [
  process.argv[2] ? resolve(process.cwd(), process.argv[2]) : join(scriptDir, '..', 'wailsjs', 'go', 'models.ts'),
];

for (const file of files) {
  const original = readFileSync(file, 'utf8');
  const normalized = original.replace(/[ \t]+$/gm, '').replace(/\n*$/u, '\n');
  if (normalized !== original) {
    writeFileSync(file, normalized);
  }
}
