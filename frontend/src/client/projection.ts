// Pure scalar projection rules for the target clients: component-wise session
// revision ordering, same-instance hydration acceptance, conversation item
// identity extraction and merging of already-connected ordered ranges. The
// module holds no state and performs no I/O; it mirrors internal/clientview,
// and the desktop and terminal readers supply their own ranges and acceptance
// decisions when they land.
import type {
  ConfigurationRevision,
  ConversationItem,
  Hydration,
  SessionRevision,
  WarningsRevision,
} from '../generated/protocol/types.gen';

const INSTANCE_ID = /^[0-9a-f]{32}$/;
const DECIMAL_DIGITS = /^[0-9]+$/;
const MAX_INT64 = 9223372036854775807n;
const MAX_UINT64 = 18446744073709551615n;
const INVALID_OVERLAP = 'conversation ranges have an inconsistent or reversed overlap';

interface SessionClock {
  instance: string;
  durable: bigint;
  local: bigint;
}

interface CounterClock {
  instance: string;
  value: bigint;
}

// CompareSessionRevision returns -1, 0 or 1 when a is older than, equal to or
// newer than b under the component-wise durable/local partial order. A pair
// that is newer in one component and older in the other is incomparable, and
// different instances, malformed components or out-of-range values throw,
// never a fabricated order.
export function compareSessionRevision(a: SessionRevision, b: SessionRevision): -1 | 0 | 1 {
  return compareSessionClocks(
    parseSessionClock(a, 'left session revision'),
    parseSessionClock(b, 'right session revision'),
  );
}

// AcceptHydration reports whether the complete top-level Session,
// configuration and warning clocks of next are at least as new as those of the
// previously accepted current hydration and the independently accepted
// configuration and warning floors. Equal clocks are accepted; a next clock
// older than any compared clock is stale and returns false; malformed, foreign
// or incomparable clocks throw. instance is the mandatory authenticated runtime
// instance: it must be a lowercase 32-hex id and every compared revision and
// optional floor must belong to it, so an empty, malformed or foreign expected
// or body identity throws, never a silent bypass. The optional floors are
// validated even when current is null. Only these canonical top-level clocks
// govern acceptance; the nested Session header, Session IDs and every other
// DTO member are not validated or compared here. Neither current nor next is
// retained or mutated.
export function acceptHydration(
  current: Hydration | null,
  next: Hydration,
  instance: string,
  configuration: ConfigurationRevision | null = null,
  warnings: WarningsRevision | null = null,
): boolean {
  const nextSession = parseSessionClock(next.session_revision, 'next session revision');
  const nextConfiguration = parseConfigurationClock(next.configuration_revision, 'next configuration revision');
  const nextWarnings = parseWarningsClock(next.warnings_revision, 'next warnings revision');
  const instances = [nextSession.instance, nextConfiguration.instance, nextWarnings.instance, instance];
  let currentSession: SessionClock | null = null;
  let currentConfiguration: CounterClock | null = null;
  let currentWarnings: CounterClock | null = null;
  if (current !== null) {
    currentSession = parseSessionClock(current.session_revision, 'current session revision');
    currentConfiguration = parseConfigurationClock(current.configuration_revision, 'current configuration revision');
    currentWarnings = parseWarningsClock(current.warnings_revision, 'current warnings revision');
    instances.push(currentSession.instance, currentConfiguration.instance, currentWarnings.instance);
  }
  let configurationFloor: CounterClock | null = null;
  if (configuration !== null) {
    configurationFloor = parseConfigurationClock(configuration, 'configuration floor');
    instances.push(configurationFloor.instance);
  }
  let warningsFloor: CounterClock | null = null;
  if (warnings !== null) {
    warningsFloor = parseWarningsClock(warnings, 'warnings floor');
    instances.push(warningsFloor.instance);
  }
  for (const candidate of instances) {
    if (candidate !== instances[0]) {
      throw new Error('revisions belong to different runtime instances');
    }
  }
  if (currentSession !== null && currentConfiguration !== null && currentWarnings !== null) {
    if (compareSessionClocks(currentSession, nextSession) > 0) {
      return false;
    }
    if (currentConfiguration.value > nextConfiguration.value) {
      return false;
    }
    if (currentWarnings.value > nextWarnings.value) {
      return false;
    }
  }
  if (configurationFloor !== null && configurationFloor.value > nextConfiguration.value) {
    return false;
  }
  if (warningsFloor !== null && warningsFloor.value > nextWarnings.value) {
    return false;
  }
  return true;
}

// ValidateRevisionHint validates exactly one delivered revision-hint scalar —
// a session, configuration or warning revision — against the authenticated
// runtime instance. It reuses the existing scalar parsers, so it enforces the
// same well-formedness (decimal, in-range, lowercase 32-hex instance) and it
// validates only the revision: never a whole event, DTO or any other member.
// It throws on a malformed, out-of-range or foreign revision and returns void
// otherwise. The connection delivery boundary and the standalone reader hint
// path share this one rule, so a delivered revision's validity can never be
// postponed until a view is accepted or an initialized reader exists.
export function validateRevisionHint(
  kind: 'session' | 'configuration' | 'warning',
  revision: SessionRevision | ConfigurationRevision | WarningsRevision,
  instance: string,
): void {
  let actual: string;
  if (kind === 'session') {
    actual = parseSessionClock(revision as SessionRevision, 'session hint revision').instance;
  } else if (kind === 'configuration') {
    actual = parseConfigurationClock(revision as ConfigurationRevision, 'configuration hint revision').instance;
  } else {
    actual = parseWarningsClock(revision as WarningsRevision, 'warning hint revision').instance;
  }
  const expected = parseInstance(instance, 'authenticated instance');
  if (actual !== expected) {
    throw new Error(`${kind} hint revision belongs to a different runtime instance`);
  }
}

// CompareConfigurationRevision returns -1, 0 or 1 when a is older than, equal
// to or newer than b under the single configuration generation. Different
// instances, malformed members or out-of-range values throw, never a
// fabricated order. The reader uses this to decide whether a configuration
// hint is covered by the accepted hydration.
export function compareConfigurationRevision(a: ConfigurationRevision, b: ConfigurationRevision): -1 | 0 | 1 {
  return compareCounterClocks(
    parseConfigurationClock(a, 'left configuration revision'),
    parseConfigurationClock(b, 'right configuration revision'),
    'configuration revisions',
  );
}
// CompareWarningsRevision returns -1, 0 or 1 when a is older than, equal to or
// newer than b under the single warnings revision. Different instances,
// malformed members or out-of-range values throw, never a fabricated order.
// The reader uses this to decide whether a warning hint is covered by the
// accepted hydration.
export function compareWarningsRevision(a: WarningsRevision, b: WarningsRevision): -1 | 0 | 1 {
  return compareCounterClocks(
    parseWarningsClock(a, 'left warnings revision'),
    parseWarningsClock(b, 'right warnings revision'),
    'warnings revisions',
  );
}

// ItemID returns the nonempty item_id of one generated conversation item
// variant. It reads the closed discriminator and then the matching member, so
// unknown kinds and non-string identities throw; it validates no other item
// field.
export function itemID(item: ConversationItem): string {
  const candidate = item as { kind?: unknown; item_id?: unknown };
  switch (candidate.kind) {
    case 'input':
    case 'assistant':
    case 'signal':
    case 'compaction':
    case 'operation_end':
      if (typeof candidate.item_id !== 'string' || candidate.item_id === '') {
        throw new Error(`conversation item ${candidate.kind} has an empty item_id`);
      }
      return candidate.item_id;
    default:
      throw new Error(`unknown conversation item kind ${String(candidate.kind)}`);
  }
}

// MergeConversation combines two caller-certified connected, ordered ranges by
// stable item identity: the values of the supplied newer range replace the
// shared identities, the prefix of the range starting earlier is kept, and the
// suffix that extends farther is kept. Disjoint ranges concatenate older then
// newer and empty ranges are legal. Repeated identities inside one input and
// inconsistent or reversed overlaps throw. The result is a fresh array;
// neither input is mutated, decoded, sorted or retained.
export function mergeConversation(older: ConversationItem[], newer: ConversationItem[]): ConversationItem[] {
  const olderIDs = conversationItemIDs(older);
  const newerIDs = conversationItemIDs(newer);
  const newerPositions = new Map<string, number>();
  newerIDs.forEach((id, index) => newerPositions.set(id, index));
  let start = -1;
  let newerStart = -1;
  for (let i = 0; i < olderIDs.length; i += 1) {
    const position = newerPositions.get(olderIDs[i]);
    if (position !== undefined) {
      start = i;
      newerStart = position;
      break;
    }
  }
  if (start < 0) {
    return [...older, ...newer];
  }
  let length = 0;
  while (
    start + length < olderIDs.length
    && newerStart + length < newerIDs.length
    && olderIDs[start + length] === newerIDs[newerStart + length]
  ) {
    length += 1;
  }
  const olderPositions = new Map<string, number>();
  olderIDs.forEach((id, index) => olderPositions.set(id, index));
  for (let i = 0; i < olderIDs.length; i += 1) {
    if (newerPositions.has(olderIDs[i]) && (i < start || i >= start + length)) {
      throw new Error(INVALID_OVERLAP);
    }
  }
  for (let j = 0; j < newerIDs.length; j += 1) {
    if (olderPositions.has(newerIDs[j]) && (j < newerStart || j >= newerStart + length)) {
      throw new Error(INVALID_OVERLAP);
    }
  }
  if (start > 0 && newerStart > 0) {
    throw new Error(INVALID_OVERLAP);
  }
  if (start + length < olderIDs.length && newerStart + length < newerIDs.length) {
    throw new Error(INVALID_OVERLAP);
  }
  const prefix = start > 0 ? older.slice(0, start) : newer.slice(0, newerStart);
  const suffix = start + length < olderIDs.length
    ? older.slice(start + length)
    : newer.slice(newerStart + length);
  return [...prefix, ...newer.slice(newerStart, newerStart + length), ...suffix];
}

function compareSessionClocks(a: SessionClock, b: SessionClock): -1 | 0 | 1 {
  if (a.instance !== b.instance) {
    throw new Error('session revisions belong to different runtime instances');
  }
  if (a.durable === b.durable && a.local === b.local) {
    return 0;
  }
  if (a.durable <= b.durable && a.local <= b.local) {
    return -1;
  }
  if (a.durable >= b.durable && a.local >= b.local) {
    return 1;
  }
  throw new Error('session revision components are incomparable');
}

function compareCounterClocks(a: CounterClock, b: CounterClock, label: string): -1 | 0 | 1 {
  if (a.instance !== b.instance) {
    throw new Error(`${label} belong to different runtime instances`);
  }
  if (a.value === b.value) {
    return 0;
  }
  return a.value < b.value ? -1 : 1;
}

function parseSessionClock(revision: SessionRevision, label: string): SessionClock {
  return {
    instance: parseInstance(revision.instance_id, label),
    durable: parseDecimal(revision.durable_revision, `${label} durable_revision`, 1n, MAX_INT64),
    local: parseDecimal(revision.local_revision, `${label} local_revision`, 0n, MAX_UINT64),
  };
}

function parseConfigurationClock(revision: ConfigurationRevision, label: string): CounterClock {
  return {
    instance: parseInstance(revision.instance_id, label),
    value: parseDecimal(revision.generation, `${label} generation`, 0n, MAX_UINT64),
  };
}

function parseWarningsClock(revision: WarningsRevision, label: string): CounterClock {
  return {
    instance: parseInstance(revision.instance_id, label),
    value: parseDecimal(revision.revision, `${label} revision`, 0n, MAX_UINT64),
  };
}

function parseInstance(value: unknown, label: string): string {
  if (typeof value !== 'string' || !INSTANCE_ID.test(value)) {
    throw new Error(`${label} instance_id is not a lowercase 32-character hex id`);
  }
  return value;
}

function parseDecimal(value: unknown, label: string, min: bigint, max: bigint): bigint {
  if (typeof value !== 'string') {
    throw new Error(`${label} must be a decimal digit string`);
  }
  if (!DECIMAL_DIGITS.test(value)) {
    throw new Error(`${label} is not a nonempty decimal digit string`);
  }
  const parsed = BigInt(value);
  if (parsed < min || parsed > max) {
    throw new Error(`${label} is out of range`);
  }
  return parsed;
}

function conversationItemIDs(items: ConversationItem[]): string[] {
  const ids = items.map((item) => itemID(item));
  if (new Set(ids).size !== ids.length) {
    throw new Error('conversation range repeats an item_id');
  }
  return ids;
}
