import { describe, expect, it } from 'vitest';

import { errorText } from './errors.js';

describe('errorText', () => {
  it('returns a fallback for nullish errors', () => {
    expect(errorText(undefined)).toBe('unknown error');
    expect(errorText(null)).toBe('unknown error');
  });

  it('returns string errors unchanged', () => {
    expect(errorText('simple message')).toBe('simple message');
  });

  it('uses message fields from error-like objects', () => {
    expect(errorText(new Error('boom'))).toBe('boom');
    expect(errorText({ message: 'plain object message' })).toBe('plain object message');
  });

  it('falls back to toString or String conversion', () => {
    expect(errorText({ toString: () => 'custom text' })).toBe('custom text');
    expect(errorText(42)).toBe('42');
  });
});
