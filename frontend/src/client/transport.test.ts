// The TS custom-fetch owner: exact generated Request to native bridge
// conversion, foreign-origin refusal before the bridge, 204 null bodies,
// raw error/partial/raw-document bytes, the frozen generation stamp, and
// canceled or stale responses, through the generated SDK operations.
import { describe, expect, it, vi } from 'vitest';

import { getHealth } from '../generated/protocol/sdk.gen';
import { createRuntimeClient, createRuntimeFetch, RUNTIME_ORIGIN } from './transport';

const nativeRequest = (response: { status: number; content_type: string; body: string }) => vi.fn().mockResolvedValue(response);

describe('createRuntimeFetch', () => {
  it('converts an exact generated request into the native bridge call', async () => {
    const request = nativeRequest({ status: 200, content_type: 'application/json', body: '{"ok":true}' });
    const fetch = createRuntimeFetch(request, '7');
    const response = await fetch(new Request(`${RUNTIME_ORIGIN}/v1/sessions/abc/restore?keep=a+b`, {
      method: 'PUT',
      body: '{"operation_id":"op-1"}',
    }));
    expect(request).toHaveBeenCalledExactlyOnceWith('7', 'PUT', '/v1/sessions/abc/restore?keep=a+b', '{"operation_id":"op-1"}');
    expect(response.status).toBe(200);
    expect(response.headers.get('Content-Type')).toBe('application/json');
    expect(await response.text()).toBe('{"ok":true}');
  });

  it('sends no body for a bodyless request', async () => {
    const request = nativeRequest({ status: 204, content_type: '', body: '' });
    const fetch = createRuntimeFetch(request, '3');
    const response = await fetch(new Request(`${RUNTIME_ORIGIN}/v1/configuration`));
    expect(request).toHaveBeenCalledExactlyOnceWith('3', 'GET', '/v1/configuration', '');
    expect(response.status).toBe(204);
    expect(response.body).toBeNull();
    expect(await response.text()).toBe('');
  });

  it('preserves raw error and partial response bytes', async () => {
    const partial = '{"restored":[1,2],"failed":[3],"durable_revision":"9007199254740993"}';
    const request = nativeRequest({ status: 409, content_type: 'application/json', body: partial });
    const fetch = createRuntimeFetch(request, '1');
    const response = await fetch(new Request(`${RUNTIME_ORIGIN}/v1/sessions/abc/restore`, { method: 'POST', body: '{}' }));
    expect(response.status).toBe(409);
    expect(await response.text()).toBe(partial);
  });

  it('refuses every foreign origin before the bridge', async () => {
    const request = nativeRequest({ status: 200, content_type: '', body: '' });
    const fetch = createRuntimeFetch(request, '1');
    for (const url of ['http://example.com/v1/health', 'https://runtime.invalid/v1/health', 'http://127.0.0.1:34115/v1/health', 'http://runtime.invalid.evil.com/v1/health']) {
      await expect(fetch(new Request(url))).rejects.toThrow(/foreign origin/);
    }
    expect(request).not.toHaveBeenCalled();
  });

  it('rejects when the bridge reports a stale generation', async () => {
    const request = vi.fn().mockRejectedValue(new Error('request carries a stale connection generation'));
    const fetch = createRuntimeFetch(request, '9');
    await expect(fetch(new Request(`${RUNTIME_ORIGIN}/v1/health`))).rejects.toThrow(/stale connection generation/);
    expect(request).toHaveBeenCalledOnce();
  });
});

describe('createRuntimeClient', () => {
  it('rides a generated operation over the native bridge with the placeholder origin', async () => {
    const request = nativeRequest({ status: 200, content_type: 'application/json', body: '{"instance_id":"0123456789abcdef0123456789abcdef","protocol_version":"1"}' });
    const client = createRuntimeClient(request, '5');
    const result = await getHealth({ client });
    expect(request).toHaveBeenCalledExactlyOnceWith('5', 'GET', '/v1/health', '');
    expect(result.error).toBeUndefined();
    expect(result.data).toEqual({ instance_id: '0123456789abcdef0123456789abcdef', protocol_version: '1' });
  });

  it('returns a 204 generated operation with a null body', async () => {
    const request = nativeRequest({ status: 204, content_type: '', body: '' });
    const client = createRuntimeClient(request, '1');
    const result = await client.delete({ url: '/v1/sessions/abc' });
    expect(result.error).toBeUndefined();
    // A contentless 204 carries no Content-Type, so the generated client
    // parses its null body as the raw stream value.
    expect(result.data).toBeNull();
    expect(result.response.body).toBeNull();
  });

  it('forwards error responses through the generated error shape with raw bytes intact', async () => {
    const partial = '{"restored":[1,2],"failed":[3],"durable_revision":"9007199254740993"}';
    const request = nativeRequest({ status: 409, content_type: 'application/json', body: partial });
    const client = createRuntimeClient(request, '1');
    const result = await client.post({ url: '/v1/sessions/abc/restore', body: '{}', headers: { 'Content-Type': 'application/json' } });
    expect(JSON.stringify(result.error)).toBe(partial);
  });

  it('carries a bodySerializer override through as its exact text', async () => {
    const document = '{"plugins":{"p":{"n":9007199254740993}}}';
    const request = nativeRequest({ status: 204, content_type: '', body: '' });
    const client = createRuntimeClient(request, '1');
    const result = await client.put({
      url: '/v1/configuration/settings',
      body: document,
      bodySerializer: (body) => body as string,
      headers: { 'Content-Type': 'application/json' },
    });
    expect(result.error).toBeUndefined();
    expect(request).toHaveBeenCalledExactlyOnceWith('1', 'PUT', '/v1/configuration/settings', document);
  });

  it('surfaces a stale-generation failure on the current request', async () => {
    const request = vi.fn().mockRejectedValue(new Error('request carries a stale connection generation'));
    const client = createRuntimeClient(request, '9');
    const result = await client.get({ url: '/v1/health' });
    expect(result.error).toBeInstanceOf(Error);
    expect((result.error as Error).message).toContain('stale connection generation');
    expect(result.response).toBeUndefined();
  });

  it('surfaces a native context-cancellation failure on the current request', async () => {
    const request = vi.fn().mockRejectedValue(new Error('Post "http://runtime.invalid/v1/sessions/abc/interrupt": context canceled'));
    const client = createRuntimeClient(request, '4');
    const result = await client.post({ url: '/v1/sessions/abc/interrupt' });
    expect(result.error).toBeInstanceOf(Error);
    expect((result.error as Error).message).toContain('context canceled');
    expect(result.response).toBeUndefined();
  });
});
