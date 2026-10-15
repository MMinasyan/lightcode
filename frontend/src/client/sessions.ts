// Session chronology ordering. The producer emits RFC3339Nano last_activity
// with a variable-length trimmed fractional second, so the fraction is padded
// to nine digits before comparing instants: descending instant, then ascending
// Session ID on an exact tie.

// Pad the fractional second to nine digits so lexicographic order equals
// chronological order; an unshaped value is compared as-is.
export function normalizeInstant(timestamp: string): string {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})?$/.exec(timestamp);
  if (match === null) {
    return timestamp;
  }
  const fraction = match[2] === undefined ? '' : match[2].slice(1);
  const suffix = match[3] === undefined ? '' : match[3];
  return `${match[1]}.${fraction.padEnd(9, '0')}${suffix}`;
}

// Descending normalized last_activity, then ascending session_id on a tie.
export function compareSessionActivity(
  a: { last_activity: string; session_id: string },
  b: { last_activity: string; session_id: string },
): number {
  const left = normalizeInstant(a.last_activity);
  const right = normalizeInstant(b.last_activity);
  if (left !== right) {
    return left < right ? 1 : -1;
  }
  if (a.session_id === b.session_id) {
    return 0;
  }
  return a.session_id < b.session_id ? -1 : 1;
}

// Drop child headers and apply the shared chronology order.
export function sortOpenRoots<T extends { last_activity: string; session_id: string; parent_session_id?: string }>(
  sessions: T[],
): T[] {
  return sessions
    .filter((session) => !session.parent_session_id)
    .sort(compareSessionActivity);
}
