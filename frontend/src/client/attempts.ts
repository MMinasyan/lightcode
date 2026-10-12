// The composer attempt ledger: one retained immutable attempt per submitted
// or compacted work until acceptance, rejection or explicit abandonment. The
// caller Operation ID is minted as 16 crypto-random bytes in lowercase hex.
// Editing content creates a new attempt, never a changed retry payload, and a
// successful disposition clears only the matching submitted draft revision.
// Acceptance is proven by the ID appearing in an accepted hydration's
// operations or pending input; absence is not rejection. Accepted attempts are
// returned once to their consumer for draft clearing and then removed, so
// consumed evidence never accumulates; only rows with real display and retry
// consumers (unknown, rejected) are retained. Nothing is persisted,
// deduplicated or retransmitted automatically.
import type { ContentPart, Hydration } from '../generated/protocol/types.gen';

export type AttemptKind = 'submit' | 'compact';
export type AttemptStatus = 'pending' | 'rejected' | 'unknown';
export type SubmitMode = 'regular' | 'queued';

export interface ComposerAttempt {
  id: string;
  kind: AttemptKind;
  mode: SubmitMode;
  content: ContentPart[];
  sessionID: string;
  draftRevision: number;
  status: AttemptStatus;
  error: string | null;
}

// mintOperationID returns exactly 16 crypto-random bytes as lowercase hex.
export function mintOperationID(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  let out = '';
  for (const byte of bytes) {
    out += byte.toString(16).padStart(2, '0');
  }
  return out;
}

export class AttemptLedger {
  private attemptsValue: ComposerAttempt[] = [];

  constructor(private readonly onChange: () => void) {}

  getAttempts(): ComposerAttempt[] {
    return this.attemptsValue;
  }

  // outstanding returns attempts that still need display: their outcome is
  // unknown or the server rejected them and the draft is retained.
  outstanding(sessionID: string): ComposerAttempt[] {
    return this.attemptsValue.filter(
      (attempt) => attempt.sessionID === sessionID && (attempt.status === 'unknown' || attempt.status === 'rejected'),
    );
  }

  create(kind: AttemptKind, mode: SubmitMode, content: ContentPart[], sessionID: string, draftRevision: number): ComposerAttempt {
    const attempt: ComposerAttempt = {
      id: mintOperationID(),
      kind,
      mode,
      content,
      sessionID,
      draftRevision,
      status: 'pending',
      error: null,
    };
    this.attemptsValue = [...this.attemptsValue, attempt];
    this.onChange();
    return attempt;
  }

  // markRejected records a known server rejection: the draft stays and the
  // generated error is reported.
  markRejected(id: string, error: string): void {
    this.update(id, { status: 'rejected', error });
  }

  // markUnknown records a transport failure: no mutation is retransmitted
  // automatically and the outcome is displayed as unknown.
  markUnknown(id: string): void {
    this.update(id, { status: 'unknown', error: null });
  }

  // markRetrying keeps the same ID for an explicit retry and warns the caller
  // (via the returned attempt) that buffered input can be duplicated.
  markRetrying(id: string): void {
    this.update(id, { status: 'pending', error: null });
  }

  // remove drops one settled or explicitly abandoned attempt. A late
  // completion for a removed ID finds nothing to update: abandonment is never
  // resurrected and consumed acceptance is never retained.
  remove(id: string): void {
    const next = this.attemptsValue.filter((attempt) => attempt.id !== id);
    if (next.length !== this.attemptsValue.length) {
      this.attemptsValue = next;
      this.onChange();
    }
  }

  // adoptHydration is the acceptance evidence: every unresolved attempt whose
  // ID is present in the accepted hydration's operations or pending input is
  // returned to the consumer (for draft clearing by its captured revision)
  // and removed. Absence proves nothing and leaves the attempt untouched.
  adoptHydration(sessionID: string, hydration: Hydration): ComposerAttempt[] {
    const ids = new Set<string>([
      ...hydration.operations.map((operation) => operation.operation_id),
      ...hydration.pending.steering.map((input) => input.operation_id),
      ...hydration.pending.queued.map((input) => input.operation_id),
    ]);
    const accepted: ComposerAttempt[] = [];
    const kept: ComposerAttempt[] = [];
    for (const attempt of this.attemptsValue) {
      if (attempt.sessionID === sessionID && ids.has(attempt.id)) {
        accepted.push(attempt);
      } else {
        kept.push(attempt);
      }
    }
    if (accepted.length > 0) {
      this.attemptsValue = kept;
      this.onChange();
    }
    return accepted;
  }

  private update(id: string, patch: Pick<ComposerAttempt, 'status' | 'error'>): void {
    let changed = false;
    this.attemptsValue = this.attemptsValue.map((attempt) => {
      if (attempt.id !== id) {
        return attempt;
      }
      changed = true;
      return { ...attempt, ...patch };
    });
    if (changed) {
      this.onChange();
    }
  }
}
