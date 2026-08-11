export function normalizeSubagentLinks(value) {
  if (!Array.isArray(value)) return [];
  return value
    .map((link) => {
      const sessionId = link?.sessionId || link?.session_id || '';
      const index = Number(link?.index);
      if (!sessionId || !Number.isFinite(index)) return null;
      return { index, sessionId };
    })
    .filter(Boolean);
}

export function subagentLinksFromMetadata(metadata) {
  if (!metadata) return [];
  return normalizeSubagentLinks(metadata.subagent_session_ids || metadata.subagentSessionIds);
}

export function mergeSubagentLinks(...groups) {
  const merged = [];
  const seen = new Set();
  for (const group of groups) {
    for (const link of normalizeSubagentLinks(group)) {
      const key = `${link.index}\0${link.sessionId}`;
      if (seen.has(key)) continue;
      seen.add(key);
      merged.push(link);
    }
  }
  return merged;
}
