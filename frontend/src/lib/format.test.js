import { describe, expect, it } from 'vitest';

import { fmtTokens, groupByProvider } from './format.js';

describe('fmtTokens', () => {
  it('returns - when known is false', () => {
    expect(fmtTokens(42, false)).toBe('-');
    expect(fmtTokens(0, false)).toBe('-');
  });

  it('formats small numbers as-is', () => {
    expect(fmtTokens(0, true)).toBe('0');
    expect(fmtTokens(999, true)).toBe('999');
  });

  it('formats thousands with k suffix', () => {
    expect(fmtTokens(1000, true)).toBe('1k');
    expect(fmtTokens(1500, true)).toBe('1.5k');
    expect(fmtTokens(999999, true)).toBe('1000k');
  });

  it('formats millions with M suffix', () => {
    expect(fmtTokens(1000000, true)).toBe('1M');
    expect(fmtTokens(2500000, true)).toBe('2.5M');
  });

  it('formats billions with B suffix', () => {
    expect(fmtTokens(1000000000, true)).toBe('1B');
    expect(fmtTokens(1500000000, true)).toBe('1.5B');
  });
});

describe('groupByProvider', () => {
  it('returns empty array for empty input', () => {
    expect(groupByProvider([])).toEqual([]);
  });

  it('groups single entry', () => {
    const result = groupByProvider([{ provider: 'openai', model: 'gpt-4' }]);
    expect(result).toHaveLength(1);
    expect(result[0].provider).toBe('openai');
    expect(result[0].providerName).toBe('openai');
    expect(result[0].providerHidden).toBe(false);
    expect(result[0].models).toHaveLength(1);
  });

  it('groups multiple entries by provider', () => {
    const result = groupByProvider([
      { provider: 'openai', model: 'gpt-4' },
      { provider: 'openai', model: 'gpt-3.5' },
      { provider: 'anthropic', model: 'claude' },
    ]);
    expect(result).toHaveLength(2);
    expect(result[0].provider).toBe('openai');
    expect(result[0].models).toHaveLength(2);
    expect(result[1].provider).toBe('anthropic');
    expect(result[1].models).toHaveLength(1);
  });

  it('preserves providerName and providerHidden', () => {
    const result = groupByProvider([
      { provider: 'test', providerName: 'Test Provider', providerHidden: true, model: 'm1' },
    ]);
    expect(result[0].providerName).toBe('Test Provider');
    expect(result[0].providerHidden).toBe(true);
  });

  it('defaults provider to empty string when missing', () => {
    const result = groupByProvider([{ model: 'm1' }]);
    expect(result[0].provider).toBe('');
    expect(result[0].providerName).toBe('');
  });
});
