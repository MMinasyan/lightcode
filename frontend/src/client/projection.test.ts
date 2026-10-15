// Corpus-driven scalar projection checks shared with the Go client view. The
// shared corpus stores every payload as raw JSON text; the typescript_only
// section carries raw JSON numbers that fail the generated Go decoders (a
// number is not a string) and are rejected here by the runtime typeof-string
// check.
import { readFileSync } from 'node:fs';

import { describe, expect, it } from 'vitest';

import type {
  ConfigurationRevision,
  ConversationItem,
  Hydration,
  SessionRevision,
  WarningsRevision,
} from '../generated/protocol/types.gen';
import { acceptHydration, compareSessionRevision, compareConfigurationRevision, compareWarningsRevision, itemID, mergeConversation, validateRevisionHint } from './projection';

const COMPARE_RESULTS: Record<string, number> = { older: -1, equal: 0, newer: 1 };

const corpus = JSON.parse(
  readFileSync(new URL('../../../protocol/testdata/client_projection_cases.json', import.meta.url), 'utf8'),
);

describe('independent counter comparators', () => {
  for (const test of corpus.counters) {
    it(test.name, () => {
      const instance = '0123456789abcdef0123456789abcdef';
      const right = test.foreign ? 'ffffffffffffffffffffffffffffffff' : instance;
      const config = () => compareConfigurationRevision({ instance_id: instance, generation: test.a }, { instance_id: right, generation: test.b });
      const warnings = () => compareWarningsRevision({ instance_id: instance, revision: test.a }, { instance_id: right, revision: test.b });
      if (test.error) { expect(config).toThrow(); expect(warnings).toThrow(); }
      else { expect(config()).toBe(COMPARE_RESULTS[test.result]); expect(warnings()).toBe(COMPARE_RESULTS[test.result]); }
    });
  }
});

function parseHydration(raw: string): Hydration {
  return JSON.parse(raw) as Hydration;
}

it('loads every shared corpus section', () => {
  expect(corpus.counters.length).toBeGreaterThan(0);
  expect(corpus.compare.length).toBeGreaterThan(0);
  expect(corpus.accept.length).toBeGreaterThan(0);
  expect(corpus.item_ids.length).toBeGreaterThan(0);
  expect(corpus.merge.length).toBeGreaterThan(0);
  expect(corpus.typescript_only.length).toBeGreaterThan(0);
});

describe('compareSessionRevision', () => {
  for (const testCase of corpus.compare) {
    it(testCase.name, () => {
      const a = JSON.parse(testCase.a) as SessionRevision;
      const b = JSON.parse(testCase.b) as SessionRevision;
      if (testCase.error) {
        expect(() => compareSessionRevision(a, b)).toThrow();
        return;
      }
      expect(compareSessionRevision(a, b)).toBe(COMPARE_RESULTS[testCase.result]);
    });
  }
});

describe('acceptHydration', () => {
  for (const testCase of corpus.accept) {
    it(testCase.name, () => {
      const next = parseHydration(testCase.next);
      const current = testCase.current === null ? null : parseHydration(testCase.current);
      const configuration = testCase.configuration === null
        ? null
        : (JSON.parse(testCase.configuration) as ConfigurationRevision);
      const warnings = testCase.warnings === null
        ? null
        : (JSON.parse(testCase.warnings) as WarningsRevision);
      const before = JSON.stringify({ current, next });
      if (testCase.error) {
        expect(() => acceptHydration(current, next, testCase.instance, configuration, warnings)).toThrow();
      } else {
        expect(acceptHydration(current, next, testCase.instance, configuration, warnings)).toBe(testCase.accept);
      }
      expect(JSON.stringify({ current, next })).toBe(before);
    });
  }
});

describe('itemID', () => {
  for (const testCase of corpus.item_ids) {
    it(testCase.name, () => {
      const item = JSON.parse(testCase.item) as ConversationItem;
      if (testCase.error) {
        expect(() => itemID(item)).toThrow();
        return;
      }
      expect(itemID(item)).toBe(testCase.id);
    });
  }
});

describe('mergeConversation', () => {
  for (const testCase of corpus.merge) {
    it(testCase.name, () => {
      const older = JSON.parse(testCase.older) as ConversationItem[];
      const newer = JSON.parse(testCase.newer) as ConversationItem[];
      const before = JSON.stringify({ older, newer });
      if (testCase.error) {
        expect(() => mergeConversation(older, newer)).toThrow();
      } else {
        const result = mergeConversation(older, newer);
        expect(result).toEqual(JSON.parse(testCase.result));
        expect(result).not.toBe(older);
        expect(result).not.toBe(newer);
      }
      expect(JSON.stringify({ older, newer })).toBe(before);
    });
  }
});

describe('typescript-only raw kinds', () => {
  for (const testCase of corpus.typescript_only) {
    it(testCase.name, () => {
      if (testCase.function === 'compare') {
        expect(() => compareSessionRevision(JSON.parse(testCase.a), JSON.parse(testCase.b))).toThrow();
        return;
      }
      expect(() => acceptHydration(null, JSON.parse(testCase.next), testCase.instance, null, null)).toThrow();
    });
  }
});

describe('validateRevisionHint', () => {
  const INSTANCE = '0123456789abcdef0123456789abcdef';
  const FOREIGN = 'ffffffffffffffffffffffffffffffff';

  it('accepts well-formed same-instance revisions of each hint kind', () => {
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: '7', local_revision: '3' }, INSTANCE)).not.toThrow();
    expect(() => validateRevisionHint('configuration', { instance_id: INSTANCE, generation: '9' }, INSTANCE)).not.toThrow();
    expect(() => validateRevisionHint('warning', { instance_id: INSTANCE, revision: '4' }, INSTANCE)).not.toThrow();
  });

  it('rejects a malformed decimal in each kind', () => {
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: 'not-a-number', local_revision: '0' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: '1', local_revision: '' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('configuration', { instance_id: INSTANCE, generation: '1.5' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('warning', { instance_id: INSTANCE, revision: '-1' }, INSTANCE)).toThrow();
  });

  it('rejects out-of-range components in each kind', () => {
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: '0', local_revision: '0' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: '9223372036854775808', local_revision: '0' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('configuration', { instance_id: INSTANCE, generation: '18446744073709551616' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('warning', { instance_id: INSTANCE, revision: '18446744073709551616' }, INSTANCE)).toThrow();
  });

  it('rejects a foreign instance and a malformed instance in each kind', () => {
    expect(() => validateRevisionHint('session', { instance_id: FOREIGN, durable_revision: '1', local_revision: '0' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('configuration', { instance_id: FOREIGN, generation: '1' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('warning', { instance_id: FOREIGN, revision: '1' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('session', { instance_id: 'nothex', durable_revision: '1', local_revision: '0' }, INSTANCE)).toThrow();
    expect(() => validateRevisionHint('session', { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' }, 'nothex')).toThrow();
  });
});
