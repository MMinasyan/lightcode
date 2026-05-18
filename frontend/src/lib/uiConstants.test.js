import { describe, expect, it } from 'vitest';

import { collapsedPreviewLines, maxInlineExpandLines } from './uiConstants.js';

describe('UI constants', () => {
  it('exports the collapsed preview line count', () => {
    expect(collapsedPreviewLines).toBe(5);
    expect(typeof collapsedPreviewLines).toBe('number');
  });

  it('exports the inline expansion line cap', () => {
    expect(maxInlineExpandLines).toBe(50);
    expect(typeof maxInlineExpandLines).toBe('number');
  });
});
