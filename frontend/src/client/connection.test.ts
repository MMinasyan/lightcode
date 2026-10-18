// The bootstrap owner: opening the native connection generation, the single
// event pull loop, disposal, failure and supersession, over the generated
// Host-shaped bindings.
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  Call: vi.fn(),
  NextEvent: vi.fn(),
  Open: vi.fn(),
}));

import { Call, NextEvent, Open } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import { RuntimeConnection } from './connection';
import { invokeNative } from './results';

const eventDocument = '{"kind":"scope_opened","scope":{"kind":"runtime"}}';

const connectionInfo = (generation: string): desktopclient.ConnectionInfo => ({
  instance_id: '0123456789abcdef0123456789abcdef',
  generation,
  workspace: '/ws',
});

function deferredOpen() {
  let resolve!: (info: desktopclient.ConnectionInfo) => void;
  const promise = new Promise<desktopclient.ConnectionInfo>((ready) => {
    resolve = ready;
  });
  return { promise, resolve };
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe('RuntimeConnection.open', () => {
  it('opens the native connection and invokes the Call binding with its frozen generation', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    vi.mocked(Call).mockResolvedValue({
      status: 200,
      body: '{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"2"}',
      invalid: false,
    });
    const connection = new RuntimeConnection();
    const info = await connection.open();
    expect(info.generation).toBe('1');
    const result = await invokeNative(() => connection.getClient(), 'health', 'GetHealth', '{}', JSON.parse);
    expect(result.kind).toBe('ok');
    expect(Call).toHaveBeenCalledExactlyOnceWith('1', 'GetHealth', '{}');
  });

  it('rejects opening after disposal', async () => {
    const connection = new RuntimeConnection();
    connection.dispose();
    await expect(connection.open()).rejects.toThrow(/disposed/);
    expect(Open).not.toHaveBeenCalled();
  });
});

describe('RuntimeConnection open attempts', () => {
  it('dispose during an awaited open rejects without installing or pulling', async () => {
    const pendingOpen = deferredOpen();
    vi.mocked(Open).mockReturnValueOnce(pendingOpen.promise);
    const connection = new RuntimeConnection();
    const first = connection.open();
    connection.dispose();
    pendingOpen.resolve(connectionInfo('1'));
    await expect(first).rejects.toThrow(/disposed|superseded/);
    expect(connection.getInfo()).toBeNull();
    expect(() => connection.getClient()).toThrow(/not open/);
    expect(NextEvent).not.toHaveBeenCalled();
  });

  it('a superseded late completion cannot replace the newest installed generation', async () => {
    const lateOpen = deferredOpen();
    vi.mocked(Open).mockReturnValueOnce(lateOpen.promise).mockResolvedValueOnce(connectionInfo('2'));
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    const connection = new RuntimeConnection();
    const first = connection.open();
    await connection.open();
    lateOpen.resolve(connectionInfo('1'));
    await expect(first).rejects.toThrow(/superseded/);
    expect(connection.getInfo()?.generation).toBe('2');
    // The retired attempt never starts its own pull.
    expect(NextEvent.mock.calls.map((call) => call[0])).toEqual(['2']);
    connection.dispose();
  });

  it('a retained old generation handle stays stamped with its own generation', async () => {
    vi.mocked(Open).mockResolvedValueOnce(connectionInfo('1')).mockResolvedValueOnce(connectionInfo('2'));
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    const connection = new RuntimeConnection();
    await connection.open();
    const oldClient = connection.getClient();
    await connection.open();

    // The retained handle of the first connection still carries generation
    // "1"; the native host's stale-generation rejection reaches the caller
    // instead of the handle silently migrating to "2".
    vi.mocked(Call).mockRejectedValue(new Error('call carries a stale connection generation'));
    const result = await invokeNative(() => oldClient, 'health', 'GetHealth', '{}', JSON.parse);
    expect(result.kind).toBe('loss');
    if (result.kind === 'loss') expect(result.message).toContain('stale connection generation');
    expect(Call).toHaveBeenCalledExactlyOnceWith('1', 'GetHealth', '{}');

    // The current connection handle carries "2".
    vi.mocked(Call).mockResolvedValue({ status: 204, body: '', invalid: false });
    const current = await invokeNative(() => connection.getClient(), 'health', 'GetHealth', '{}', JSON.parse);
    expect(current.kind).toBe('ok');
    expect(Call).toHaveBeenLastCalledWith('2', 'GetHealth', '{}');
    connection.dispose();
  });

  it('getClient is unavailable after disposal', async () => {
    vi.mocked(Open).mockResolvedValue(connectionInfo('1'));
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    const connection = new RuntimeConnection();
    await connection.open();
    const client = connection.getClient();
    void client;
    connection.dispose();
    expect(() => connection.getClient()).toThrow(/not open/);
  });
});

describe('RuntimeConnection pull loop', () => {
  it('delivers parsed events in order through the single pull', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    vi.mocked(NextEvent).mockResolvedValueOnce(eventDocument).mockResolvedValueOnce(eventDocument).mockImplementation(() => new Promise(() => {}));
    const delivered: string[] = [];
    const connection = new RuntimeConnection();
    connection.setEventHandler((event) => delivered.push(event.kind));
    await connection.open();
    await vi.waitFor(() => {
      expect(delivered).toEqual(['scope_opened', 'scope_opened']);
    });
    expect(NextEvent).toHaveBeenNthCalledWith(1, '1');
    expect(NextEvent).toHaveBeenNthCalledWith(2, '1');
    connection.dispose();
  });

  it('stops the loop on a terminal pull failure and signals the failure callback', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    vi.mocked(NextEvent).mockRejectedValue(new Error('event stream resync required'));
    const handler = vi.fn();
    const failureHandler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    connection.setFailureHandler(failureHandler);
    await connection.open();
    await vi.waitFor(() => {
      expect(failureHandler).toHaveBeenCalledTimes(1);
    });
    expect(failureHandler.mock.calls[0][1]).toBe('1');
    expect(handler).not.toHaveBeenCalled();
    expect(NextEvent).toHaveBeenCalledTimes(1);
  });

  it('ignores a delivery settled after disposal', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    let settle: (raw: string) => void = () => {};
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>((resolve) => { settle = resolve; }));
    const handler = vi.fn();
    const failureHandler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    connection.setFailureHandler(failureHandler);
    await connection.open();
    connection.dispose();
    settle(eventDocument);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(handler).not.toHaveBeenCalled();
    expect(failureHandler).not.toHaveBeenCalled();
    expect(NextEvent).toHaveBeenCalledTimes(1);
  });

  it('a superseding open retires the previous pull without dispatching its delivery', async () => {
    vi.mocked(Open)
      .mockResolvedValueOnce({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' })
      .mockResolvedValueOnce({ instance_id: '0123456789abcdef0123456789abcdef', generation: '2', workspace: '/ws' });
    const pending: Array<(raw: string) => void> = [];
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>((resolve) => { pending.push(resolve); }));
    const handler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    await connection.open();
    const next = await connection.open();
    expect(next.generation).toBe('2');
    expect(NextEvent).toHaveBeenNthCalledWith(1, '1');
    expect(NextEvent).toHaveBeenNthCalledWith(2, '2');
    pending[0](eventDocument);
    await vi.waitFor(() => {
      expect(handler).not.toHaveBeenCalled();
    });
    // The retired loop issues no further pulls on the old generation.
    expect(NextEvent).toHaveBeenCalledTimes(2);
    connection.dispose();
  });
});

describe('RuntimeConnection delivery-boundary revision validity', () => {
  const INSTANCE = '0123456789abcdef0123456789abcdef';

  function openWith(event: string) {
    vi.mocked(Open).mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
    let settle: (raw: string) => void = () => {};
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>((resolve) => { settle = resolve; }));
    const handler = vi.fn();
    const failureHandler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    connection.setFailureHandler(failureHandler);
    return { connection, deliver: () => settle(event), handler, failureHandler };
  }

  const malformedSessionHint = JSON.stringify({
    kind: 'session_changed',
    scope: { kind: 'session', session_id: 's1' },
    session_revision: { instance_id: INSTANCE, durable_revision: 'not-a-number', local_revision: '0' },
  });
  const outOfRangeWarningHint = JSON.stringify({
    kind: 'warning_changed',
    scope: { kind: 'runtime' },
    warnings_revision: { instance_id: INSTANCE, revision: '18446744073709551616' },
  });
  const foreignConfigHint = JSON.stringify({
    kind: 'configuration_changed',
    scope: { kind: 'runtime' },
    configuration_revision: { instance_id: 'ffffffffffffffffffffffffffffffff', generation: '1' },
  });
  const validSessionHint = JSON.stringify({
    kind: 'session_changed',
    scope: { kind: 'session', session_id: 's1' },
    session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
  });

  it('repairs on a malformed session hint before any reader exists and does not dispatch it', async () => {
    const ctx = openWith(malformedSessionHint);
    await ctx.connection.open();
    ctx.deliver();
    await vi.waitFor(() => {
      expect(ctx.failureHandler).toHaveBeenCalledTimes(1);
    });
    expect(ctx.failureHandler.mock.calls[0][1]).toBe('1');
    expect(ctx.handler).not.toHaveBeenCalled();
    ctx.connection.dispose();
  });

  it('repairs on an out-of-range warning hint', async () => {
    const ctx = openWith(outOfRangeWarningHint);
    await ctx.connection.open();
    ctx.deliver();
    await vi.waitFor(() => {
      expect(ctx.failureHandler).toHaveBeenCalledTimes(1);
    });
    expect(ctx.handler).not.toHaveBeenCalled();
    ctx.connection.dispose();
  });

  it('repairs on a foreign-instance configuration hint even though its Session is not selected', async () => {
    // The hint addresses no displayed Session; validity is not gated on view
    // relevance, so the delivery boundary still repairs.
    const ctx = openWith(foreignConfigHint);
    await ctx.connection.open();
    ctx.deliver();
    await vi.waitFor(() => {
      expect(ctx.failureHandler).toHaveBeenCalledTimes(1);
    });
    expect(ctx.handler).not.toHaveBeenCalled();
    ctx.connection.dispose();
  });

  it('dispatches a valid hint unchanged', async () => {
    const ctx = openWith(validSessionHint);
    await ctx.connection.open();
    ctx.deliver();
    await vi.waitFor(() => {
      expect(ctx.handler).toHaveBeenCalledTimes(1);
    });
    expect(ctx.handler.mock.calls[0][0].kind).toBe('session_changed');
    expect(ctx.failureHandler).not.toHaveBeenCalled();
    ctx.connection.dispose();
  });

  it('ignores a malformed hint delivered on a superseded generation', async () => {
    vi.mocked(Open)
      .mockResolvedValueOnce({ instance_id: INSTANCE, generation: '1', workspace: '/ws' })
      .mockResolvedValueOnce({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    const pending: Array<(raw: string) => void> = [];
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>((resolve) => { pending.push(resolve); }));
    const handler = vi.fn();
    const failureHandler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    connection.setFailureHandler(failureHandler);
    await connection.open();
    await connection.open(); // generation 2 supersedes generation 1
    // The superseded generation's pull delivers a malformed hint: ignored
    // entirely, never a repair signal and never dispatched.
    pending[0](malformedSessionHint);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(failureHandler).not.toHaveBeenCalled();
    expect(handler).not.toHaveBeenCalled();
    connection.dispose();
  });
});
