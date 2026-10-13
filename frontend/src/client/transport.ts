// The target transport: one credential-free fetch forwarding generated SDK
// requests through the native byte bridge. The native host owns authentication.
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import { createClient } from '../generated/protocol/client';
import type { Client } from '../generated/protocol/client';

// Placeholder origin; the native fetch refuses any other origin before the bridge.
export const RUNTIME_ORIGIN = 'http://runtime.invalid';

// The generated Host.Request binding shape: validates the generation, returns raw bytes.
export type NativeRequest = (
  generation: string,
  method: string,
  path: string,
  body: string,
) => Promise<desktopclient.HTTPResponse>;

// Convert SDK requests into native bridge calls and raw byte responses back
// into Response values. The frozen generation rejects a stamped request before
// I/O; raw body bytes pass through untouched and a 204 carries a null body.
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

// Configure one generated SDK client over the placeholder origin and native
// fetch. The generation is frozen into the client; SSE stays native.
export function createRuntimeClient(request: NativeRequest, generation: string): Client {
  return createClient({
    baseUrl: RUNTIME_ORIGIN,
    fetch: createRuntimeFetch(request, generation),
  });
}
