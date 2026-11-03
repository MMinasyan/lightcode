// Pure scalar projection rules: clock ordering, hydration acceptance, identity
// and connected-range merging. No state.
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

type Component = { member: string; min: bigint; max: bigint };

// Session components in comparison order: durable is a positive signed int64,
// local a uint64; every other clock is one uint64 counter member.
const SESSION_COMPONENTS: readonly Component[] = [
  { member: 'durable_revision', min: 1n, max: MAX_INT64 },
  { member: 'local_revision', min: 0n, max: MAX_UINT64 },
];
const CONFIGURATION_COMPONENTS: readonly Component[] = [{ member: 'generation', min: 0n, max: MAX_UINT64 }];
const WARNINGS_COMPONENTS: readonly Component[] = [{ member: 'revision', min: 0n, max: MAX_UINT64 }];

// One parsed revision: instance plus its components.
interface Clock {
  instance: string;
  values: bigint[];
}

// Parse lowercase 32-hex instance and every component as an in-range decimal
// digit string, exactly, with BigInt.
function parseClock(revision: unknown, label: string, components: readonly Component[]): Clock {
  const source = revision as Record<string, unknown>;
  const instance = source?.instance_id;
  if (typeof instance !== 'string' || !INSTANCE_ID.test(instance)) {
    throw new Error(`${label} instance_id is not a lowercase 32-character hex id`);
  }
  const values = components.map(({ member, min, max }) => {
    const value = source[member];
    if (typeof value !== 'string' || !DECIMAL_DIGITS.test(value)) {
      throw new Error(`${label} ${member} is not a nonempty decimal digit string`);
    }
    const parsed = BigInt(value);
    if (parsed < min || parsed > max) {
      throw new Error(`${label} ${member} is out of range`);
    }
    return parsed;
  });
  return { instance, values };
}

// Component-wise comparator; a pair newer in one component and older in another
// is incomparable (instance/range were checked by parseClock).
function compareClocks(a: Clock, b: Clock): -1 | 0 | 1 {
  if (a.instance !== b.instance) {
    throw new Error('revisions belong to different runtime instances');
  }
  let direction: -1 | 0 | 1 = 0;
  for (let i = 0; i < a.values.length; i += 1) {
    const step = a.values[i] < b.values[i] ? -1 : a.values[i] > b.values[i] ? 1 : 0;
    if (step === 0) {
      continue;
    }
    if (direction !== 0 && direction !== step) {
      throw new Error('session revision components are incomparable');
    }
    direction = step;
  }
  return direction;
}

// -1/0/1 under the component-wise durable/local order; incomparable pairs throw.
export function compareSessionRevision(a: SessionRevision, b: SessionRevision): -1 | 0 | 1 {
  return compareClocks(
    parseClock(a, 'left session revision', SESSION_COMPONENTS),
    parseClock(b, 'right session revision', SESSION_COMPONENTS),
  );
}

// Whether next's top-level clocks are at least as new as the accepted hydration
// and independent floors; every supplied clock validates before any stale result.
export function acceptHydration(
  current: Hydration | null,
  next: Hydration,
  instance: string,
  configuration: ConfigurationRevision | null = null,
  warnings: WarningsRevision | null = null,
): boolean {
  const nextSession = parseClock(next.session_revision, 'next session revision', SESSION_COMPONENTS);
  const nextConfiguration = parseClock(next.configuration_revision, 'next configuration revision', CONFIGURATION_COMPONENTS);
  const nextWarnings = parseClock(next.warnings_revision, 'next warnings revision', WARNINGS_COMPONENTS);
  const expected = parseInstance(instance, 'authenticated instance');
  let currentSession: Clock | null = null;
  let currentConfiguration: Clock | null = null;
  let currentWarnings: Clock | null = null;
  if (current !== null) {
    currentSession = parseClock(current.session_revision, 'current session revision', SESSION_COMPONENTS);
    currentConfiguration = parseClock(current.configuration_revision, 'current configuration revision', CONFIGURATION_COMPONENTS);
    currentWarnings = parseClock(current.warnings_revision, 'current warnings revision', WARNINGS_COMPONENTS);
  }
  const configurationFloor = configuration === null
    ? null
    : parseClock(configuration, 'configuration floor', CONFIGURATION_COMPONENTS);
  const warningsFloor = warnings === null
    ? null
    : parseClock(warnings, 'warnings floor', WARNINGS_COMPONENTS);
  for (const clock of [nextSession, nextConfiguration, nextWarnings, currentSession, currentConfiguration, currentWarnings, configurationFloor, warningsFloor]) {
    if (clock !== null && clock.instance !== expected) {
      throw new Error('revisions belong to different runtime instances');
    }
  }
  if (currentSession !== null && currentConfiguration !== null && currentWarnings !== null) {
    if (compareClocks(currentSession, nextSession) > 0) {
      return false;
    }
    if (compareClocks(currentConfiguration, nextConfiguration) > 0) {
      return false;
    }
    if (compareClocks(currentWarnings, nextWarnings) > 0) {
      return false;
    }
  }
  if (configurationFloor !== null && compareClocks(configurationFloor, nextConfiguration) > 0) {
    return false;
  }
  if (warningsFloor !== null && compareClocks(warningsFloor, nextWarnings) > 0) {
    return false;
  }
  return true;
}

// Validate one delivered hint against the authenticated instance.
export function validateRevisionHint(
  kind: 'session' | 'configuration' | 'warning',
  revision: SessionRevision | ConfigurationRevision | WarningsRevision,
  instance: string,
): void {
  const components = kind === 'session'
    ? SESSION_COMPONENTS
    : (kind === 'configuration' ? CONFIGURATION_COMPONENTS : WARNINGS_COMPONENTS);
  const parsed = parseClock(revision, `${kind} hint revision`, components);
  if (parsed.instance !== parseInstance(instance, 'authenticated instance')) {
    throw new Error(`${kind} hint revision belongs to a different runtime instance`);
  }
}

export function compareConfigurationRevision(a: ConfigurationRevision, b: ConfigurationRevision): -1 | 0 | 1 {
  return compareClocks(
    parseClock(a, 'left configuration revision', CONFIGURATION_COMPONENTS),
    parseClock(b, 'right configuration revision', CONFIGURATION_COMPONENTS),
  );
}

export function compareWarningsRevision(a: WarningsRevision, b: WarningsRevision): -1 | 0 | 1 {
  return compareClocks(
    parseClock(a, 'left warnings revision', WARNINGS_COMPONENTS),
    parseClock(b, 'right warnings revision', WARNINGS_COMPONENTS),
  );
}

// The nonempty item_id of one generated item variant; unknown kinds throw.
export function itemID(item: ConversationItem): string {
  const candidate = item as { kind?: unknown; item_id?: unknown };
  switch (candidate.kind) {
    case 'input':
    case 'assistant':
    case 'signal':
    case 'context_checkpoint':
    case 'operation_end':
      if (typeof candidate.item_id !== 'string' || candidate.item_id === '') {
        throw new Error(`conversation item ${candidate.kind} has an empty item_id`);
      }
      return candidate.item_id;
    default:
      throw new Error(`unknown conversation item kind ${String(candidate.kind)}`);
  }
}

// Combine two connected ordered ranges by stable identity: newer values replace
// shared identities, the farther-reaching ranges supply prefix and suffix.
export function mergeConversation(older: ConversationItem[], newer: ConversationItem[]): ConversationItem[] {
  const olderIDs = conversationItemIDs(older);
  const newerIDs = conversationItemIDs(newer);
  const newerPositions = new Map(newerIDs.map((id, index) => [id, index]));
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
  if (start > 0 && newerStart > 0) {
    throw new Error(INVALID_OVERLAP);
  }
  if (start + length < olderIDs.length && newerStart + length < newerIDs.length) {
    throw new Error(INVALID_OVERLAP);
  }
  for (let i = 0; i < olderIDs.length; i += 1) {
    if (newerPositions.has(olderIDs[i]) && (i < start || i >= start + length)) {
      throw new Error(INVALID_OVERLAP);
    }
  }
  const prefix = start > 0 ? older.slice(0, start) : newer.slice(0, newerStart);
  const suffix = start + length < olderIDs.length
    ? older.slice(start + length)
    : newer.slice(newerStart + length);
  return [...prefix, ...newer.slice(newerStart, newerStart + length), ...suffix];
}

function parseInstance(value: unknown, label: string): string {
  if (typeof value !== 'string' || !INSTANCE_ID.test(value)) {
    throw new Error(`${label} instance_id is not a lowercase 32-character hex id`);
  }
  return value;
}

function conversationItemIDs(items: ConversationItem[]): string[] {
  const ids = items.map((item) => itemID(item));
  if (new Set(ids).size !== ids.length) {
    throw new Error('conversation range repeats an item_id');
  }
  return ids;
}
