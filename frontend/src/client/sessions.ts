// Session chronology ordering for the desktop navigation. The producer emits
// `last_activity` from a UTC time.Time, which encoding/json writes as
// RFC3339Nano: a variable-length fractional second with trailing zeros trimmed
// (".1", ".000000001", or no fraction at all). Comparing those strings raw is
// wrong — "…:00Z" sorts after "…:00.5Z" though the instant is earlier. So
// normalize the fractional suffix to exactly nine digits before comparing the
// instant, then order descending by instant and ascending by Session ID on an
// exact tie. No locale sorter, no millisecond rounding, no clock policy.

// normalizeInstant returns the timestamp with its fractional second padded to
// nine digits, so lexicographic order equals chronological order. A value the
// producer did not shape this way is compared as-is (never thrown).
export function normalizeInstant(timestamp: string): string {
  const match = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})?$/.exec(timestamp);
  if (match === null) {
    return timestamp;
  }
  const fraction = match[2] === undefined ? '' : match[2].slice(1);
  const suffix = match[3] === undefined ? '' : match[3];
  return `${match[1]}.${fraction.padEnd(9, '0')}${suffix}`;
}

// compareSessionActivity orders two Sessions descending by normalized
// last_activity, then ascending by session_id on an exact-tied instant.
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

// sortOpenRoots filters out child headers and applies the shared chronology
// order used by the initial selection, the Session picker and lineage lists.
export function sortOpenRoots<T extends { last_activity: string; session_id: string; parent_session_id?: string }>(
  sessions: T[],
): T[] {
  return sessions
    .filter((session) => !session.parent_session_id)
    .sort(compareSessionActivity);
}
