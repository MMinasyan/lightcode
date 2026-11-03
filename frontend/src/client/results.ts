// The one native invocation boundary shared by every target consumer. The
// generated native Call binding carries one clientops invocation envelope and
// returns the lossless local result: the actual status, the available raw body
// and the one invalid-delivery bit. Classification: native rejection = loss;
// invalid bit = invalid delivery before any status/body branch; non-2xx = known
// rejection over its validated Error/partial body; 204 = valid empty success;
// required success JSON the selected decoder cannot consume or that decodes to
// null = invalid delivery. A malformed composed envelope is a known local
// rejection, never transport loss or a fabricated Runtime error.
import { Call } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { Error as RuntimeError } from '../generated/protocol/types.gen';
import { errorText } from '../lib/errors.js';

// The locally frozen generation handle, not a copied protocol DTO.
export type InvocationClient = Readonly<{ generation: string }>;

export type ClassifiedResult =
  | { kind: 'ok'; data: unknown }
  | { kind: 'loss'; message: string }
  | { kind: 'rejection'; message: string; payload: unknown }
  | { kind: 'invalid'; message: string };

// ONE invocation boundary: compose nothing, decode only the accepted body.
export async function invokeNative(
  getClient: () => InvocationClient,
  label: string,
  operation: string,
  params: string,
  decode: (body: string) => unknown,
): Promise<ClassifiedResult> {
  // Syntax only: the composed text is dispatched byte-exact, so opaque numeric
  // lexemes pass through unchanged.
  try {
    JSON.parse(params);
  } catch (error) {
    return { kind: 'rejection', message: `${label}: ${errorText(error)}`, payload: undefined };
  }
  let response;
  try {
    response = await Call(getClient().generation, operation, params);
  } catch (error) {
    return { kind: 'loss', message: `${label}: ${errorText(error)}` };
  }
  if (response.invalid) {
    return { kind: 'invalid', message: `${label}: received an unusable response` };
  }
  if (response.status === 204) {
    return { kind: 'ok', data: {} };
  }
  if (response.status < 200 || response.status >= 300) {
    // The native owner validated this Error/partial body; decode it directly.
    const payload = JSON.parse(response.body);
    return { kind: 'rejection', message: rejectionMessage(payload, label), payload };
  }
  // Decoder failures are invalid delivery, never native-call loss.
  try {
    const data = decode(response.body);
    // A required-success JSON null carries no result: invalid delivery, never
    // an accepted empty value at any consumer.
    if (data === null || data === undefined) {
      return { kind: 'invalid', message: `${label}: received success without a result` };
    }
    return { kind: 'ok', data };
  } catch (error) {
    return { kind: 'invalid', message: `${label}: ${errorText(error)}` };
  }
}

// Preserve the rejected body's text; fall back to the label only when it carries none.
function rejectionMessage(error: unknown, label: string): string {
  const generated = error as RuntimeError | undefined;
  if (typeof generated?.message === 'string' && generated.message !== '') {
    return generated.message;
  }
  return `${label}: request failed`;
}
