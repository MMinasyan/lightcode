// Bearer-contract checks over the generated protocol SDK: one configured
// credential must reach both the health read and the SSE events stream, and
// an absent credential must not invent a header. The fetch layer is faked;
// the SSE stream consumes one finite synthetic frame with retries disabled,
// so no sleeps, reconnects, or real listeners are involved. Contract check
// only — the transport implementation belongs to the runtime adapter work.
import { expect, test, vi } from 'vitest';

import { getEvents, getHealth } from './src/generated/protocol';
import { createClient } from './src/generated/protocol/client';

const credential = '0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f';
const instanceID = '1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f';
const sseFrame = 'event: notification\ndata: {"kind":"scope_opened","scope":{"kind":"runtime"}}\n\n';

test('configured bearer credential reaches health and events', async () => {
  const fetchFn = vi.fn(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (request.url.endsWith('/v1/events')) {
      return new Response(sseFrame, {
        status: 200,
        headers: { 'content-type': 'text/event-stream' },
      });
    }
    return Response.json({ instance_id: instanceID, protocol_version: '1' });
  });
  const client = createClient({
    baseUrl: 'http://127.0.0.1:1',
    auth: credential,
    fetch: fetchFn,
  });

  const health = await getHealth({ client });
  expect(health.data).toEqual({ instance_id: instanceID, protocol_version: '1' });

  const events = await getEvents({ client, sseMaxRetryAttempts: 1 });
  const received = [];
  for await (const event of events.stream) {
    received.push(event);
  }
  expect(received).toEqual([{ kind: 'scope_opened', scope: { kind: 'runtime' } }]);

  expect(fetchFn).toHaveBeenCalledTimes(2);
  const urls = fetchFn.mock.calls.map(([request]) => String(request.url));
  expect(urls.sort()).toEqual(['http://127.0.0.1:1/v1/events', 'http://127.0.0.1:1/v1/health']);
  for (const [request] of fetchFn.mock.calls) {
    expect(request.headers.get('authorization')).toBe(`Bearer ${credential}`);
  }
});

test('absent auth does not invent a bearer header', async () => {
  const fetchFn = vi.fn(async () => Response.json({ instance_id: instanceID, protocol_version: '1' }));
  const client = createClient({ baseUrl: 'http://127.0.0.1:1', fetch: fetchFn });

  await getHealth({ client });
  const [request] = fetchFn.mock.calls[0];
  expect(request.headers.get('authorization')).toBeNull();
});
