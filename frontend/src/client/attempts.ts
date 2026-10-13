// One retained immutable attempt per submitted, compacted or forked work until
// acceptance, rejection or abandonment. Acceptance is the attempt ID appearing
// in an accepted hydration (or, for a Fork, a destination naming its source
// Session); absence is not rejection. Nothing is retransmitted automatically.
import type { ContentPart, Hydration } from '../generated/protocol/types.gen';
import type { RestoreOutcome } from './restore';

export type AttemptKind = 'submit' | 'compact' | 'fork';
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
  // Fork-only immutable payload.
  boundaryItemID: string;
  boundaryOperationID: string;
  replacementText: string;
  rewind: boolean;
  // Handled rewind outcome; null = not attempted.
  restore: RestoreOutcome | null;
}

export function mintOperationID(): string {
  return Array.from(
    crypto.getRandomValues(new Uint8Array(16)),
    (byte) => byte.toString(16).padStart(2, '0'),
  ).join('');
}

export class AttemptLedger {
  private attemptsValue: ComposerAttempt[] = [];

  constructor(private readonly onChange: () => void) {}

  getAttempts(): ComposerAttempt[] {
    return this.attemptsValue;
  }

  // One attempt's snapshot, or null after settlement/abandonment.
  get(id: string): ComposerAttempt | null {
    return this.attemptsValue.find((attempt) => attempt.id === id) ?? null;
  }

  // Unknown/rejected attempts plus a Fork with an unknown rewind outcome.
  outstanding(sessionID: string): ComposerAttempt[] {
    return this.attemptsValue.filter(
      (attempt) => attempt.sessionID === sessionID
        && (attempt.status === 'unknown' || attempt.status === 'rejected' || attempt.restore?.kind === 'unknown'),
    );
  }

  // ONE attempt producer; callers contribute only their payload fields.
  private insert(kind: AttemptKind, sessionID: string, draftRevision: number, fields: Partial<ComposerAttempt> = {}): ComposerAttempt {
    const attempt: ComposerAttempt = {
      id: mintOperationID(),
      kind,
      mode: fields.mode ?? 'regular',
      content: fields.content ?? [],
      sessionID,
      draftRevision,
      status: 'pending',
      error: null,
      boundaryItemID: fields.boundaryItemID ?? '',
      boundaryOperationID: fields.boundaryOperationID ?? '',
      replacementText: fields.replacementText ?? '',
      rewind: fields.rewind ?? false,
      restore: null,
    };
    this.attemptsValue = [...this.attemptsValue, attempt];
    this.onChange();
    return attempt;
  }

  create(kind: AttemptKind, mode: SubmitMode, content: ContentPart[], sessionID: string, draftRevision: number): ComposerAttempt {
    return this.insert(kind, sessionID, draftRevision, { mode, content });
  }

  createFork(
    sessionID: string,
    boundaryItemID: string,
    boundaryOperationID: string,
    replacementText: string,
    rewind: boolean,
    draftRevision: number,
  ): ComposerAttempt {
    return this.insert('fork', sessionID, draftRevision, { boundaryItemID, boundaryOperationID, replacementText, rewind });
  }

  // Store the handled rewind outcome; an unknown one stops the sequence.
  markRestore(id: string, outcome: RestoreOutcome): void {
    this.update(id, { restore: outcome });
  }

  // A known rejection keeps the draft.
  markRejected(id: string, error: string): void {
    this.update(id, { status: 'rejected', error });
  }

  // A transport failure is displayed as unknown.
  markUnknown(id: string): void {
    this.update(id, { status: 'unknown', error: null });
  }

  markRetrying(id: string): void {
    this.update(id, { status: 'pending', error: null });
  }

  // Drop one attempt; a late completion for it finds nothing.
  remove(id: string): void {
    const next = this.attemptsValue.filter((attempt) => attempt.id !== id);
    if (next.length !== this.attemptsValue.length) {
      this.attemptsValue = next;
      this.onChange();
    }
  }

  // Attempts accepted by the hydration are returned and removed; absence proves nothing.
  adoptHydration(sessionID: string, hydration: Hydration): ComposerAttempt[] {
    const ids = new Set<string>([
      ...hydration.operations.map((operation) => operation.operation_id),
      ...hydration.pending.steering.map((input) => input.operation_id),
      ...hydration.pending.queued.map((input) => input.operation_id),
    ]);
    const lineage = hydration.session.source_session_id;
    const accepted: ComposerAttempt[] = [];
    const kept: ComposerAttempt[] = [];
    for (const attempt of this.attemptsValue) {
      const inSource = attempt.sessionID === sessionID;
      const inDestination = attempt.kind === 'fork'
        && typeof lineage === 'string'
        && lineage !== ''
        && lineage === attempt.sessionID;
      if ((inSource || inDestination) && ids.has(attempt.id)) {
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

  private update(id: string, patch: Partial<Pick<ComposerAttempt, 'status' | 'error' | 'restore'>>): void {
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
