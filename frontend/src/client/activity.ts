// Provisional Model activity for one displayed Session: one chronological
// text/refusal range plus temporary tool activity, keyed to the accepted active
// Operation, bounded to a UTF-8-safe trailing 64 KiB. Never persisted or
// replayed; positions are not sequence numbers. Events that do not address the
// accepted Operation are refused by the caller without mutating this state.
import type {
  RefusalDeltaEvent,
  TextDeltaEvent,
  ToolCallStatus,
  ToolFinishedEvent,
  ToolStartedEvent,
} from '../generated/protocol/types.gen';
import { writable, type Readable, type Subscriber, type Unsubscriber } from 'svelte/store';

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
// ignoreBOM keeps a leading U+FEFF as literal content.
const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

function byteLength(text: string): number {
  return encoder.encode(text).length;
}

// The longest UTF-8-safe suffix within limit: decoding starts on a code point
// boundary, so a sequence split at the window front is dropped whole.
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

// Merge one delta into the last piece of the same kind, else append it; no two
// adjacent pieces share a kind.
function appendPiece(pieces: ActivityPiece[], kind: PieceKind, content: string): ActivityPiece[] {
  if (content === '') {
    return pieces;
  }
  const last = pieces[pieces.length - 1];
  if (last !== undefined && last.kind === kind) {
    return [...pieces.slice(0, -1), { kind, content: last.content + content }];
  }
  return [...pieces, { kind, content }];
}

// Drop the oldest content until the range fits the trailing byte limit.
function trimToByteBudget(pieces: ActivityPiece[], limit: number): { pieces: ActivityPiece[]; truncated: boolean } {
  let total = pieces.reduce((sum, piece) => sum + byteLength(piece.content), 0);
  if (total <= limit) {
    return { pieces, truncated: false };
  }
  let start = 0;
  while (start < pieces.length) {
    const piece = pieces[start];
    const size = byteLength(piece.content);
    if (size <= total - limit) {
      total -= size;
      start += 1;
      continue;
    }
    const suffix = keepUtf8Suffix(piece.content, Math.max(0, limit - (total - size)));
    return {
      pieces: suffix === ''
        ? pieces.slice(start + 1)
        : [{ kind: piece.kind, content: suffix }, ...pieces.slice(start + 1)],
      truncated: true,
    };
  }
  return { pieces: [], truncated: true };
}

export class ProvisionalActivity implements Readable<ProvisionalActivity> {
  // The owner publishes itself after every mutation; subscribers observe the
  // same one state owner, never a copied value record.
  private readonly store = writable<ProvisionalActivity>(this);
  private stateValue: ActivityState | null = null;

  subscribe(run: Subscriber<ProvisionalActivity>, invalidate?: () => void): Unsubscriber {
    return this.store.subscribe(run, invalidate);
  }

  getState(): ActivityState | null {
    return this.stateValue;
  }

  // Adopt one event addressing the accepted active Operation; otherwise return
  // false without touching the state and the caller dirties the hydration.
  accept(activeOperationID: string | null, event: ActivityEvent): boolean {
    if (activeOperationID === null || event.scope.operation_id !== activeOperationID) {
      return false;
    }
    const state = this.ensure(event.scope.operation_id);
    if (event.kind === 'text_delta' || event.kind === 'refusal_delta') {
      const kind: PieceKind = event.kind === 'text_delta' ? 'text' : 'refusal';
      const appended = appendPiece(state.pieces, kind, event.content);
      const trimmed = trimToByteBudget(appended, ACTIVITY_BYTE_LIMIT);
      state.pieces = trimmed.pieces;
      state.truncated = state.truncated || trimmed.truncated;
    } else {
      const index = state.tools.findIndex((tool) => tool.callID === event.call_id);
      if (index === -1) {
        state.tools = [...state.tools, event.kind === 'tool_started'
          ? { callID: event.call_id, name: event.name, status: null }
          : { callID: event.call_id, name: '', status: event.status }];
      } else {
        state.tools = state.tools.map((tool, i) => (i === index
          ? { ...tool, name: event.kind === 'tool_started' ? event.name : tool.name, status: event.kind === 'tool_started' ? null : event.status }
          : tool));
      }
    }
    this.store.set(this);
    return true;
  }

  // Drop the provisional area.
  clear(): void {
    if (this.stateValue === null) {
      return;
    }
    this.stateValue = null;
    this.store.set(this);
  }

  private ensure(operationID: string): ActivityState {
    if (this.stateValue === null || this.stateValue.operationID !== operationID) {
      this.stateValue = { operationID, pieces: [], truncated: false, tools: [] };
    }
    return this.stateValue;
  }
}
