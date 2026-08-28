// Pending permission requests keyed by request id. A live permission_request
// event carries camelCase fields (and a pre-negated canSaveProject); a hydration
// snapshot carries the canonical snake_case permission.Request (with the raw
// disable_project_save). normalizePermission reconciles both into the single
// camelCase shape PermissionPrompt reads, and the map keeps one entry per id so a
// request delivered both in the snapshot and as a replayed event is not doubled.

export function normalizePermission(p) {
  if (!p) return null;
  const canSaveProject = p.canSaveProject !== undefined
    ? p.canSaveProject
    : !(p.disable_project_save ?? false);
  return {
    id: p.id,
    sessionId: p.sessionId ?? p.session_id ?? '',
    projectId: p.projectId ?? p.project_id ?? '',
    tool: p.tool,
    args: p.args,
    resolvedArg: p.resolvedArg ?? p.resolved_arg ?? '',
    canSaveProject,
    batchFiles: p.batchFiles ?? p.batch_files,
    batchResolvedFiles: p.batchResolvedFiles ?? p.batch_resolved_files,
  };
}

// A Map keeps insertion order for every key, including all-numeric request ids
// (which a plain object would reorder numerically ahead of the rest).
export function upsertPermission(map, p) {
  const norm = normalizePermission(p);
  if (!norm?.id) return map;
  const next = new Map(map);
  next.set(norm.id, norm);
  return next;
}

export function removePermission(map, id) {
  if (!id || !map.has(id)) return map;
  const next = new Map(map);
  next.delete(id);
  return next;
}

export function seedPermissions(list) {
  const map = new Map();
  for (const p of list || []) {
    const norm = normalizePermission(p);
    if (norm?.id) map.set(norm.id, norm);
  }
  return map;
}

// permissionList returns the pending requests in insertion order (the head is the
// one PermissionPrompt shows).
export function permissionList(map) {
  return map ? [...map.values()] : [];
}
