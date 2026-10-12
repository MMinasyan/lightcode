// The target desktop transport: one credential-free fetch that forwards every
// generated SDK request through the native byte bridge, and the generated
// createClient instance configured over it. The frontend never installs
// authentication, persists a credential, or reaches the Runtime loopback
// directly; the native Go host owns the verified connection.
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import { createClient } from '../generated/protocol/client';
import type { Client } from '../generated/protocol/client';

// The placeholder origin every generated SDK request carries. The native
// fetch refuses any other origin before touching the byte bridge, so no
// absolute, foreign, or direct loopback target can leave the frontend.
export const RUNTIME_ORIGIN = 'http://runtime.invalid';

// NativeRequest is the generated Host.Request binding shape: it validates
// the connection generation and returns one raw byte response.
export type NativeRequest = (
  generation: string,
  method: string,
  path: string,
  body: string,
) => Promise<desktopclient.HTTPResponse>;

// createRuntimeFetch converts generated SDK requests into native bridge
// calls and native byte responses back into Response values. The frozen
// generation belongs to the constructed fetch: a superseded connection
// rejects every stamped request before the bridge performs I/O, and the Go
// host cancels a request whose connection is replaced while it is in
// flight. Raw body bytes pass through untouched in both directions, and a
// 204 carries a null body.
export function createRuntimeFetch(
  request: NativeRequest,
  generation: string,
): (input: RequestInfo | URL, init?: RequestInit) => Promise<Response> {
  return async (input, init) => {
    const req = input instanceof Request ? input : new Request(input, init);
    const url = new URL(req.url);
    if (url.origin !== RUNTIME_ORIGIN) {
      throw new Error(`native transport refuses the foreign origin ${url.origin}`);
    }
    const body = req.body === null ? '' : await req.text();
    const response = await request(generation, req.method, url.pathname + url.search, body);
    const headers = new Headers();
    if (response.content_type !== '') {
      headers.set('Content-Type', response.content_type);
    }
    return new Response(response.status === 204 ? null : response.body, {
      status: response.status,
      headers,
    });
  };
}

// createRuntimeClient configures one generated SDK client over the
// placeholder origin and the native fetch, overriding the generated loopback
// default without editing generated code. The generation is frozen into the
// constructed client: a handle never migrates to a newer connection.
// GetEvents stays supplied by the native generated Go subscription, never
// this client's SSE engine.
export function createRuntimeClient(request: NativeRequest, generation: string): Client {
  return createClient({
    baseUrl: RUNTIME_ORIGIN,
    fetch: createRuntimeFetch(request, generation),
  });
}
