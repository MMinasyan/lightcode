// @vitest-environment happy-dom
// The real-stack mounted owner: the actual RuntimeConnection, its credential-
// free custom fetch, and the generated SDK drive the mounted App over MOCKED
// GENERATED HOST BINDINGS ONLY. This is where the actual throwOnError=false
// contract is proven: a received status is always a result and only a binding
// rejection leaves the response absent. Nothing here mocks the SDK surface.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  NextEvent: vi.fn(),
  Open: vi.fn(),
  Request: vi.fn(),
}));

import { Open, NextEvent, Request } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import { RuntimeConnection } from './connection';

const INSTANCE = '0123456789abcdef0123456789abcdef';
const NEWER = '2026-10-02T10:00:00Z';
const OLDER = '2026-10-01T05:00:00Z';
const mounted: Array<ReturnType<typeof mount>> = [];

function session(id: string, lastActivity: string, parent?: string) {
  return {
    session_id: id,
    workspace: '/ws',
    ...(parent ? { parent_session_id: parent } : {}),
    lifecycle: 'open',
    agent_type: 'primary',
    created_at: '0',
    last_activity: lastActivity,
    session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
  };
}

function hydration(sessionID: string, overrides: Record<string, unknown> = {}) {
  return {
    session: session(sessionID, NEWER),
    selected_model: null,
    session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    operations: [],
    pending: { steering: [], queued: [] },
    conversation: { items: [] },
    usage: { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } },
    warnings: [],
    background: [],
    ...overrides,
  };
}

function json(status: number, value: unknown) {
  return { status, content_type: 'application/json', body: JSON.stringify(value) };
}

// routeState is the scripted in-memory Runtime behind the byte bridge.
interface Delayed {
  delay: Promise<{ status: number; body: unknown }>;
}

interface Script {
  submit?: (body: unknown) => { status: number; body: unknown } | 'throw' | Delayed;
  compact?: (body: unknown) => { status: number; body: unknown } | 'throw';
  stop?: () => { status: number; body: unknown } | 'throw' | Delayed;
  events?: string[];
  // Scripted raw hydration responses consumed in order (e.g. an empty 200).
  hydration?: Array<{ status: number; content_type: string; body: string }>;
}

function bridge(script: Script = {}) {
  const calls: Array<{ method: string; path: string; body: string; generation: string }> = [];
  vi.mocked(Request).mockImplementation(async (generation, method, path, body) => {
    calls.push({ generation, method, path, body });
    const url = new URL(path, 'http://runtime.invalid');
    const p = url.pathname;
    if (method === 'GET' && p === '/v1/sessions') {
      return json(200, [session('root1', NEWER), session('root2', OLDER)]);
    }
    const hydrationMatch = /^\/v1\/sessions\/([^/]+)\/hydration$/.exec(p);
    if (method === 'GET' && hydrationMatch) {
      if (script.hydration && script.hydration.length > 0) {
        return script.hydration.shift();
      }
      return json(200, hydration(hydrationMatch[1]));
    }
    if (method === 'GET' && p === '/v1/configuration') {
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      });
    }
    if (method === 'GET' && p === '/v1/warnings') {
      return json(200, { warnings_revision: { instance_id: INSTANCE, revision: '1' }, warnings: [] });
    }
    const submitMatch = /^\/v1\/sessions\/([^/]+)\/submit$/.exec(p);
    if (method === 'POST' && submitMatch) {
      if (script.submit === 'throw') {
        throw new Error('native bridge: connection lost mid-request');
      }
      if (typeof script.submit === 'function') {
        const outcome = script.submit(JSON.parse(body));
        if ('delay' in outcome) {
          const resolved = await outcome.delay;
          if (resolved.status === 0) throw new Error('native bridge: late loss');
          return json(resolved.status, resolved.body);
        }
        return json(outcome.status, outcome.body);
      }
      return json(200, { disposition: 'admitted' });
    }
    const compactMatch = /^\/v1\/sessions\/([^/]+)\/compact$/.exec(p);
    if (method === 'POST' && compactMatch) {
      if (script.compact === 'throw' || (typeof script.compact === 'function' && script.compact(JSON.parse(body)) === 'throw')) {
        throw new Error('native bridge: connection lost mid-request');
      }
      return json(200, { operation_id: JSON.parse(body).operation_id, request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } });
    }
    if (/^\/v1\/sessions\/[^/]+\/interrupt$/.test(p) && method === 'POST') {
      return { status: 204, content_type: '', body: '' };
    }
    const stopMatch = /^\/v1\/sessions\/([^/]+)\/stop$/.exec(p);
    if (method === 'POST' && stopMatch && script.stop) {
      const outcome = script.stop();
      if (outcome === 'throw') throw new Error('native bridge: stop lost');
      if ('delay' in outcome) {
        const resolved = await outcome.delay;
        if (resolved.status === 0) throw new Error('native bridge: stop lost');
        return json(resolved.status, resolved.body);
      }
      return json(outcome.status, outcome.body);
    }
    if (/^\/v1\/sessions\/[^/]+\/stop$/.test(p) && method === 'POST') {
      return { status: 204, content_type: '', body: '' };
    }
    return json(404, { code: 'not_found', message: 'no fixture for ' + method + ' ' + p });
  });
  vi.mocked(Open).mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
  const events = script.events ?? [];
  let pullIndex = 0;
  vi.mocked(NextEvent).mockImplementation(async () => {
    if (pullIndex < events.length) {
      // Real hints arrive after composition settled the initial read; the
      // scripted queue yields one macrotask before each delivery.
      await new Promise((resolve) => setTimeout(resolve, 5));
      return events[pullIndex++];
    }
    return new Promise<string>(() => {});
  });
  return calls;
}

async function mountReal(script: Script = {}) {
  const calls = bridge(script);
  const { default: App } = await import('./App.svelte');
  const connection = new RuntimeConnection();
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(App, { target, props: { connection } });
  mounted.push(component);
  await vi.waitFor(() => {
    expect(calls.some((call) => call.path.includes('/hydration'))).toBe(true);
  });
  await tick();
  return { connection, component, target, calls };
}

function typeInto(target: HTMLElement, text: string) {
  const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
  textarea.value = text;
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

afterEach(async () => {
  for (const component of mounted.splice(0)) unmount(component);
  document.body.innerHTML = '';
  vi.clearAllMocks();
});

describe('real generated stack over the byte bridge', () => {
  it('composes the initial view through the actual SDK request encoding', async () => {
    const { target, calls } = await mountReal();
    expect(calls.some((call) => call.method === 'GET' && call.path === '/v1/sessions?workspace=%2Fws&lifecycle=open')).toBe(true);
    expect(calls.some((call) => call.path === '/v1/configuration')).toBe(true);
    expect(calls.some((call) => call.path === '/v1/warnings')).toBe(true);
    const hydrationCall = calls.find((call) => call.path.includes('/hydration'));
    expect(hydrationCall?.path).toBe('/v1/sessions/root1/hydration');
    expect(hydrationCall?.generation).toBe('1');
    expect(target.textContent).toContain('root1');
  });

  it('a real native Request rejection is a transport loss, not a server rejection', async () => {
    const { target } = await mountReal({ submit: 'throw' });
    typeInto(target, 'lost message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    // The generated SDK resolves {error, response: undefined}; the consumer
    // must show Outcome unknown, never a generated protocol error.
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect(target.querySelector('.attempt.rejected')).toBeNull();
  });

  it('a real 400 is a known rejection that keeps the draft and reports the generated error', async () => {
    const { target } = await mountReal({
      submit: () => ({ status: 400, body: { code: 'invalid', message: 'content is not admitted' } }),
    });
    typeInto(target, 'bad message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('.attempt.rejected')?.textContent).toContain('content is not admitted');
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('bad message');
    expect(target.querySelector('.attempt.unknown')).toBeNull();
  });

  it('a real 200 disposition clears the matching draft and retires the attempt', async () => {
    const { target, calls } = await mountReal();
    typeInto(target, 'delivered text');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    const submit = calls.find((call) => call.path.endsWith('/submit'));
    expect(submit).toBeDefined();
    const body = JSON.parse(submit!.body) as { operation_id: string; mode: string; content: unknown[] };
    expect(body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(body.mode).toBe('regular');
    expect(body.content).toEqual([{ kind: 'text', text: 'delivered text' }]);
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('');
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });

  it('a delayed loss across a root selection records unknown against the captured session and resends nothing', async () => {
    let release: () => void = () => {};
    const gate = new Promise<{ status: number; body: unknown }>((resolve) => { release = () => resolve({ status: 0, body: undefined }); });
    const { target, calls } = await mountReal({
      submit: () => ({ delay: gate }),
    });
    typeInto(target, 'in flight');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    // Move to root2 while the submit is in flight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    (rows[1] as HTMLElement).click();
    await settle();
    release();
    await settle();
    await settle();
    // Exactly one submit ever left the bridge.
    expect(calls.filter((call) => call.path.endsWith('/submit'))).toHaveLength(1);
    // Back on root1 the attempt is retained as unknown, not silently dropped.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const backRows = target.querySelectorAll('dialog .row');
    (backRows[0] as HTMLElement).click();
    await settle();
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
  });

  it('Compact uses the same shared loss classification', async () => {
    const { target, calls } = await mountReal({ compact: () => 'throw' });
    (target.querySelector('.composer .compact') as HTMLElement).click();
    await settle();
    await settle();
    const compactCall = calls.find((call) => call.path.endsWith('/compact'));
    expect(compactCall).toBeDefined();
    expect((JSON.parse(compactCall!.body) as { operation_id: string }).operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect(target.querySelector('.attempt.rejected')).toBeNull();
  });

  it('an addressed control still reports its generated failure after the selection moved', async () => {
    let release: () => void = () => {};
    const gate = new Promise<{ status: number; body: unknown }>((resolve) => { release = () => resolve({ status: 500, body: { code: 'internal', message: 'stop blew up' } }); });
    const { target } = await mountReal({ stop: () => ({ delay: gate }) });
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    // Move the selection while the control is in flight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    (rows[1] as HTMLElement).click();
    await settle();
    release();
    await settle();
    await settle();
    // The addressed outcome is reported against the captured session even
    // though root2 is now displayed.
    expect(target.querySelector('.note.outcome')?.textContent).toContain('session root1');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('stop blew up');
  });

  it('delivers live events through the real pull loop into the reader hint path', async () => {
    const newerSessionChanged = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { calls } = await mountReal({ events: [newerSessionChanged] });
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a malformed event document ends the pull and triggers the real repair reopen', async () => {
    const { calls } = await mountReal({ events: ['{not json'] });
    await vi.waitFor(() => {
      expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1);
    });
    // The reopened generation re-establishes SSE and re-reads the root.
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a real 204 Stop is a valid success: no note, and the stamped view refreshes', async () => {
    const { target, calls } = await mountReal();
    await settle();
    const readsBefore = calls.filter((c) => c.path.includes('/hydration')).length;
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    await settle();
    // The bridge returns a real 204 for /stop; the classification treats it as
    // success, so NO outcome/error/unknown note is rendered...
    expect(target.querySelector('.note')).toBeNull();
    // ...and the stamped root view is refreshed by a further hydration read.
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.path.includes('/hydration')).length).toBeGreaterThan(readsBefore);
    });
    expect(target.querySelector('.note')).toBeNull();
  });

  it('a required-body hydration returning an empty 200 is invalid delivery, never adopted as a Hydration', async () => {
    // Boot reads a valid hydration; the first re-read (a hint-driven refresh)
    // returns a 200 with an empty application/json body. The SDK yields no
    // data; the reader must route that to repair, never render it as a
    // (blank) projection.
    const empty = { status: 200, content_type: 'application/json', body: '' };
    const newerHint = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { target, calls } = await mountReal({ hydration: [empty], events: [newerHint] });
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
    // The invalid read drove a repair (a second Open), and no blank view was
    // adopted: the session label remains root1, never reset to a sessionless
    // render from the empty body.
    await vi.waitFor(() => expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1));
    expect(target.textContent).toContain('root1');
  });

  it('preserves every repeated pending occurrence across initial and replacement hydration (real SDK)', async () => {
    // The producer may buffer an id more than once; the real fetch→SDK→reader
    // path must render every FIFO occurrence on the initial body and again on
    // a replacement body, without deduplicating or minting attempt rows.
    const firstBody = hydration('root1', {
      pending: {
        steering: [
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer first' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer second' }] },
        ],
        queued: [
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue first' }] },
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue second' }] },
        ],
      },
    });
    const secondBody = hydration('root1', {
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
      pending: {
        steering: [
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer first' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer second' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer third' }] },
        ],
        queued: [
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue first' }] },
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue second' }] },
        ],
      },
    });
    const newerHint = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { target } = await mountReal({
      hydration: [json(200, firstBody), json(200, secondBody)],
      events: [newerHint],
    });
    // The replacement body is the live one once its re-read lands. It carries
    // three SAME-id steering occurrences and two SAME-id queued occurrences;
    // its first two steering rows are exactly the initial body's duplicates, so
    // this single final render proves both bodies preserved every occurrence.
    await vi.waitFor(() => {
      expect(target.querySelectorAll('.queued-msg.steering')).toHaveLength(3);
    });
    const steeringRows = [...target.querySelectorAll('.queued-msg.steering')];
    expect(steeringRows.map((r) => r.textContent)).toEqual([
      expect.stringContaining('steer first'),
      expect.stringContaining('steer second'),
      expect.stringContaining('steer third'),
    ]);
    const queuedRows = [...target.querySelectorAll('.queued-msg:not(.steering)')];
    expect(queuedRows).toHaveLength(2);
    expect(queuedRows[0].textContent).toContain('queue first');
    expect(queuedRows[1].textContent).toContain('queue second');
    // Pending rows never mint composer attempt rows.
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });
});
