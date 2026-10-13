// The exact-JSON editor helpers: source-context parsing keeps every unedited
// numeric lexeme (including signed int64 tokens beyond Number precision)
// exactly as the document carried it, plugin documents stay their exact string
// values, and the composed patches/fork bodies carry raw user tokens verbatim.
import { describe, expect, it } from 'vitest';

import {
  composeForkBody,
  composeMemberPatch,
  composeObjectPatch,
  composePluginsPatch,
  composeSessionsPatch,
  extractSettingsText,
  parseExactJSON,
  parseSettingsObject,
  rawJSONText,
  readRetainedTurnsText,
} from './exactjson';

const CONFIGURATION = '{"configuration_revision":{"instance_id":"0123456789abcdef0123456789abcdef","generation":"1"},'
  + '"settings":{"sessions":{"auto_archive":false,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1e400},'
  + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993,\\"command_timeout\\":1e400}"}},"agents":[],"providers":[]}';

describe('parseExactJSON', () => {
  it('preserves exact numeric source tokens through the ordinary round trip', () => {
    const text = '{"n":-9223372036854775808,"big":9007199254740993,"float":1e400,"plain":2}';
    const parsed = parseExactJSON(text);
    expect(JSON.stringify(parsed)).toBe(text);
  });

  it('exposes the raw token and renders booleans and strings ordinarily', () => {
    const parsed = parseExactJSON('{"n":-9223372036854775808,"flag":true,"s":"x"}') as Record<string, unknown>;
    expect(rawJSONText(parsed.n)).toBe('-9223372036854775808');
    expect(rawJSONText(parsed.flag)).toBeNull();
    expect(JSON.stringify(parsed.n)).toBe('-9223372036854775808');
    expect(JSON.stringify(parsed.flag)).toBe('true');
    expect(JSON.stringify(parsed.s)).toBe('"x"');
  });

  it('rejects malformed text through the native JSON boundary', () => {
    expect(() => parseExactJSON('{not json')).toThrow();
  });
});

describe('settings extraction', () => {
  it('keeps the exact settings subtree, numeric tokens and plugin document values', () => {
    const settingsText = extractSettingsText(CONFIGURATION);
    // The unedited interval tokens survive byte-for-byte.
    expect(settingsText).toContain('-9223372036854775808');
    expect(settingsText).toContain('1e400');
    expect(settingsText).toContain('9007199254740993');
    const parsed = parseSettingsObject(settingsText);
    expect(parsed.sessions.auto_archive).toBe(false);
    // The plugin document is carried as its exact string value.
    expect(parsed.plugins.tools).toBe('{"max_output_bytes":9007199254740993,"command_timeout":1e400}');
  });

  it('rejects a document without a settings member', () => {
    expect(() => extractSettingsText('{"agents":[]}')).toThrow();
  });
});

describe('patch composition', () => {
  it('carries a raw user token verbatim in a one-member patch', () => {
    expect(composeMemberPatch('archive_after_days', '-9223372036854775808'))
      .toBe('{"archive_after_days":-9223372036854775808}');
    expect(composeObjectPatch([['a', '1e400'], ['b', 'true']])).toBe('{"a":1e400,"b":true}');
  });

  it('rebuilds the sessions member preserving auto_archive and the unedited token', () => {
    const settings = parseSettingsObject(extractSettingsText(CONFIGURATION));
    const patch = composeSessionsPatch(settings.sessions, 'delete_after_archive_days', '30');
    expect(patch).toContain('"auto_archive":false');
    expect(patch).toContain('"archive_after_days":-9223372036854775808');
    expect(patch).toContain('"delete_after_archive_days":30');
  });

  it('replaces or removes one plugin document while keeping the others exact', () => {
    const plugins = { tools: '{"a":1}', jobs: '{"b":2}' };
    const replaced = composePluginsPatch(plugins, 'jobs', '{"b":3}');
    expect(replaced).toBe('{"tools":"{\\"a\\":1}","jobs":"{\\"b\\":3}"}');
    const removed = composePluginsPatch(plugins, 'jobs', null);
    expect(removed).toBe('{"tools":"{\\"a\\":1}"}');
  });

  it('treats prototype-named plugin documents as ordinary members for add, update and remove', () => {
    const plugins = JSON.parse('{"__proto__":"{\\"a\\":1}","constructor":"{\\"b\\":2}"}') as Record<string, string>;
    expect(Object.hasOwn(plugins, '__proto__')).toBe(true);
    // Update an existing prototype-named member; the sibling keeps its exact text.
    const updated = composePluginsPatch(plugins, 'constructor', '{"b":3}');
    expect(updated).toContain('"__proto__":"{\\"a\\":1}"');
    expect(updated).toContain('"constructor":"{\\"b\\":3}"');
    // Add a new prototype-named member without disturbing the others.
    const added = composePluginsPatch(plugins, '__proto__', '{"c":1}');
    expect(added).toContain('"__proto__":"{\\"c\\":1}"');
    expect(added).toContain('"constructor":"{\\"b\\":2}"');
    // Remove one as an ordinary own member.
    const removed = composePluginsPatch(plugins, '__proto__', null);
    expect(removed).not.toContain('__proto__');
    expect(removed).toContain('"constructor":"{\\"b\\":2}"');
    // The input map is never mutated.
    expect(Object.hasOwn(plugins, '__proto__')).toBe(true);
    expect(plugins['__proto__']).toBe('{"a":1}');
  });
});

describe('fork body and retained turns', () => {
  it('composes the Fork body from the immutable IDs and the exact replacement text', () => {
    const body = composeForkBody('item-1', 'abcdef', '[{"kind":"text","text":"x"}]');
    expect(JSON.parse(body)).toEqual({
      boundary_item_id: 'item-1',
      operation_id: 'abcdef',
      content: [{ kind: 'text', text: 'x' }],
    });
    // A malformed replacement stays byte-verbatim: the server decides.
    expect(composeForkBody('i', 'o', '[not json')).toBe('{"boundary_item_id":"i","operation_id":"o","content":[not json}');
  });

  it('reads retained turns with their exact boundary tokens', () => {
    const document = parseExactJSON('{"turns":[{"turn":9007199254740993,"files":[{"path":"a","existed":true}]}]}');
    const turns = readRetainedTurnsText(document);
    expect(turns).toEqual([{ turnText: '9007199254740993', files: [{ path: 'a', existed: true }] }]);
  });

  it('rejects a retained turn without a numeric boundary or files', () => {
    expect(() => readRetainedTurnsText({ turns: [{ files: [] }] })).toThrow();
    expect(() => readRetainedTurnsText({ turns: [{ turn: 1 }] })).toThrow();
    expect(() => readRetainedTurnsText({})).toThrow();
  });

  it('returns the retained files array and its file objects by reference', () => {
    const document = parseExactJSON('{"turns":[{"turn":9007199254740993,"files":[{"path":"a","existed":true,"extra":1}]}]}') as {
      turns: Array<{ files: unknown[] }>;
    };
    const turns = readRetainedTurnsText(document);
    expect(turns[0].turnText).toBe('9007199254740993');
    // The consumed shape is validated in place; the original array and objects
    // are returned, never rebuilt (the uninterpreted extra member survives).
    expect(turns[0].files).toBe(document.turns[0].files);
    expect(turns[0].files[0]).toBe(document.turns[0].files[0]);
    // Malformed consumed shapes still throw.
    expect(() => readRetainedTurnsText({ turns: [{ turn: 1, files: [null] }] })).toThrow();
    expect(() => readRetainedTurnsText({ turns: [{ turn: 1, files: [{}] }] })).toThrow();
    expect(() => readRetainedTurnsText({ turns: [{ turn: {}, files: [] }] })).toThrow();
    expect(() => readRetainedTurnsText({ turns: [{ turn: '5', files: [] }] })).toThrow();
  });
});
