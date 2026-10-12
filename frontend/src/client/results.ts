// The shared generated-SDK result rule for the actual desktop consumer. The
// generated client runs with throwOnError=false: a received HTTP status is
// ALWAYS returned as {data?, error?, response}, and only a binding/fetch
// rejection leaves response absent. One classification therefore governs every
// call site instead of Error-instanceof or catch-only loss guessing:
//   - response absent            -> transport loss (outcome unknown, nothing
//                                   was received; never a known rejection)
//   - response not ok            -> known server rejection with the actual
//                                   generated error/raw text
//   - 204 No Content            -> valid success even when data is null/undefined
//   - 2xx without a result      -> invalid delivery (a received success whose
//                                   body could not be decoded)
//   - 2xx with a result          -> ok
import type { Error as RuntimeError } from '../generated/protocol/types.gen';

export interface SdkResultLike {
  data?: unknown;
  error?: unknown;
  response?: Response;
}

export type ClassifiedResult =
  | { kind: 'ok'; data: unknown }
  | { kind: 'loss'; message: string }
  | { kind: 'rejection'; message: string }
  | { kind: 'invalid'; message: string };

// A rejected body's raw text is preserved verbatim; only an error with no
// usable message at all falls back to the caller's label.
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
    return { kind: 'rejection', message: rejectionMessage(result.error, label) };
  }
  // A 204 is an explicit valid success: the endpoint carries no body, so a
  // null/undefined result is expected, never an invalid delivery.
  if (result.response.status === 204) {
    return { kind: 'ok', data: result.data ?? {} };
  }
  if (result.data === undefined || result.data === null) {
    return { kind: 'invalid', message: `${label}: received success without a result` };
  }
  return { kind: 'ok', data: result.data };
}
