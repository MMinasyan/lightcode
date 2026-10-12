// @vitest-environment happy-dom
// The conversation renderer: all five generated item variants, refusal and
// tool-call presentation, opaque fields rendered without control
// interpretation, XSS-safe markdown, user-origin input action eligibility,
// scroll follow versus deliberate scroll, and the provisional activity area.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

import type { ConversationItem } from '../generated/protocol/types.gen';

const items: ConversationItem[] = [
  {
    item_id: 'u1', kind: 'input', committed_at: '0', operation_id: 'op-u1', origin: 'user',
    content: [{ kind: 'text', text: 'hello user' }],
  },
  {
    item_id: 'u2', kind: 'input', committed_at: '0', origin: 'plugin',
    content: [{ kind: 'opaque', opaque_wire_type: 'weird', extra: { a: 1 } }],
  },
  {
    item_id: 'a1', kind: 'assistant', committed_at: '0', operation_id: 'op-a1', status: 'completed', source: 'prov/model',
    content: [{ kind: 'text', text: '**bold** and <script>window.__xss = true;</script> tail' }],
    refusal: 'I refuse this part',
    tool_calls: [
      { id: 'call1', name: 'read_file', arguments: '{"path":"a.go"}', status: 'success', content: 'line1\nline2\nline3\nline4\nline5\nline6\nline7' },
      { id: 'call2', name: 'run_command', arguments: '{"command":"go test"}', status: 'error', content: 'boom' },
    ],
    extra: { custom: 'opaque-extra' },
  },
  {
    item_id: 's1', kind: 'signal', committed_at: '0', subtype: 'interruption',
    content: 'stopped by user', related_member: { kind: 'job', id: 'job1' },
  },
  {
    item_id: 'c1', kind: 'compaction', committed_at: '0', operation_id: 'op-c1', summary: 'earlier work summarized', model: 'prov/compact',
  },
  {
    item_id: 'o1', kind: 'operation_end', committed_at: '0', operation_id: 'op-o1', status: 'success', detail: 'all done',
  },
];

beforeEach(() => {
  document.body.innerHTML = '';
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function mountConversation(props = {}) {
  const { default: Conversation } = await import('./Conversation.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(Conversation, { target, props: { items, pending: { steering: [], queued: [] }, ...props } });
  await tick();
  return { component, target };
}

describe('Conversation item variants', () => {
  it('renders every generated variant with its distinct semantics', async () => {
    const { target } = await mountConversation();
    expect(target.querySelector('.message.user')?.textContent).toContain('hello user');
    const assistant = target.querySelector('.message.assistant');
    expect(assistant?.querySelector('strong')?.textContent).toBe('bold');
    expect(assistant?.querySelector('.refusal')?.textContent).toContain('I refuse this part');
    expect(assistant?.querySelector('.source')?.textContent).toContain('prov/model');
    const toolCalls = assistant?.querySelectorAll('.tool-call');
    expect(toolCalls).toHaveLength(2);
    expect(toolCalls?.[0].querySelector('.tool-name')?.textContent).toBe('read_file');
    expect(toolCalls?.[0].querySelector('.status')?.textContent).toContain('success');
    expect(toolCalls?.[1].querySelector('.status')?.textContent).toContain('error');
    expect(target.textContent).toContain('Interrupted');
    expect(target.textContent).toContain('job1');
    expect(target.textContent).toContain('earlier work summarized');
    expect(target.textContent).toContain('prov/compact');
    expect(target.textContent).toContain('all done');
    expect(target.textContent).toContain('opaque-extra');
  });

  it('does not interpret assistant content as controls (XSS)', async () => {
    const { target } = await mountConversation();
    expect(target.querySelector('script')).toBeNull();
    expect((window as unknown as { __xss?: boolean }).__xss).toBeUndefined();
    expect(target.querySelector('.message.assistant')?.textContent).toContain('<script>');
  });

  it('renders unknown opaque fields as plain text', async () => {
    const { target } = await mountConversation();
    const opaque = target.querySelector('.message.user')?.nextElementSibling;
    expect(opaque?.textContent).toContain('[opaque weird');
  });

  it('keys items by their stable IDs without any action affordance', async () => {
    const onClick = vi.fn();
    const { target } = await mountConversation({ onItemAction: onClick });
    const rows = target.querySelectorAll('.message.user');
    expect(rows).toHaveLength(2);
    // An unknown callback prop is inert: no actionable class, no click path.
    expect(target.querySelector('.message.user.actionable')).toBeNull();
    rows[0].addEventListener('click', () => undefined);
    (rows[0] as HTMLElement).click();
    await tick();
    expect(onClick).not.toHaveBeenCalled();
  });
});

describe('Conversation provisional activity', () => {
  it('renders ordered text/refusal/tool pieces under the one provisional identity, without a model', async () => {
    const activity = {
      operationID: 'op1',
      pieces: [
        { kind: 'text', content: 'live text' },
        { kind: 'refusal', content: 'live refusal' },
      ],
      truncated: false,
      tools: [{ callID: 'c1', name: 'read_file', status: null }],
    };
    const { target } = await mountConversation({ activity, activeOperation: { request_kind: 'compact', model: 'prov/compact' } });
    // The combined range is always visibly identified as provisional.
    expect(target.querySelector('.activity-identity')?.textContent).toContain('Model activity — provisional');
    expect(target.querySelector('.provisional-text')?.textContent).toBe('live text');
    expect(target.querySelector('.provisional-refusal')?.textContent).toContain('live refusal');
    expect(target.querySelector('.activity-tool')?.textContent).toContain('read_file');
    // The active compact Operation shows no transport model; only a committed
    // compaction item names one.
    expect(target.querySelector('.activity-area')?.textContent).not.toContain('prov/compact');
  });

  it('shows the single truncation marker for the range and the generic compact label when empty', async () => {
    const activity = {
      operationID: 'op1',
      pieces: [{ kind: 'text', content: 'kept' }],
      truncated: true,
      tools: [],
    };
    const { target } = await mountConversation({ activity, activeOperation: { request_kind: 'compact' } });
    const markers = target.querySelectorAll('.truncation');
    expect(markers).toHaveLength(1);
    expect(markers[0].textContent).toContain('truncated');
    // An empty provisional (no fragments/tools) stays generic with no
    // transport-model claim.
    const empty = await mountConversation({ activity: { operationID: 'op1', pieces: [], truncated: false, tools: [] }, activeOperation: { request_kind: 'compact', model: 'prov/compact' }, busy: true });
    expect(empty.target.querySelector('.activity-area')).toBeNull();
    expect(empty.target.querySelector('.activity-label')?.textContent).toContain('Compacting');
    expect(empty.target.querySelector('.activity-label')?.textContent).not.toContain('prov/compact');
    unmount(empty.component);
  });

  it('renders steering and queued pending inputs dimmed', async () => {
    const { target } = await mountConversation({
      pending: {
        steering: [{ operation_id: 'p1', origin: 'user', content: [{ kind: 'text', text: 'steering one' }] }],
        queued: [{ operation_id: 'p2', origin: 'user', content: [{ kind: 'text', text: 'queued one' }] }],
      },
    });
    expect(target.querySelector('.queued-msg.steering')?.textContent).toContain('steering one');
    expect(target.querySelector('.queued-msg:not(.steering)')?.textContent).toContain('queued one');
  });

  it('renders every occurrence of repeated pending ids in server order without dedup', async () => {
    // Buffered input may repeat a caller id; the pending rows are stateless
    // FIFO occurrences, never identity-keyed or deduplicated.
    const { target } = await mountConversation({
      pending: {
        steering: [
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steering first' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steering second' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steering third' }] },
        ],
        queued: [
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queued first' }] },
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queued second' }] },
        ],
      },
    });
    const steeringRows = [...target.querySelectorAll('.queued-msg.steering')];
    const queuedRows = [...target.querySelectorAll('.queued-msg:not(.steering)')];
    expect(steeringRows.map((r) => r.textContent)).toEqual([
      expect.stringContaining('steering first'),
      expect.stringContaining('steering second'),
      expect.stringContaining('steering third'),
    ]);
    expect(queuedRows.map((r) => r.textContent)).toEqual([
      expect.stringContaining('queued first'),
      expect.stringContaining('queued second'),
    ]);
    // Labels distinguish the two pending kinds.
    expect(steeringRows[0].querySelector('.pending-label')?.textContent).toBe('Steering');
    expect(queuedRows[0].querySelector('.pending-label')?.textContent).toBe('Queued');
  });

  it('offers older pages only when a cursor exists', async () => {
    const { target, component } = await mountConversation({ olderAvailable: false });
    expect(target.querySelector('.load-older')).toBeNull();
    unmount(component);
    const second = await mountConversation({ olderAvailable: true });
    const button = second.target.querySelector('.load-older') as HTMLElement;
    expect(button).not.toBeNull();
    expect(button.getAttribute('aria-label')).toBe('Load older messages');
    button.click(); // the App wires the event; the click must not throw
    await tick();
    unmount(second.component);
  });
});

function rows(prefix: string, count: number, kind0: 'add' | 'remove' | 'same' = 'add') {
  return Array.from({ length: count }, (_, i) => ({
    // Real producer rows are context/add/remove (the Go editpreview kinds).
    kind: i === 0 ? 'context' : (kind0 === 'remove' ? 'remove' : 'add'),
    oldLine: i + 1,
    newLine: i + 1,
    text: `${prefix} line ${i}`,
  }));
}

describe('Conversation apply_patch multi-file preview', () => {
  const applyPatchItems = [
    {
      item_id: 'a1', kind: 'assistant', committed_at: '0', status: 'completed', source: 'p/m',
      content: [{ kind: 'text', text: 'patched three files' }],
      tool_calls: [{
        id: 'call-patch', name: 'apply_patch', arguments: '{}', status: 'success', content: '',
        metadata: {
          edit_preview_files: [
            { path: 'a.go', op: 'A', preview: { hunks: [{ rows: rows('added', 7) }] } },
            { path: 'b.go', op: 'M', preview: { hunks: [{ rows: rows('modified', 6) }] } },
            { path: 'c.go', op: 'D', preview: { hunks: [{ rows: rows('deleted', 8, 'remove') }] } },
          ],
        },
      }],
    },
  ];

  it('renders one block per file with actual path/op labels and independent collapse', async () => {
    const { target } = await mountConversation({ items: applyPatchItems });
    const files = target.querySelectorAll('.output-block.edit-preview');
    expect(files).toHaveLength(3);
    const labels = [...target.querySelectorAll('.file-label')].map((n) => n.textContent?.replace(/\s+/g, ' ').trim());
    expect(labels).toEqual(['A a.go', 'M b.go', 'D c.go']);
    // Each file collapses to the shared preview line budget, none fabricated.
    const expanderCount = () => [...target.querySelectorAll('.output-block.edit-preview button.more-btn')];
    expect(expanderCount()).toHaveLength(3);
    expect(expanderCount().every((b) => b.getAttribute('aria-expanded') === 'false')).toBe(true);
    // Gutter rows are the shared EditPreview rendering (add/remove markers).
    expect(target.querySelectorAll('.edit-preview .edit-row.add').length).toBeGreaterThan(0);
    // Collapsing the FIRST file leaves the others collapsed: per-file state.
    expanderCount()[0].click();
    await tick();
    const after = expanderCount();
    expect(after[0].getAttribute('aria-expanded')).toBe('true');
    expect(after[1].getAttribute('aria-expanded')).toBe('false');
    expect(after[2].getAttribute('aria-expanded')).toBe('false');
  });

  it('preserves raw other metadata without interpreting it as a preview', async () => {
    const { target } = await mountConversation({
      items: [{
        item_id: 'a1', kind: 'assistant', committed_at: '0', status: 'completed', source: 'p/m',
        content: [{ kind: 'text', text: 'x' }],
        tool_calls: [{ id: 'c', name: 'run_command', arguments: '{}', status: 'success', content: 'out', metadata: { opaque_thing: 42 } }],
      }],
    });
    expect(target.textContent).toContain('opaque_thing');
    expect(target.querySelectorAll('.output-block.edit-preview')).toHaveLength(0);
  });
});

// Conversation scroll-following identity reset (C12) is verified in the bounded
// real-browser recheck: happy-dom reports zero scrollHeight/clientHeight so the
// pin-to-bottom effect is not observable there.
