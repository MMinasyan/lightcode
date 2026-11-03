// Session chronology ordering against the ACTUAL producer shape: projectSession
// stores UTC time.Time values that encoding/json emits as RFC3339Nano with a
// variable, zero-trimmed fractional second. The tests pin plain-vs-fraction,
// .1-vs-.11, sub-millisecond and exact-tie ordering, plus the newest-root pick.
import { describe, expect, it } from 'vitest';
import { compareSessionActivity, normalizeInstant, sortOpenRoots } from './sessions';

type Session = { session_id: string; last_activity: string; parent_session_id?: string };

function s(session_id: string, last_activity: string, parent_session_id?: string): Session {
  return { session_id, last_activity, parent_session_id };
}

describe('normalizeInstant', () => {
  it('pads the fractional second to nine digits and leaves no-fraction equal to .000000000', () => {
    expect(normalizeInstant('2026-10-02T10:00:00Z')).toBe('2026-10-02T10:00:00.000000000Z');
    expect(normalizeInstant('2026-10-02T10:00:00.1Z')).toBe('2026-10-02T10:00:00.100000000Z');
    expect(normalizeInstant('2026-10-02T10:00:00.000000001Z')).toBe('2026-10-02T10:00:00.000000001Z');
  });

  it('compares a whole-second instant before its fractional sibling', () => {
    // The exact-zero instant is EARLIER than 100ms, so descending order places
    // the fractional one first — the naive string compare would get this wrong.
    expect(normalizeInstant('2026-10-02T10:00:00Z') < normalizeInstant('2026-10-02T10:00:00.5Z')).toBe(true);
    // Raw strings order the opposite way, proving the normalization matters.
    expect('2026-10-02T10:00:00Z' > '2026-10-02T10:00:00.5Z').toBe(true);
  });
});

describe('compareSessionActivity', () => {
  it('orders plain versus fractional stamps chronologically (descending)', () => {
    const whole = s('a', '2026-10-02T10:00:00Z');
    const frac = s('b', '2026-10-02T10:00:00.5Z');
    // frac is later → first in descending order.
    expect(compareSessionActivity(whole, frac)).toBe(1);
    expect(compareSessionActivity(frac, whole)).toBe(-1);
  });

  it('distinguishes .1 from .11 (100ms vs 110ms)', () => {
    const tenth = s('a', '2026-10-02T10:00:00.1Z');
    const eleven = s('b', '2026-10-02T10:00:00.11Z');
    expect(compareSessionActivity(tenth, eleven)).toBe(1);
    expect(compareSessionActivity(eleven, tenth)).toBe(-1);
  });

  it('distinguishes sub-millisecond nanosecond fractions', () => {
    const nano1 = s('a', '2026-10-02T10:00:00.000000001Z');
    const micro = s('b', '2026-10-02T10:00:00.000001Z');
    expect(compareSessionActivity(nano1, micro)).toBe(1);
  });

  it('breaks an exact-tied instant by ascending Session ID', () => {
    const a = s('aaa', '2026-10-02T10:00:00.100Z');
    const b = s('bbb', '2026-10-02T10:00:00.1Z'); // same instant, different spelling
    expect(compareSessionActivity(a, b)).toBe(-1);
    expect(compareSessionActivity(b, a)).toBe(1);
  });
});

describe('sortOpenRoots', () => {
  it('excludes child headers and picks the newest root first', () => {
    const roots = sortOpenRoots([
      s('kid', '2026-10-05T00:00:00Z', 'parent'),
      s('old', '2026-10-01T10:00:00Z'),
      s('new', '2026-10-02T10:00:00.5Z'),
      s('whole', '2026-10-02T10:00:00Z'),
    ]);
    // new (.5 of the 2nd) is the latest; whole is earlier the same second.
    expect(roots.map((r) => r.session_id)).toEqual(['new', 'whole', 'old']);
  });
});
