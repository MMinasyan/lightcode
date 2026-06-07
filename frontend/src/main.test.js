import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

describe('Svelte entrypoint', () => {
  it('uses the Svelte 5 mount API instead of the legacy component constructor', () => {
    const main = readFileSync(resolve('src/main.js'), 'utf8');

    expect(main).toMatch(/import\s*\{[^}]*mount[^}]*\}\s*from\s*['"]svelte['"]/);
    expect(main).toMatch(/mount\s*\(\s*App\s*,/);
    expect(main).not.toMatch(/new\s+App\s*\(/);
  });
});
