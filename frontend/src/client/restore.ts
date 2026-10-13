// The shared code-restore result rule. Both endpoints answer one generated
// union: a 200 result without an error member, a non-2xx partial result with
// restored/skipped arrays and its Error, or a plain non-2xx Error. Anything
// else is outcome unknown, never a fabricated protocol error or empty success.
import type {
  CodeRevertResult,
  Error as RuntimeError,
} from '../generated/protocol/types.gen';
import type { ClassifiedResult } from './results';

// The generated closed error-code enum as an exhaustive runtime record.
export const ERROR_CODES: Record<RuntimeError['code'], true> = {
  invalid: true,
  unauthorized: true,
  not_found: true,
  conflict: true,
  corrupt: true,
  closed: true,
  configuration: true,
  storage: true,
  internal: true,
};

export type RestoreOutcome =
  | { kind: 'ok'; result: CodeRevertResult }
  | { kind: 'failed'; result: CodeRevertResult | null; message: string }
  | { kind: 'unknown'; message: string };

function isRuntimeError(value: unknown): value is RuntimeError {
  if (value === null || typeof value !== 'object') return false;
  const candidate = value as { code?: unknown; message?: unknown };
  return (
    typeof candidate.code === 'string'
    && Object.prototype.hasOwnProperty.call(ERROR_CODES, candidate.code)
    && typeof candidate.message === 'string'
  );
}

function isSkippedEntry(value: unknown): boolean {
  if (value === null || typeof value !== 'object') return false;
  const candidate = value as { path?: unknown; reason?: unknown };
  return typeof candidate.path === 'string' && typeof candidate.reason === 'string';
}

// The consumed restore shape: non-null restored strings and skipped {path,reason} entries.
function hasRestoreArrays(value: unknown): value is CodeRevertResult {
  if (value === null || typeof value !== 'object') return false;
  const candidate = value as { restored?: unknown; skipped?: unknown };
  return (
    Array.isArray(candidate.restored)
    && candidate.restored.every((entry) => typeof entry === 'string')
    && Array.isArray(candidate.skipped)
    && candidate.skipped.every(isSkippedEntry)
  );
}

// A payload carrying any result-shape member is the partial branch first.
function hasRestoreResultMember(value: unknown): boolean {
  if (value === null || typeof value !== 'object') return false;
  const candidate = value as Record<string, unknown>;
  return 'restored' in candidate || 'skipped' in candidate || 'error' in candidate;
}

// A body matching neither declared branch is one uniform invalid delivery.
function invalidBody(label: string): RestoreOutcome {
  return { kind: 'unknown', message: `${label}: restore body is not a decodable result` };
}

// Map one classified call onto the restore union: a clean success, a partial
// result as a known failure with its file effects, a plain Error as a known
// failure without effects, everything else invalid.
export function classifyRestore(classified: ClassifiedResult, label: string): RestoreOutcome {
  if (classified.kind === 'ok') {
    const data = classified.data;
    if (!hasRestoreArrays(data) || 'error' in (data as Record<string, unknown>)) {
      return invalidBody(label);
    }
    return { kind: 'ok', result: data };
  }
  if (classified.kind === 'rejection') {
    const payload = classified.payload;
    if (hasRestoreResultMember(payload)) {
      if (!hasRestoreArrays(payload) || !isRuntimeError(payload.error)) {
        return invalidBody(label);
      }
      return { kind: 'failed', result: payload, message: payload.error.message };
    }
    if (isRuntimeError(payload)) {
      return { kind: 'failed', result: null, message: payload.message };
    }
    return invalidBody(label);
  }
  return { kind: 'unknown', message: classified.message };
}

// Render the restored paths, skipped paths with reasons and the error message.
export function restoreLines(outcome: RestoreOutcome): string[] {
  if (outcome.kind === 'unknown') {
    return [`Rewind outcome unknown: ${outcome.message}`];
  }
  const lines: string[] = [];
  const result = outcome.result;
  if (result !== null) {
    lines.push(result.restored.length > 0 ? `Restored: ${result.restored.join(', ')}` : 'Restored: (none)');
    for (const skipped of result.skipped) {
      lines.push(`Skipped: ${skipped.path} (${skipped.reason})`);
    }
  }
  if (outcome.kind === 'failed') {
    lines.push(`Restore failed: ${outcome.message}`);
  }
  return lines;
}
