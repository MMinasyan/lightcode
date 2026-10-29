// @vitest-environment happy-dom
// Tool-call presentation: arguments, output and the edit preview expand
// independently through native buttons with aria-expanded and keyboard
// activation; unknown metadata/extra stay raw text.
import { afterEach, describe, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { collapsedPreviewLines } from '../lib/uiConstants.js';
import { parseExactJSON } from './exactjson';

function multilineCall() {
  return {
    id: 'call-1',
    name: 'edit_file',
    arguments: JSON.stringify({
      path: 'internal/config/config.go',
      lines: Array.from({ length: 12 }, (_, i) => `arg line ${i}`),
    }, null, 1),
    status: 'success',
    content: Array.from({ length: 12 }, (_, i) => `output line ${i}`).join('\n'),
    metadata: { edit_preview: { hunks: [{ rows: Array.from({ length: 12 }, (_, i) => ({ kind: i % 2 ? 'add' : 'remove', oldLine: i + 1, newLine: i + 1, text: `preview line ${i}` })) }] } },
    extra: { opaque_thing: 1 },
  };
}

const mounted: Array<ReturnType<typeof mount>> = [];

async function mountCall(call: ReturnType<typeof multilineCall> | Record<string, unknown>) {
  const { default: ToolCallBlock } = await import('./ToolCallBlock.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(ToolCallBlock, { target, props: { call } });
  mounted.push(component);
  await tick();
  return { component, target };
}

afterEach(() => {
  for (const component of mounted.splice(0)) unmount(component);
  document.body.innerHTML = '';
});

function previewOf(pre: Element | null, total: number) {
  const lines = (pre?.textContent ?? '').split('\n');
  expect(lines.length).toBeLessThanOrEqual(total);
  return lines;
}

describe('ToolCallBlock independent expansion', () => {
  it('previews all three blocks collapsed with independent native expanders', async () => {
    const { target } = await mountCall(multilineCall());
    const blocks = target.querySelectorAll('.output-block');
    expect(blocks).toHaveLength(3);
    const expanders = target.querySelectorAll('button.more-btn');
    expect(expanders).toHaveLength(3);
    const argPre = target.querySelectorAll('.output')[0];
    const outPre = target.querySelectorAll('.output')[1];
    expect(previewOf(argPre, 999).length).toBe(collapsedPreviewLines);
    expect(previewOf(outPre, 999).length).toBe(collapsedPreviewLines);
    expect(target.querySelectorAll('.edit-row')).toHaveLength(collapsedPreviewLines);
    for (const expander of expanders) {
      expect(expander.getAttribute('aria-expanded')).toBe('false');
      expect(expander.tagName).toBe('BUTTON');
    }
  });

  it('expands arguments without touching output or preview', async () => {
    const { target } = await mountCall(multilineCall());
    const [argsButton, outputButton, editButton] = [...target.querySelectorAll('button.more-btn')] as HTMLButtonElement[];
    argsButton.click();
    await tick();
    expect(argsButton.getAttribute('aria-expanded')).toBe('true');
    expect(outputButton.getAttribute('aria-expanded')).toBe('false');
    expect(editButton.getAttribute('aria-expanded')).toBe('false');
    expect(target.querySelectorAll('.output')[0].textContent?.split('\n').length).toBeGreaterThan(collapsedPreviewLines);
    expect(target.querySelectorAll('.output')[1].textContent?.split('\n').length).toBe(collapsedPreviewLines);
    expect(target.querySelectorAll('.edit-row')).toHaveLength(collapsedPreviewLines);
  });

  it('expands output while arguments stay expanded and preview collapsed', async () => {
    const { target } = await mountCall(multilineCall());
    const [argsButton, outputButton, editButton] = [...target.querySelectorAll('button.more-btn')] as HTMLButtonElement[];
    argsButton.click();
    await tick();
    outputButton.click();
    await tick();
    expect(argsButton.getAttribute('aria-expanded')).toBe('true');
    expect(outputButton.getAttribute('aria-expanded')).toBe('true');
    expect(editButton.getAttribute('aria-expanded')).toBe('false');
    expect(target.querySelectorAll('.output')[1].textContent?.split('\n').length).toBeGreaterThan(collapsedPreviewLines);
    expect(target.querySelectorAll('.edit-row')).toHaveLength(collapsedPreviewLines);
    // Collapsing arguments never collapses output.
    argsButton.click();
    await tick();
    expect(argsButton.getAttribute('aria-expanded')).toBe('false');
    expect(outputButton.getAttribute('aria-expanded')).toBe('true');
  });

  it('activates each expander through native button semantics independently', async () => {
    const { target } = await mountCall(multilineCall());
    const [argsButton, outputButton, editButton] = [...target.querySelectorAll('button.more-btn')] as HTMLButtonElement[];
    // Every expander is a native BUTTON, so keyboard Enter/Space activation is
    // the platform behavior (verified in the real-browser recheck); a
    // click-only div would not provide it.
    for (const button of [argsButton, outputButton, editButton]) {
      expect(button.tagName).toBe('BUTTON');
    }
    argsButton.click();
    await tick();
    expect(argsButton.getAttribute('aria-expanded')).toBe('true');
    outputButton.click();
    await tick();
    expect(outputButton.getAttribute('aria-expanded')).toBe('true');
    expect(editButton.getAttribute('aria-expanded')).toBe('false');
  });

  it('renders short blocks without expanders and keeps extra opaque', async () => {
    const { target } = await mountCall({
      id: 'call-2', name: 'read_file', arguments: '{"path":"a.go"}', status: 'success', content: 'one line',
    });
    expect(target.querySelectorAll('button.more-btn')).toHaveLength(0);
    expect(target.textContent).toContain('one line');
    expect(target.querySelector('.opaque')).toBeNull();
  });

  it('shows an unknown outcome as uncertain, not as a clean cancellation', async () => {
    const uncertain = await mountCall({
      id: 'call-u', name: 'echo', arguments: '{}', status: 'interrupted', evidence: 'unknown',
      content: 'Execution outcome unknown; the action may have occurred.\n\nTool call interrupted.',
    });
    const uncertainStatus = uncertain.target.querySelector('.status')?.textContent ?? '';
    expect(uncertainStatus).toContain('outcome unknown');
    expect(uncertainStatus).not.toContain('· interrupted');
    const clean = await mountCall({
      id: 'call-c', name: 'echo', arguments: '{}', status: 'interrupted', content: 'Tool call interrupted.',
    });
    const cleanStatus = clean.target.querySelector('.status')?.textContent ?? '';
    expect(cleanStatus).toContain('· interrupted');
    expect(cleanStatus).not.toContain('outcome unknown');
  });
});

// A consumed-shape row as the ACTUAL producer emits it (kind context/add/remove,
// string text, finite oldLine/newLine); unknown members are legal.
function row(kind: string, text: string, extra: Record<string, unknown> = {}) {
  return { kind, text, oldLine: 1, newLine: 1, ...extra };
}

function validHunks() {
  return [{ rows: [row('context', 'a'), row('add', 'b'), row('remove', 'c')] }];
}

function callWith(metadata: unknown) {
  return { id: 'c-1', name: 'edit_file', arguments: '{}', status: 'success', content: 'out', metadata };
}

describe('ToolCallBlock preview recognition (open JSON metadata)', () => {
  const unrecognized = [
    ['primitive number metadata', 42],
    ['string metadata', 'edit_preview'],
    ['array metadata', [1, 2, 3]],
    ['nested null in object metadata', { edit_preview: null }],
    ['array edit_preview', { edit_preview: [1, 2] }],
    ['edit_preview without hunks', { edit_preview: { some: 'object' } }],
    ['edit_preview hunks not an array', { edit_preview: { hunks: 'nope' } }],
    ['conventional key carrying unrelated JSON', { edit_preview: 'not a preview at all' }],
    ['multi candidate not an array', { edit_preview_files: 'nope' }],
    ['multi preview unrelated JSON', { edit_preview_files: [{ path: 'a.go', op: 'A', preview: 'not-a-preview' }] }],
    ['multi missing preview', { edit_preview_files: [{ path: 'a.go', op: 'A' }] }],
    ['null hunk single', { edit_preview: { hunks: [null] } }],
    ['null hunk multi', { edit_preview_files: [{ path: 'a.go', op: 'A', preview: { hunks: [null] } }] }],
    ['null row single', { edit_preview: { hunks: [{ rows: [null] }] } }],
    ['null row multi', { edit_preview_files: [{ path: 'a.go', op: 'A', preview: { hunks: [{ rows: [null] }] } }] }],
    ['scalar row single', { edit_preview: { hunks: [{ rows: ['row'] }] } }],
    ['invalid row kind', { edit_preview: { hunks: [{ rows: [row('same', 'x')] }] } }],
    ['non-string row text', { edit_preview: { hunks: [{ rows: [row('add', 7 as unknown as string)] }] } }],
    ['non-numeric oldLine', { edit_preview: { hunks: [{ rows: [row('add', 'x', { oldLine: '1' })] }] } }],
    ['null newLine', { edit_preview: { hunks: [{ rows: [row('add', 'x', { newLine: null })] }] } }],
    ['infinite oldLine', { edit_preview: { hunks: [{ rows: [row('add', 'x', { oldLine: Infinity })] }] } }],
    ['missing file path', { edit_preview_files: [{ op: 'A', preview: { hunks: [{ rows: [row('add', 'x')] }] } }] }],
    ['empty file path', { edit_preview_files: [{ path: '', op: 'A', preview: { hunks: [{ rows: [row('add', 'x')] }] } }] }],
    ['missing file op', { edit_preview_files: [{ path: 'a.go', preview: { hunks: [{ rows: [row('add', 'x')] }] } }] }],
    ['fabricated-M candidate (invalid op)', { edit_preview_files: [{ path: 'a.go', op: 'X', preview: { hunks: [{ rows: [row('add', 'x')] }] } }] }],
    ['nested null rows array', { edit_preview: { hunks: [{ rows: null }] } }],
    ['nonfinite raw line token', parseExactJSON('{"edit_preview":{"hunks":[{"rows":[{"kind":"add","oldLine":1e400,"text":"x"}]}]}}')],
    ['rawJSON-lookalike oldLine member', { edit_preview: { hunks: [{ rows: [row('add', 'x', { oldLine: { rawJSON: '5' } })] }] } }],
  ];

  for (const [name, metadata] of unrecognized) {
    it(`produces no structured preview for ${name}`, async () => {
      const { target } = await mountCall(callWith(metadata));
      // No structured preview is forwarded or rendered: not an exception, not
      // guessed rows, not a fabricated outcome.
      expect(target.querySelectorAll('.output-block.edit-preview')).toHaveLength(0);
      expect(target.querySelectorAll('.edit-row')).toHaveLength(0);
      // The complete raw metadata stays visible unchanged.
      expect(target.querySelector('.opaque')?.textContent).toContain('metadata');
    });
  }

  it('renders real single and multi-file previews with A/M/D rows, gutters and unknown members', async () => {
    // The single-file preview carries genuine raw line tokens; the per-file
    // previews carry ordinary numbers.
    const rawHunks = (parseExactJSON('{"hunks":[{"rows":[{"kind":"add","newLine":5,"text":"added"},{"kind":"remove","oldLine":6,"text":"removed"},{"kind":"context","oldLine":7,"newLine":7,"text":"kept"}]}]}') as { hunks: never[] }).hunks;
    const { target } = await mountCall(callWith({
      edit_preview: { hunks: rawHunks, unknown_member: 'kept' },
      edit_preview_files: [
        { path: 'a.go', op: 'A', preview: { hunks: validHunks() }, extra_member: 1 },
        { path: 'b.go', op: 'M', preview: { hunks: validHunks() } },
        { path: 'c.go', op: 'D', preview: { hunks: validHunks() } },
      ],
    }));
    // single + three per-file blocks
    expect(target.querySelectorAll('.output-block.edit-preview')).toHaveLength(4);
    const labels = [...target.querySelectorAll('.file-label')].map((n) => n.textContent?.replace(/\s+/g, ' ').trim());
    expect(labels).toEqual(['A a.go', 'M b.go', 'D c.go']);
    // add/remove gutters render through the shared EditPreview, including the
    // interpreted exact raw line tokens
    const gutters = [...target.querySelectorAll('.edit-gutter')].map((n) => n.textContent?.trim()).join(' ');
    expect(gutters).toContain('5 +');
    expect(gutters).toContain('6 -');
    expect(target.querySelectorAll('.edit-preview .edit-gutter.add').length).toBeGreaterThan(0);
    expect(target.querySelectorAll('.edit-preview .edit-gutter.remove').length).toBeGreaterThan(0);
    // unknown members are allowed and stay in raw metadata
    expect(target.querySelector('.opaque')?.textContent).toContain('unknown_member');
  });
});
