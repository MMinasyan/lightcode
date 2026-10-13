// The shared generated-SDK result rule. The generated client returns every
// received status as {data?, error?, response}; only a binding/fetch rejection
// leaves response absent. Classification: absent response = transport loss;
// non-ok = known rejection preserving the generated error/raw payload; 204 =
// valid empty success; 2xx without a result = invalid delivery; else ok.
import type { Error as RuntimeError } from '../generated/protocol/types.gen';
import { errorText } from '../lib/errors.js';

export interface SdkResultLike {
  data?: unknown;
  error?: unknown;
  response?: Response;
}

export type ClassifiedResult =
  | { kind: 'ok'; data: unknown }
  | { kind: 'loss'; message: string }
  | { kind: 'rejection'; message: string; payload: unknown }
  | { kind: 'invalid'; message: string };

// ONE invocation boundary: classify a caller's generated call or its transport loss.
export async function invokeGenerated<C>(
  getClient: () => C,
  label: string,
  call: (client: C) => Promise<SdkResultLike>,
): Promise<ClassifiedResult> {
  try {
    return classifyResult(await call(getClient()), label);
  } catch (error) {
    return { kind: 'loss', message: `${label}: ${errorText(error)}` };
  }
}

// Preserve the rejected body's text; fall back to the label only when it carries none.
function rejectionMessage(error: unknown, label: string): string {
  const generated = error as RuntimeError | undefined;
  if (typeof generated?.message === 'string' && generated.message !== '') {
    return generated.message;
  }
  if (typeof error === 'string' && error !== '') {
    return error;
  }
  if (error instanceof Error && error.message !== '') {
    return error.message;
  }
  return `${label}: request failed`;
}

export function classifyResult(result: SdkResultLike, label: string): ClassifiedResult {
  if (result.response === undefined) {
    return { kind: 'loss', message: `${label}: transport unavailable` };
  }
  if (!result.response.ok) {
    return { kind: 'rejection', message: rejectionMessage(result.error, label), payload: result.error };
  }
  // 204 is an explicit valid success, never an invalid delivery.
  if (result.response.status === 204) {
    return { kind: 'ok', data: result.data ?? {} };
  }
  if (result.data === undefined || result.data === null) {
    return { kind: 'invalid', message: `${label}: received success without a result` };
  }
  return { kind: 'ok', data: result.data };
}
