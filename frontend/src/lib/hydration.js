// Transcript high-water gating for complete-state hydration. After applying a
// snapshot, a live transcript item that is already represented in that snapshot
// (its sequence is at or below the snapshot's high-water) must not re-render,
// while every later item applies once. Tool results are delivered id-keyed and
// carry no gating sequence, so they are always admitted and applied by id.

// snapshotHighWater returns the largest sequence represented in a complete-state
// snapshot: the max of the committed cursor and the retained tail and error rows.
// A live transcript item at or below it is already shown by the snapshot.
export function snapshotHighWater(state) {
  let hw = Number(state?.cursor?.committedSeq) || 0;
  for (const row of state?.tail || []) {
    const seq = Number(row?.seq) || 0;
    if (seq > hw) hw = seq;
  }
  for (const row of state?.errors || []) {
    const seq = Number(row?.seq) || 0;
    if (seq > hw) hw = seq;
  }
  return hw;
}

// newTranscriptGate creates a gate seeded from a complete-state snapshot.
export function newTranscriptGate(state) {
  return { highWater: snapshotHighWater(state) };
}

// admitSequenced reports whether a sequenced transcript item is new relative to
// the gate's high-water, advancing the water when it is. An item with no sequence
// (an id-keyed update such as a tool result) is always admitted; the caller
// applies it idempotently by id.
export function admitSequenced(gate, seq) {
  if (seq == null) return true;
  const s = Number(seq);
  if (!Number.isFinite(s) || s <= gate.highWater) return false;
  gate.highWater = s;
  return true;
}

// snapshotMessages builds the ordered display list for a complete-state snapshot:
// the durable committed messages first, then the retained tail rows and retained
// errors merged by their shared display sequence.
export function snapshotMessages(state) {
  const committed = state?.messages || [];
  const live = [
    ...(state?.tail || []),
    ...(state?.errors || []),
  ]
    .filter((row) => row && row.message)
    .sort((a, b) => (Number(a.seq) || 0) - (Number(b.seq) || 0))
    .map((row) => row.message);
  return [...committed, ...live];
}
