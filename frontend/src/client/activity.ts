// Provisional Model activity for one displayed Session: ONE combined
// chronological text/refusal range plus temporary tool activity, all keyed to
// the Session's accepted active Operation. It is never appended to a hydrated
// assistant item, never persisted and never replayed after a read. Positions
// and ordinals are not sequence numbers; repeated positions remain progress.
// The single range is bounded to a UTF-8-safe trailing 64 KiB (one visible
// truncation marker for the whole range); refusal pieces stay visibly distinct
// from text pieces but share the one budget. Events that do not address the
// accepted active Operation are refused by the caller (which dirties the
// hydration) without mutating this state.
import type {
  RefusalDeltaEvent,
  TextDeltaEvent,
  ToolCallStatus,
  ToolFinishedEvent,
  ToolStartedEvent,
} from '../generated/protocol/types.gen';

export const ACTIVITY_BYTE_LIMIT = 64 * 1024;

export interface ToolActivity {
  callID: string;
  name: string;
  status: ToolCallStatus | null;
}

export type PieceKind = 'text' | 'refusal';

export interface ActivityPiece {
  kind: PieceKind;
  content: string;
}

export interface ActivityState {
  operationID: string;
  pieces: ActivityPiece[];
  truncated: boolean;
  tools: ToolActivity[];
}

export type ActivityEvent = TextDeltaEvent | RefusalDeltaEvent | ToolStartedEvent | ToolFinishedEvent;

const encoder = new TextEncoder();
// ignoreBOM keeps a legitimate leading U+FEFF as literal content instead of
// letting the decoder strip it as an encoding marker.
const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

function byteLength(text: string): number {
  return encoder.encode(text).length;
}

// keepUtf8Suffix returns the longest UTF-8-safe suffix of text whose byte
// length is at most limit. The byte slice is advanced past any leading UTF-8
// continuation bytes so decoding starts on a code point boundary: a sequence
// genuinely split at the window front is dropped whole rather than decoded to
// a bogus leading U+FFFD. A literal U+FFFD or U+FEFF fully inside the window is
// preserved as real content and never stripped.
export function keepUtf8Suffix(text: string, limit: number): string {
  const bytes = encoder.encode(text);
  if (bytes.length <= limit) {
    return text;
  }
  let start = bytes.length - limit;
  while (start < bytes.length && (bytes[start] & 0xc0) === 0x80) {
    start += 1;
  }
  return decoder.decode(bytes.subarray(start));
}

// combineAdjacent merges runs of same-kind pieces so the range stays a minimal
// chronological list of nonempty text/refusal segments.
function combineAdjacent(pieces: ActivityPiece[]): ActivityPiece[] {
  const out: ActivityPiece[] = [];
  for (const piece of pieces) {
    if (piece.content === '') {
      continue;
    }
    const last = out[out.length - 1];
    if (last && last.kind === piece.kind) {
      out[out.length - 1] = { kind: last.kind, content: last.content + piece.content };
    } else {
      out.push({ kind: piece.kind, content: piece.content });
    }
  }
  return out;
}

// trimToByteBudget drops the oldest content until the whole range fits in the
// trailing byte limit; reports whether anything was trimmed.
function trimToByteBudget(pieces: ActivityPiece[], limit: number): { pieces: ActivityPiece[]; truncated: boolean } {
  let total = pieces.reduce((sum, piece) => sum + byteLength(piece.content), 0);
  if (total <= limit) {
    return { pieces, truncated: false };
  }
  let truncated = false;
  const kept = [...pieces];
  while (kept.length > 0) {
    const first = kept[0];
    const size = byteLength(first.content);
    if (size <= total - limit) {
      // The whole oldest piece can go.
      total -= size;
      truncated = true;
      kept.shift();
      continue;
    }
    // Trim the oldest piece down to the remaining budget.
    const keepForFirst = limit - (total - size);
    const suffix = keepUtf8Suffix(first.content, Math.max(0, keepForFirst));
    truncated = true;
    if (suffix === '') {
      kept.shift();
    } else {
      kept[0] = { kind: first.kind, content: suffix };
    }
    break;
  }
  return { pieces: combineAdjacent(kept), truncated };
}

export class ProvisionalActivity {
  private stateValue: ActivityState | null = null;

  constructor(private readonly onChange: () => void) {}

  getState(): ActivityState | null {
    return this.stateValue;
  }

  // accept adopts one event when it addresses the accepted active Operation;
  // otherwise it returns false without touching the state, and the caller
  // dirties the hydration and discards the event.
  accept(activeOperationID: string | null, event: ActivityEvent): boolean {
    if (activeOperationID === null || event.scope.operation_id !== activeOperationID) {
      return false;
    }
    const state = this.ensure(event.scope.operation_id);
    if (event.kind === 'text_delta' || event.kind === 'refusal_delta') {
      const kind: PieceKind = event.kind === 'text_delta' ? 'text' : 'refusal';
      if (event.content !== '') {
        const appended = combineAdjacent([...state.pieces, { kind, content: event.content }]);
        const trimmed = trimToByteBudget(appended, ACTIVITY_BYTE_LIMIT);
        state.pieces = trimmed.pieces;
        state.truncated = state.truncated || trimmed.truncated;
      }
    } else if (event.kind === 'tool_started') {
      const existing = state.tools.find((tool) => tool.callID === event.call_id);
      if (existing) {
        existing.name = event.name;
        existing.status = null;
      } else {
        state.tools = [...state.tools, { callID: event.call_id, name: event.name, status: null }];
      }
    } else {
      const existing = state.tools.find((tool) => tool.callID === event.call_id);
      if (existing) {
        existing.status = event.status;
      } else {
        state.tools = [...state.tools, { callID: event.call_id, name: '', status: event.status }];
      }
    }
    this.onChange();
    return true;
  }

  // clear drops the provisional area: accepted hydration, selection change,
  // repair and terminal Operation replacement all call it.
  clear(): void {
    if (this.stateValue === null) {
      return;
    }
    this.stateValue = null;
    this.onChange();
  }

  private ensure(operationID: string): ActivityState {
    if (this.stateValue === null || this.stateValue.operationID !== operationID) {
      this.stateValue = { operationID, pieces: [], truncated: false, tools: [] };
    }
    return this.stateValue;
  }
}
