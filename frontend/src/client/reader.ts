// One serialized hydration/history chain per displayed Session: one revision
// floor, one contiguous range, one oldest cursor and one dirty bit. A page older
// than the floor is rejected whole; invalid identities, malformed revisions and
// an exhausted cursor without the expected retained items request repair.
import type {
  ConfigurationRevision,
  ConversationItem,
  HistoryPage,
  Hydration,
  SessionRevision,
  WarningsRevision,
} from '../generated/protocol/types.gen';
import {
  acceptHydration,
  compareConfigurationRevision,
  compareSessionRevision,
  compareWarningsRevision,
  itemID,
  mergeConversation,
  validateRevisionHint,
} from './projection';
import { writable, type Readable, type Subscriber, type Unsubscriber } from 'svelte/store';

// Immutable addressing captured before every read; a completion applies only
// while its stamp still describes the presented view.
export interface ReaderStamp {
  instance: string;
  generation: string;
  selection: number;
  sessionID: string;
}

// One generated read: a decoded value, an ordinary failure or an invalid delivery.
export type ReaderOutcome<T> =
  | { ok: true; value: T }
  | { ok: false; kind: 'ordinary' | 'invalid'; message: string };

export function okRead<T>(value: T): ReaderOutcome<T> {
  return { ok: true, value };
}

export function failRead(kind: 'ordinary' | 'invalid', message: string): ReaderOutcome<never> {
  return { ok: false, kind, message };
}

// The chain's two generated reads; the reader checks stamp and disposal after every await.
export interface ReaderTransport {
  hydration(stamp: ReaderStamp): Promise<ReaderOutcome<Hydration>>;
  history(stamp: ReaderStamp, cursor: string): Promise<ReaderOutcome<HistoryPage>>;
}

export type HintKind = 'session' | 'configuration' | 'warning';
export type HintOutcome = 'covered' | 'dirty' | 'repair';

export function sameStamp(a: ReaderStamp, b: ReaderStamp): boolean {
  return (
    a.instance === b.instance
    && a.generation === b.generation
    && a.selection === b.selection
    && a.sessionID === b.sessionID
  );
}

// Assistant item IDs with at least one pending tool call.
export function unresolvedAssistantIDs(items: ConversationItem[]): string[] {
  const ids: string[] = [];
  for (const item of items) {
    if (item.kind !== 'assistant') {
      continue;
    }
    if (item.tool_calls.some((call) => call.status === undefined)) {
      ids.push(itemID(item));
    }
  }
  return ids;
}

type StepOutcome = 'adopted' | 'stale' | 'error' | 'repair' | 'superseded' | 'stopped';

export class SessionReader implements Readable<SessionReader> {
  // The owner publishes itself after every mutation; subscribers observe the
  // same one state owner, never a copied value record.
  private readonly store = writable<SessionReader>(this);
  private itemsValue: ConversationItem[] = [];
  private hydrationValue: Hydration | null = null;
  private floorValue: SessionRevision | null = null;
  private oldestCursorValue: string | null = null;
  private configurationFloorValue: ConfigurationRevision | null = null;
  private warningsFloorValue: WarningsRevision | null = null;
  private dirtyValue = false;
  private pendingOlder = false;
  private busyValue = false;
  private errorValue: string | null = null;
  private disposedValue = false;
  private chainPromise: Promise<void> = Promise.resolve();

  constructor(
    private readonly transport: ReaderTransport,
    private readonly stamp: () => ReaderStamp,
    private readonly onRepair: (reason: string) => void,
    private readonly onAdopt?: (hydration: Hydration) => void,
  ) {}

  subscribe(run: Subscriber<SessionReader>, invalidate?: () => void): Unsubscriber {
    return this.store.subscribe(run, invalidate);
  }

  getItems(): ConversationItem[] {
    return this.itemsValue;
  }

  getHydration(): Hydration | null {
    return this.hydrationValue;
  }

  getFloor(): SessionRevision | null {
    return this.floorValue;
  }

  getOldestCursor(): string | null {
    return this.oldestCursorValue;
  }

  hasOlder(): boolean {
    return this.oldestCursorValue !== null;
  }

  getError(): string | null {
    return this.errorValue;
  }

  isBusy(): boolean {
    return this.busyValue;
  }

  isDirty(): boolean {
    return this.dirtyValue;
  }

  // The in-flight chain promise the repair owner awaits.
  currentChain(): Promise<void> {
    return this.chainPromise;
  }

  // Install the independently accepted configuration and warning floors.
  setFloors(configuration: ConfigurationRevision | null, warnings: WarningsRevision | null): void {
    this.configurationFloorValue = configuration;
    this.warningsFloorValue = warnings;
  }

  // Newer authoritative state may exist; the chain reads again.
  markDirty(): void {
    if (this.disposedValue) {
      return;
    }
    this.dirtyValue = true;
    this.schedule();
  }

  // Queue an older page through the same chain.
  requestOlder(): void {
    if (this.disposedValue) {
      return;
    }
    this.pendingOlder = true;
    this.schedule();
  }

  // Permanently retire this reader; late completions never emit an orphan adoption.
  dispose(): void {
    this.disposedValue = true;
  }

  // Validate the hint instance first, then dirty only when newer than the
  // accepted hydration; invalid or foreign hints request repair.
  hint(revision: SessionRevision | ConfigurationRevision | WarningsRevision, kind: HintKind): HintOutcome {
    if (this.disposedValue) {
      return 'covered';
    }
    try {
      validateRevisionHint(kind, revision, this.stamp().instance);
    } catch {
      this.repairState('hint revision is malformed, foreign or incomparable');
      return 'repair';
    }
    const hydration = this.hydrationValue;
    if (hydration === null) {
      this.markDirty();
      return 'dirty';
    }
    let comparison: -1 | 0 | 1;
    try {
      if (kind === 'session') {
        comparison = compareSessionRevision(hydration.session_revision, revision as SessionRevision);
      } else if (kind === 'configuration') {
        comparison = compareConfigurationRevision(hydration.configuration_revision, revision as ConfigurationRevision);
      } else {
        comparison = compareWarningsRevision(hydration.warnings_revision, revision as WarningsRevision);
      }
    } catch {
      this.repairState('hint revision is malformed, foreign or incomparable');
      return 'repair';
    }
    if (comparison === -1) {
      this.markDirty();
      return 'dirty';
    }
    return 'covered';
  }

  private current(stamp: ReaderStamp): boolean {
    return !this.disposedValue && sameStamp(stamp, this.stamp());
  }

  private schedule(): void {
    if (!this.busyValue) {
      this.chainPromise = this.runChain();
    } else {
      this.store.set(this);
    }
  }

  private async runChain(): Promise<void> {
    if (this.busyValue || this.disposedValue) {
      return;
    }
    this.busyValue = true;
    this.store.set(this);
    try {
      while (!this.disposedValue) {
        let outcome: StepOutcome;
        if (this.dirtyValue) {
          this.dirtyValue = false;
          outcome = await this.refreshStep();
        } else if (this.pendingOlder) {
          this.pendingOlder = false;
          outcome = await this.olderStep();
        } else {
          return;
        }
        if (outcome === 'repair' || outcome === 'stopped') {
          return;
        }
        if (outcome === 'superseded' && !this.dirtyValue) {
          // A superseded delivery applies nothing; dirty work continues this chain.
          return;
        }
        if (outcome === 'stale') {
          this.dirtyValue = true;
        }
      }
    } finally {
      this.busyValue = false;
      this.store.set(this);
    }
  }

  // Retire this reader before its callback; disposal plus the stamp guard suppress
  // every late completion until replacement.
  private repairState(reason: string): StepOutcome {
    this.disposedValue = true;
    this.itemsValue = [];
    this.hydrationValue = null;
    this.floorValue = null;
    this.oldestCursorValue = null;
    this.errorValue = null;
    this.dirtyValue = false;
    this.pendingOlder = false;
    this.store.set(this);
    this.onRepair(reason);
    return 'repair';
  }

  // Keep one read only while its stamp is current; null means superseded.
  private async readPage<T>(stamp: ReaderStamp, read: Promise<ReaderOutcome<T>>): Promise<ReaderOutcome<T> | null> {
    const outcome = await read;
    return this.current(stamp) ? outcome : null;
  }

  // Invalid delivery repairs; an ordinary error is displayed without re-dirtying.
  private failedReadStep(outcome: ReaderOutcome<never> & { ok: false }, label: string): StepOutcome {
    if (outcome.kind === 'invalid') {
      return this.repairState(label + ' delivery is invalid: ' + outcome.message);
    }
    this.errorValue = outcome.message;
    this.store.set(this);
    return 'error';
  }

  // -1 newer, 0 equal, 1 older (reject whole), null when no floor is accepted.
  private floorAgainst(revision: SessionRevision): -1 | 0 | 1 | null {
    return this.floorValue === null ? null : compareSessionRevision(this.floorValue, revision);
  }

  // Remove the page's identities from the missing set; an invalid identity repairs.
  private encounter(missing: Set<string>, items: ConversationItem[], label: string): StepOutcome | null {
    try {
      for (const item of items) {
        missing.delete(itemID(item));
      }
    } catch {
      return this.repairState(`${label} item identity is invalid`);
    }
    return null;
  }

  private async refreshStep(): Promise<StepOutcome> {
    const stamp = this.stamp();
    const read = await this.readPage(stamp, this.transport.hydration(stamp));
    if (read === null) {
      return 'superseded';
    }
    if (!read.ok) {
      return this.failedReadStep(read, 'hydration');
    }
    const next = read.value;
    try {
      if (this.floorAgainst(next.session_revision) === 1) {
        return 'stale';
      }
    } catch {
      return this.repairState('hydration session revision is malformed, foreign or incomparable');
    }
    let accepts: boolean;
    try {
      accepts = acceptHydration(
        this.hydrationValue,
        next,
        stamp.instance,
        this.configurationFloorValue,
        this.warningsFloorValue,
      );
    } catch {
      return this.repairState('hydration revisions are malformed, foreign or incomparable');
    }
    if (!accepts) {
      return 'stale';
    }
    const cached = this.itemsValue;
    const firstCachedID = cached.length > 0 ? itemID(cached[0]) : null;
    const newestItems = next.conversation.items;
    // ONE set of retained identities to reconnect: latest anchor plus unresolved items.
    const missing = new Set<string>(cached.length > 0 ? [itemID(cached[cached.length - 1])] : []);
    for (const id of unresolvedAssistantIDs(cached)) {
      missing.add(id);
    }
    const encountered = this.encounter(missing, newestItems, 'hydration page');
    if (encountered !== null) {
      return encountered;
    }
    const bridge: ConversationItem[] = [];
    let walkCursor = next.conversation.older_cursor ?? null;
    while (missing.size > 0) {
      if (walkCursor === null) {
        return this.repairState('history cursor exhausted without the expected anchor or unresolved item');
      }
      const pageRead = await this.readPage(stamp, this.transport.history(stamp, walkCursor));
      if (pageRead === null) {
        return 'superseded';
      }
      if (!pageRead.ok) {
        return this.failedReadStep(pageRead, 'history');
      }
      const page = pageRead.value;
      let comparison: -1 | 0 | 1 | null;
      try {
        comparison = this.floorAgainst(page.session_revision);
      } catch {
        return this.repairState('history page revision is malformed, foreign or incomparable');
      }
      if (comparison === 1) {
        return 'stale';
      }
      if (comparison === -1) {
        this.floorValue = page.session_revision;
      }
      bridge.unshift(...page.items);
      const failed = this.encounter(missing, page.items, 'history page');
      if (failed !== null) {
        return failed;
      }
      walkCursor = page.older_cursor ?? null;
    }
    // One commit: re-check acceptance against the advanced floor, then install the
    // merged range; a hydration that no longer passes joins only bridge history.
    let bridgeOnly = false;
    try {
      const comparison = this.floorAgainst(next.session_revision);
      if (comparison === 1) {
        bridgeOnly = true;
      } else if (comparison === null || comparison === -1) {
        this.floorValue = next.session_revision;
      }
      if (!bridgeOnly) {
        bridgeOnly = !acceptHydration(
          this.hydrationValue,
          next,
          stamp.instance,
          this.configurationFloorValue,
          this.warningsFloorValue,
        );
      }
    } catch {
      return this.repairState('hydration revisions became incomparable at commit');
    }
    let merged: ConversationItem[];
    try {
      merged = mergeConversation(cached, bridgeOnly ? bridge : [...bridge, ...newestItems]);
      // Preserve the cursor when the range still starts at the cached head.
      if (!(firstCachedID !== null && merged.length > 0 && itemID(merged[0]) === firstCachedID)) {
        this.oldestCursorValue = walkCursor;
      }
    } catch {
      return this.repairState('conversation pages do not join the cached range');
    }
    if (!this.current(stamp)) {
      return 'superseded';
    }
    this.itemsValue = merged;
    this.errorValue = null;
    if (bridgeOnly) {
      this.store.set(this);
      return 'stale';
    }
    this.hydrationValue = next;
    this.store.set(this);
    this.onAdopt?.(next);
    return 'adopted';
  }
  private async olderStep(): Promise<StepOutcome> {
    const cursor = this.oldestCursorValue;
    if (cursor === null) {
      return 'stopped';
    }
    const stamp = this.stamp();
    const read = await this.readPage(stamp, this.transport.history(stamp, cursor));
    if (read === null) {
      return 'superseded';
    }
    if (!read.ok) {
      return this.failedReadStep(read, 'older page');
    }
    const page = read.value;
    try {
      const comparison = this.floorAgainst(page.session_revision);
      if (comparison === 1) {
        return 'stale';
      }
      if (comparison === -1) {
        this.floorValue = page.session_revision;
      }
    } catch {
      return this.repairState('older page revision is malformed, foreign or incomparable');
    }
    let merged: ConversationItem[];
    try {
      merged = mergeConversation(page.items, this.itemsValue);
    } catch {
      return this.repairState('older page does not connect to the displayed range');
    }
    this.itemsValue = merged;
    this.oldestCursorValue = page.older_cursor ?? null;
    this.errorValue = null;
    this.store.set(this);
    return 'adopted';
  }
}
