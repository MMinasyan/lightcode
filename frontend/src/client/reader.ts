// One serialized hydration/history chain per displayed Session: a single
// conversation revision floor, one contiguous displayed item range, one
// oldest-edge cursor and one dirty bit. Every read captures the current
// (instance, connection generation, selection generation, Session ID) stamp;
// a result that settles under a different stamp — or after this reader was
// reset or disposed — is ignored, success and error alike. The floor governs
// every page: a page older than the floor is rejected whole, a newer page
// advances the floor. Hydration acceptance additionally checks the complete
// top-level projection clocks through projection.acceptHydration, passing the
// independently accepted configuration and warning floors. Malformed,
// foreign or incomparable revisions, invalid delivered item identities and an
// exhausted cursor without the expected anchor request repair. The chain
// drains all recorded dirty and older-page work after every step; an ordinary
// error never self-redirties.
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

// ReaderStamp is the immutable addressing captured before every read: the
// authenticated instance, the connection generation, the view's local
// selection generation and the addressed Session ID. A completion applies
// only while the stamp it captured still describes the presented view.
export interface ReaderStamp {
  instance: string;
  generation: string;
  selection: number;
  sessionID: string;
}

// ReaderOutcome is the transport's explicit classification of one generated
// read: a decoded value, an ordinary failure (transport loss or a received
// error status), or an invalid delivery (a received success without a
// decodable result). The reader routes invalid deliveries to repair/reset and
// displays ordinary ones without re-dirtying; no Error-instanceof or
// catch-only guessing is involved.
export type ReaderOutcome<T> =
  | { ok: true; value: T }
  | { ok: false; kind: 'ordinary' | 'invalid'; message: string };

export function okRead<T>(value: T): ReaderOutcome<T> {
  return { ok: true, value };
}

export function failRead(kind: 'ordinary' | 'invalid', message: string): ReaderOutcome<never> {
  return { ok: false, kind, message };
}

// ReaderTransport performs the two generated reads the chain needs. It never
// applies stamps itself; the reader checks its stamp and permanent disposal
// after every await.
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

// unresolvedAssistantIDs returns the item IDs of assistant items that still
// carry at least one pending tool call (a call whose status is absent). Those
// are retained across an ordinary append and the bridge walk continues until
// every one has been encountered, whether or not it remains pending.
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

function containsID(items: ConversationItem[], id: string): boolean {
  return items.some((item) => itemID(item) === id);
}

type StepOutcome = 'adopted' | 'stale' | 'error' | 'repair' | 'superseded' | 'stopped';

export class SessionReader {
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
    private readonly onChange: () => void,
    private readonly onAdopt?: (hydration: Hydration) => void,
  ) {}

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

  // currentChain exposes the actual in-flight chain promise so the repair
  // owner can await the completion of the reads it launched instead of
  // polling isBusy or adding another queue.
  currentChain(): Promise<void> {
    return this.chainPromise;
  }

  // setFloors installs the independently accepted configuration and warning
  // floors; the next hydration acceptance gates against them before adoption.
  setFloors(configuration: ConfigurationRevision | null, warnings: WarningsRevision | null): void {
    this.configurationFloorValue = configuration;
    this.warningsFloorValue = warnings;
  }

  // markDirty records that newer authoritative state may exist. A hint or a
  // relevant command completion calls it; the serialized chain reads again.
  markDirty(): void {
    if (this.disposedValue) {
      return;
    }
    this.dirtyValue = true;
    this.schedule();
  }

  // requestOlder queues a user-requested older page through the same chain,
  // never a parallel reader. It runs after any in-flight step settles.
  requestOlder(): void {
    if (this.disposedValue) {
      return;
    }
    this.pendingOlder = true;
    this.schedule();
  }

  // A replacement (root switch or repair) disposes the old reader and builds a
  // fresh one; there is no in-place reset. dispose permanently retires this
  // reader and drops late completions, so a replaced reader never emits an
  // orphan adoption.
  dispose(): void {
    this.disposedValue = true;
  }

  // hint routes one delivered revision hint against the accepted hydration
  // clocks: a hint not newer than the accepted hydration is covered and
  // starts no read; a newer hint marks the chain dirty. The delivered revision
  // is validated against the authenticated instance FIRST — before the
  // empty-hydration fast path can bypass validity — so a malformed,
  // out-of-range or foreign hint requests repair, never revision zero.
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
      this.onChange();
    }
  }

  private async runChain(): Promise<void> {
    if (this.busyValue || this.disposedValue) {
      return;
    }
    this.busyValue = true;
    this.onChange();
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
        if (outcome === 'repair' || outcome === 'superseded' || outcome === 'stopped') {
          return;
        }
        if (outcome === 'stale') {
          // The whole body was rejected; the reader stays dirty for the
          // mandated fresh read. An ordinary error never re-dirties itself.
          this.dirtyValue = true;
        }
        // 'adopted' and 'error' fall through: any dirty or older-page work
        // recorded during the step is drained before the chain exits.
      }
    } finally {
      this.busyValue = false;
      this.onChange();
    }
  }

  // repairState permanently retires this reader BEFORE its callback: the
  // affected reader is gone for good and a replacement is built by the caller.
  // Permanent disposal plus the async stamp guard (current) covers the interval
  // before that replacement, suppressing every late completion. This replaces,
  // rather than merely deletes, the old chain-token lifetime.
  private repairState(reason: string): StepOutcome {
    this.disposedValue = true;
    this.itemsValue = [];
    this.hydrationValue = null;
    this.floorValue = null;
    this.oldestCursorValue = null;
    this.errorValue = null;
    this.dirtyValue = false;
    this.pendingOlder = false;
    this.onChange();
    this.onRepair(reason);
    return 'repair';
  }

  private async refreshStep(): Promise<StepOutcome> {
    const stamp = this.stamp();
    const read = await this.transport.hydration(stamp);
    if (!this.current(stamp)) {
      return 'superseded';
    }
    if (!read.ok) {
      if (read.kind === 'invalid') {
        return this.repairState('hydration delivery is invalid: ' + read.message);
      }
      this.errorValue = read.message;
      this.onChange();
      return 'error';
    }
    const next = read.value;
    // The one conversation floor governs acceptance, with no chain-start
    // exemption: a hydration below the floor is rejected whole.
    if (this.floorValue !== null) {
      let comparison: -1 | 0 | 1;
      try {
        comparison = compareSessionRevision(this.floorValue, next.session_revision);
      } catch {
        return this.repairState('hydration session revision is malformed, foreign or incomparable');
      }
      if (comparison === 1) {
        return 'stale';
      }
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
    const latestAnchor = cached.length > 0 ? itemID(cached[cached.length - 1]) : null;
    const unresolved = unresolvedAssistantIDs(cached);
    const newestItems = next.conversation.items;
    let anchorFound = false;
    let unresolvedLeft: string[] = [];
    try {
      anchorFound = latestAnchor === null || containsID(newestItems, latestAnchor);
      unresolvedLeft = unresolved.filter((id) => !containsID(newestItems, id));
    } catch {
      return this.repairState('hydration page item identity is invalid');
    }
    const bridge: ConversationItem[] = [];
    let walkCursor = next.conversation.older_cursor ?? null;
    if (latestAnchor !== null && (!anchorFound || unresolvedLeft.length > 0)) {
      while (!anchorFound || unresolvedLeft.length > 0) {
        if (walkCursor === null) {
          return this.repairState('history cursor exhausted without the expected anchor or unresolved item');
        }
        const pageRead = await this.transport.history(stamp, walkCursor);
        if (!this.current(stamp)) {
          return 'superseded';
        }
        if (!pageRead.ok) {
          if (pageRead.kind === 'invalid') {
            return this.repairState('history delivery is invalid: ' + pageRead.message);
          }
          // An ordinary bridge error preserves the prior contiguous display
          // and reports the error; only continuity loss uses repair.
          this.errorValue = pageRead.message;
          this.onChange();
          return 'error';
        }
        const page = pageRead.value;
        let comparison: -1 | 0 | 1;
        try {
          comparison = compareSessionRevision(this.floorValue as SessionRevision, page.session_revision);
        } catch {
          return this.repairState('history page revision is malformed, foreign or incomparable');
        }
        if (comparison === 1) {
          // An older mid-walk page is rejected whole; the displayed range
          // stays and a fresh hydration is fetched.
          return 'stale';
        }
        if (comparison === -1) {
          // A newer mid-walk page is legal: it advances the floor and its
          // overlapping values win.
          this.floorValue = page.session_revision;
        }
        try {
          bridge.unshift(...page.items);
          if (!anchorFound) {
            anchorFound = containsID(page.items, latestAnchor);
          }
          unresolvedLeft = unresolvedLeft.filter((id) => !containsID(page.items, id));
        } catch {
          return this.repairState('history page item identity is invalid');
        }
        walkCursor = page.older_cursor ?? null;
      }
    }
    // A mid-walk advancement can make the pending hydration body older than
    // the floor: reject its whole projection and newest page, keep the
    // accepted connected history and floor, and fetch fresh hydration.
    let nextBelowFloor = false;
    try {
      if (this.floorValue === null) {
        this.floorValue = next.session_revision;
      } else {
        const comparison = compareSessionRevision(next.session_revision, this.floorValue);
        if (comparison === 1) {
          this.floorValue = next.session_revision;
        } else if (comparison === -1) {
          nextBelowFloor = true;
        }
      }
    } catch {
      return this.repairState('hydration session revision is incomparable with the accepted floor');
    }
    if (nextBelowFloor) {
      return this.adoptJoined(cached, bridge, firstCachedID, walkCursor, stamp);
    }
    return this.adoptHydration(next, cached, bridge, firstCachedID, walkCursor, stamp);
  }

  // A mid-walk or post-walk advancement of the floor or the independent global
  // clocks can make the pending body stale by the time the final bridge settles.
  // Re-run the SAME full acceptance (projection clocks + current global floors
  // + the one conversation floor) immediately before committing any body or
  // evidence: a body that no longer clears them is rejected whole, the accepted
  // connected history/floor/cursor stay, and the reader is left dirty for a
  // fresh hydration. An equal (not older) clock set still accepts.
  private acceptsAtCommit(next: Hydration, stamp: ReaderStamp): boolean {
    if (this.floorValue !== null) {
      const floorComparison = compareSessionRevision(this.floorValue, next.session_revision);
      if (floorComparison === 1) {
        return false;
      }
    }
    return acceptHydration(
      this.hydrationValue,
      next,
      stamp.instance,
      this.configurationFloorValue,
      this.warningsFloorValue,
    );
  }

  // joinInstalled installs a connected merged range only while this reader is
  // still the presented one; the oldest cursor follows the merged geometry:
  // preserved when the merged range still starts at the retained cached head,
  // otherwise the walk's final older_cursor (including an exhausted null).
  private joinInstalled(
    cached: ConversationItem[],
    collected: ConversationItem[],
    firstCachedID: string | null,
    walkCursor: string | null,
  ): ConversationItem[] | null {
    try {
      const merged = mergeConversation(cached, collected);
      if (!(firstCachedID !== null && merged.length > 0 && itemID(merged[0]) === firstCachedID)) {
        this.oldestCursorValue = walkCursor;
      }
      return merged;
    } catch {
      return null;
    }
  }

  // adoptJoined joins an accumulated bridge range onto the cached range at
  // their shared item IDs and installs it without touching the hydration
  // projection or its clocks.
  private adoptJoined(
    cached: ConversationItem[],
    bridge: ConversationItem[],
    firstCachedID: string | null,
    walkCursor: string | null,
    stamp: ReaderStamp,
  ): StepOutcome {
    const merged = this.joinInstalled(cached, bridge, firstCachedID, walkCursor);
    if (merged === null) {
      return this.repairState('bridge pages do not join the cached range');
    }
    if (!this.current(stamp)) {
      return 'superseded';
    }
    this.itemsValue = merged;
    // The retained connected history neither replaces the rejected projection
    // nor its clocks. Any successful step clears the displayed read error.
    this.errorValue = null;
    this.onChange();
    return 'stale';
  }

  private adoptHydration(
    next: Hydration,
    cached: ConversationItem[],
    bridge: ConversationItem[],
    firstCachedID: string | null,
    walkCursor: string | null,
    stamp: ReaderStamp,
  ): StepOutcome {
    let stillAccepted: boolean;
    try {
      stillAccepted = this.acceptsAtCommit(next, stamp);
    } catch {
      return this.repairState('hydration revisions became incomparable at commit');
    }
    if (!stillAccepted) {
      // The body is now stale against the floor/clocks that advanced during the
      // bridge: reject its newest page and projection whole. Retain only the
      // connected bridge history that legitimately joined the cache.
      return this.adoptJoined(cached, bridge, firstCachedID, walkCursor, stamp);
    }
    const merged = this.joinInstalled(cached, [...bridge, ...next.conversation.items], firstCachedID, walkCursor);
    if (merged === null) {
      return this.repairState('conversation pages do not join the cached range');
    }
    if (!this.current(stamp)) {
      return 'superseded';
    }
    this.itemsValue = merged;
    this.hydrationValue = next;
    this.errorValue = null;
    this.onChange();
    this.onAdopt?.(next);
    return 'adopted';
  }

  private async olderStep(): Promise<StepOutcome> {
    const cursor = this.oldestCursorValue;
    if (cursor === null) {
      return 'stopped';
    }
    const stamp = this.stamp();
    const read = await this.transport.history(stamp, cursor);
    if (!this.current(stamp)) {
      return 'superseded';
    }
    if (!read.ok) {
      if (read.kind === 'invalid') {
        return this.repairState('older page delivery is invalid: ' + read.message);
      }
      this.errorValue = read.message;
      this.onChange();
      return 'error';
    }
    const page = read.value;
    if (this.floorValue !== null) {
      let comparison: -1 | 0 | 1;
      try {
        comparison = compareSessionRevision(this.floorValue, page.session_revision);
      } catch {
        return this.repairState('older page revision is malformed, foreign or incomparable');
      }
      if (comparison === 1) {
        return 'stale';
      }
      if (comparison === -1) {
        this.floorValue = page.session_revision;
      }
    }
    let merged: ConversationItem[];
    try {
      merged = mergeConversation(page.items, this.itemsValue);
    } catch {
      return this.repairState('older page does not connect to the displayed range');
    }
    this.itemsValue = merged;
    // An accepted older page always extends the oldest edge: its own
    // older_cursor, including an exhausted null, is the new edge cursor.
    // Any successful step clears the displayed read error.
    this.oldestCursorValue = page.older_cursor ?? null;
    this.errorValue = null;
    this.onChange();
    return 'adopted';
  }
}
