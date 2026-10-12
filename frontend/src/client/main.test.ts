// The bootstrap owner: opening the native connection generation, the single
// event pull loop, disposal, failure and supersession, over the generated
// Host-shaped bindings.
import { beforeEach, describe, expect, it, vi } from 'vitest';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  NextEvent: vi.fn(),
  Open: vi.fn(),
  Request: vi.fn(),
}));

import { NextEvent, Open, Request } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import { getHealth } from '../generated/protocol/sdk.gen';
import { RuntimeConnection } from './main';

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
  it('opens the native connection and wires the generated client over the bridge', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    vi.mocked(Request).mockResolvedValue({
      status: 200,
      content_type: 'application/json',
      body: '{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"1"}',
    });
    const connection = new RuntimeConnection();
    const info = await connection.open();
    expect(info.generation).toBe('1');
    const result = await getHealth({ client: connection.getClient() });
    expect(result.error).toBeUndefined();
    expect(Request).toHaveBeenCalledExactlyOnceWith('1', 'GET', '/v1/health', '');
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

  it('a retained old generated client stays stamped with its own generation', async () => {
    vi.mocked(Open).mockResolvedValueOnce(connectionInfo('1')).mockResolvedValueOnce(connectionInfo('2'));
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>(() => {}));
    const connection = new RuntimeConnection();
    await connection.open();
    const oldClient = connection.getClient();
    await connection.open();

    // The retained handle of the first connection still carries generation
    // "1"; the native host's stale-generation rejection reaches the caller
    // instead of the handle silently migrating to "2".
    vi.mocked(Request).mockRejectedValue(new Error('request carries a stale connection generation'));
    const result = await getHealth({ client: oldClient });
    expect((result.error as Error).message).toContain('stale connection generation');
    expect(Request).toHaveBeenCalledExactlyOnceWith('1', 'GET', '/v1/health', '');

    // The current connection handle carries "2".
    vi.mocked(Request).mockResolvedValue({ status: 204, content_type: '', body: '' });
    const current = await getHealth({ client: connection.getClient() });
    expect(current.error).toBeUndefined();
    expect(Request).toHaveBeenLastCalledWith('2', 'GET', '/v1/health', '');
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

  it('stops the loop on a terminal pull failure and records it', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    vi.mocked(NextEvent).mockRejectedValue(new Error('event stream resync required'));
    const handler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    await connection.open();
    await vi.waitFor(() => {
      expect(connection.lastFailure()).toBeInstanceOf(Error);
    });
    expect(handler).not.toHaveBeenCalled();
    expect(NextEvent).toHaveBeenCalledTimes(1);
  });

  it('ignores a delivery settled after disposal', async () => {
    vi.mocked(Open).mockResolvedValue({ instance_id: '0123456789abcdef0123456789abcdef', generation: '1', workspace: '/ws' });
    let settle: (raw: string) => void = () => {};
    vi.mocked(NextEvent).mockImplementation(() => new Promise<string>((resolve) => { settle = resolve; }));
    const handler = vi.fn();
    const connection = new RuntimeConnection();
    connection.setEventHandler(handler);
    await connection.open();
    connection.dispose();
    settle(eventDocument);
    await vi.waitFor(() => {
      expect(connection.isDisposed()).toBe(true);
    });
    expect(handler).not.toHaveBeenCalled();
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
